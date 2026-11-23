package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// 客户端幂等键（G-07）的服务层：摘要、抢占、回放、释放、过期清理。
//
// 三层分工：
//
//	model.idempotency_keys  唯一索引 + 条件更新，负责「谁持有这个键」
//	service（本文件）       幂等语义：摘要一致才回放、不一致 409、失败释放
//	middleware              读 Idempotency-Key 头、缓存响应、按结论写回客户端
//
// 失败请求（5xx/panic）会释放键，让客户端能用同一个键重试；4xx 视为确定性
// 结果，按首次响应回放（同一个错误请求再发一次也不会变成成功）。

// 幂等冲突原因：都在 middleware 里映射成 409 + 不同的 code，便于客户端区分
// 「我复用了键」和「上一笔还在跑」。
const (
	// IdempotencyReasonDigestMismatch 同一个键第一次是 A 请求，第二次是 B 请求。
	IdempotencyReasonDigestMismatch = "digest_mismatch"
	// IdempotencyReasonInProgress 同一个键的第一次请求还在处理中。
	IdempotencyReasonInProgress = "in_progress"
	// IdempotencyReasonNotReplayable 首次响应超出回放上限（只存了状态没存 body）。
	IdempotencyReasonNotReplayable = "not_replayable"
)

// IdempotencyConflictError 表示这个幂等键不能按「回放」处理。
type IdempotencyConflictError struct {
	Reason string
	// FirstRequestId 是占用这个键的那次请求的 request id，便于两侧对账。
	FirstRequestId string
	// FirstDigest 是第一次请求的内容摘要，只在摘要不一致时给出。
	FirstDigest string
}

func (e *IdempotencyConflictError) Error() string {
	switch e.Reason {
	case IdempotencyReasonDigestMismatch:
		return fmt.Sprintf("Idempotency-Key was already used with a different request payload (first request %s)", e.displayRequest())
	case IdempotencyReasonInProgress:
		return fmt.Sprintf("a request with this Idempotency-Key is still in progress (request %s)", e.displayRequest())
	case IdempotencyReasonNotReplayable:
		return fmt.Sprintf("the first response for this Idempotency-Key is too large to replay (request %s)", e.displayRequest())
	default:
		return fmt.Sprintf("Idempotency-Key conflict: %s", e.Reason)
	}
}

func (e *IdempotencyConflictError) displayRequest() string {
	if e.FirstRequestId == "" {
		return "unknown"
	}
	return e.FirstRequestId
}

// IdempotencyScope 是一次带幂等键请求的幂等域。
//
// 幂等域按「用户 + 令牌 + 键」隔离：换成另一把令牌后同一个键是另一笔业务，
// 互不影响（多把令牌往往就是多个客户端）。
type IdempotencyScope struct {
	UserId  int
	TokenId int
	Key     string
	Route   string
	Digest  string
	// RequestId 记到占位行上，409 时客户端能对上「是哪个请求占着这个键」。
	RequestId string
}

// IdempotencyReplay 是可直接写回客户端的首次响应。
type IdempotencyReplay struct {
	Status      int
	ContentType string
	Body        []byte
	RequestId   string
}

// IdempotencyOutcome 是 Begin 的结论：要么持有键继续处理，要么回放首次响应。
type IdempotencyOutcome struct {
	// Record 非空表示本次请求持有这个键，处理完必须 Complete 或 Abort。
	Record *model.IdempotencyKey
	// Replay 非空表示这是重复请求，直接回放它，不能再执行业务逻辑（不再扣费）。
	Replay *IdempotencyReplay
}

// IdempotencyRequestDigest 计算请求内容摘要（方法 + 路径 + 原始 body）。
//
// 路径也进摘要的原因：同一个键被用到另一个接口上本来就是「复用到不同请求」，
// 应该 409 而不是回放一个语义完全不同的响应。
func IdempotencyRequestDigest(method string, path string, body common.BodyStorage) (string, error) {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(method))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(path))
	_, _ = hasher.Write([]byte{0})
	if body != nil {
		reader, err := body.NewReader()
		if err != nil {
			return "", err
		}
		defer reader.Close()
		if _, err := io.Copy(hasher, reader); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// BeginIdempotentRequest 判定这个幂等键该怎么走：
//   - 首次见到：登记 pending 行并返回 Record，调用方正常处理；
//   - 已完成且内容一致：返回 Replay，调用方直接回放，不再执行、不再扣费；
//   - 内容不一致 / 上一笔还在跑 / 首次响应不可回放：返回 *IdempotencyConflictError。
//
// 并发安全依赖 model 层的唯一索引 + 条件更新；这里的重试循环只负责把
// 「刚好撞上别人插入/接管」的瞬时状态重新读一遍。
func BeginIdempotentRequest(scope IdempotencyScope) (*IdempotencyOutcome, error) {
	setting := operation_setting.GetIdempotencySetting()
	lease := int64(setting.EffectivePendingLeaseSeconds())

	for attempt := 0; attempt < 3; attempt++ {
		now := common.GetTimestamp()
		existing, err := model.GetIdempotencyKey(scope.UserId, scope.TokenId, scope.Key)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			row, owned, err := model.CreatePendingIdempotencyKey(&model.IdempotencyKey{
				UserId:        scope.UserId,
				TokenId:       scope.TokenId,
				Key:           scope.Key,
				Route:         scope.Route,
				RequestDigest: scope.Digest,
				Status:        model.IdempotencyStatusPending,
				RequestId:     scope.RequestId,
				ExpiresAt:     now + lease,
			})
			if err != nil {
				return nil, err
			}
			if owned {
				return &IdempotencyOutcome{Record: row}, nil
			}
			// 竞争失败：键在查与插之间被别人拿走了，按已有行重新判定。
			existing = row
		}

		if existing.ExpiresAt <= now {
			// 租约/保留期已过：接管（条件更新保证只有一个请求接管成功）。
			taken, err := model.TakeOverExpiredIdempotencyKey(existing.Id, now, scope.Route, scope.Digest, now+lease)
			if err != nil {
				return nil, err
			}
			if taken {
				row, err := model.GetIdempotencyKeyById(existing.Id)
				if err != nil {
					return nil, err
				}
				if row == nil {
					return nil, errors.New("idempotency key disappeared after takeover")
				}
				return &IdempotencyOutcome{Record: row}, nil
			}
			continue
		}

		if existing.RequestDigest != scope.Digest {
			return nil, &IdempotencyConflictError{
				Reason:         IdempotencyReasonDigestMismatch,
				FirstRequestId: existing.RequestId,
				FirstDigest:    existing.RequestDigest,
			}
		}
		if existing.Status == model.IdempotencyStatusCompleted {
			if !existing.Replayable {
				return nil, &IdempotencyConflictError{
					Reason:         IdempotencyReasonNotReplayable,
					FirstRequestId: existing.RequestId,
				}
			}
			return &IdempotencyOutcome{Replay: &IdempotencyReplay{
				Status:      existing.ResponseStatus,
				ContentType: existing.ResponseType,
				Body:        []byte(existing.ResponseBody),
				RequestId:   existing.RequestId,
			}}, nil
		}
		return nil, &IdempotencyConflictError{
			Reason:         IdempotencyReasonInProgress,
			FirstRequestId: existing.RequestId,
		}
	}
	return nil, errors.New("idempotency key contention: could not claim the key after retries")
}

// CompleteIdempotentRequest 回填首次响应，之后同键同内容请求会回放它。
//
// 回填失败不阻断响应（客户端已经拿到结果），但要留日志：键没登记成功意味着
// 下一次同键请求会重新执行一次业务——这是需要人工关注的重复计费风险。
func CompleteIdempotentRequest(record *model.IdempotencyKey, status int, contentType string, body []byte, replayable bool) {
	if record == nil {
		return
	}
	ttl := int64(operation_setting.GetIdempotencySetting().EffectiveTTLSeconds())
	expiresAt := common.GetTimestamp() + ttl
	if err := model.CompleteIdempotencyKey(record.Id, status, contentType, string(body), len(body), replayable, expiresAt); err != nil {
		common.SysError(common.LogText(
			"failed to complete idempotency key %s (id=%d, status=%d): %s",
			record.Key, record.Id, status, err.Error()))
	}
}

// AbortIdempotentRequest 释放键：只删「仍由本次请求持有」的 pending 行。
//
// 带 digest 条件的删除很关键：如果本次请求的长耗时超过了 pending 租约、键已被
// 下一个请求接管，这里就不能把别人的登记删掉。
func AbortIdempotentRequest(record *model.IdempotencyKey, reason string) {
	if record == nil {
		return
	}
	if err := model.ReleasePendingIdempotencyKey(record.Id, record.RequestDigest); err != nil {
		common.SysError(common.LogText(
			"failed to release idempotency key %s (id=%d): %s", record.Key, record.Id, err.Error()))
	}
	if reason != "" {
		common.SysLog(common.LogText("idempotency key %s released: %s", record.Key, reason))
	}
}

// ---------------------------------------------------------------------------
// 过期清理：键必须有保留期，不能无限增长
// ---------------------------------------------------------------------------

const idempotencyCleanupBatchSize = 500

var idempotencyCleanupOnce sync.Once

// StartIdempotencyKeyCleanup 启动过期键清理（仅主节点，避免多实例重复删）。
//
// 清理间隔从配置里动态读取：运维调小间隔后不需要重启进程，下一轮就生效。
func StartIdempotencyKeyCleanup() {
	idempotencyCleanupOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		go func() {
			RunIdempotencyKeyCleanupOnce()
			for {
				interval := time.Duration(operation_setting.GetIdempotencySetting().EffectiveCleanupIntervalSeconds()) * time.Second
				time.Sleep(interval)
				RunIdempotencyKeyCleanupOnce()
			}
		}()
	})
}

// RunIdempotencyKeyCleanupOnce 删一轮过期键，返回删除行数（供测试与运维手动触发）。
func RunIdempotencyKeyCleanupOnce() int {
	now := common.GetTimestamp()
	deleted, err := model.PurgeExpiredIdempotencyKeys(now, idempotencyCleanupBatchSize)
	if err != nil {
		common.SysError(common.LogText("failed to purge expired idempotency keys: %s", err.Error()))
		return deleted
	}
	if deleted > 0 {
		common.SysLog(common.LogText("purged %d expired idempotency keys", deleted))
	}
	return deleted
}
