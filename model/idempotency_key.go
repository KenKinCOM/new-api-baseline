package model

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/QuantumNous/new-api/common"
)

// 客户端幂等键（G-07）。
//
// 问题：计费接口是「扣钱」的，客户端超时重试、网关重放、SDK 自动重试都会把
// 同一笔业务请求再送一遍。没有服务端幂等键时，每一次重试都是一次新的预扣费，
// 用户就被重复扣款了。
//
// 口径（本仓约定，与主流支付网关一致）：
//   - 同一个幂等键 + 同一个请求内容 → 返回第一次的响应，不再执行、不再扣费；
//   - 同一个幂等键 + 不同请求内容 → 409，说明这个键已经被用在别的请求上了，
//     不返回第一次的结果（否则客户端会拿到别的请求的答案）。
//
// 表结构上的三处关键设计：
//
//	唯一索引 (user_id, token_id, key)：幂等域按「用户 + 令牌」隔离，两个用户
//	恰好用了同一个 UUID 互不影响；也保证「谁先插入谁持有」，不需要先查后插。
//
//	status/expires_at：插入时先落 pending 行占位（租约 60s，进程崩溃后租约
//	自然过期，别人可以接管），处理完回填 completed + 响应；pending 行的租约
//	只覆盖「一次请求的生存期」，不占用完整的键保留期。
//
//	request_digest：请求内容摘要。同一个键再来时先比摘要：一致才允许回放，
//	不一致直接 409，不能把第一次的响应当成第二次的回答。

const (
	// IdempotencyStatusPending 表示键已被某个正在执行的请求占用（租约期）。
	IdempotencyStatusPending = "pending"
	// IdempotencyStatusCompleted 表示请求已结束，响应已回填，可回放。
	IdempotencyStatusCompleted = "completed"

	// IdempotencyKeyMaxLength 是幂等键的最大长度（与唯一索引列宽度一致）。
	IdempotencyKeyMaxLength = 255
	// IdempotencyDigestLength 是 sha256 十六进制摘要的长度。
	IdempotencyDigestLength = 64
)

// IdempotencyKey 是一次带幂等键的请求的登记行。
//
// 响应体只存「可回放的小响应」：超过调用方给定的上限时 replayable 置 false，
// 回放请求会收到 409（明确告知首次响应超限不可回放），而不是拿一个被截断的
// body 冒充原响应。
type IdempotencyKey struct {
	Id        int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"bigint;not null"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint;not null"`
	// ExpiresAt 是这一行可以被接管/清理的时间点：pending 行是租约到期时间，
	// completed 行是键的保留期到期时间。
	ExpiresAt int64 `json:"expires_at" gorm:"bigint;not null;index:idx_idempotency_keys_expires"`
	UserId    int   `json:"user_id" gorm:"not null;uniqueIndex:uk_idempotency_key_scope,priority:1"`
	TokenId   int   `json:"token_id" gorm:"not null;uniqueIndex:uk_idempotency_key_scope,priority:2"`
	// Key 是客户端带来的幂等键，列名不用保留字 key（MySQL 保留字）。
	Key string `json:"key" gorm:"column:idempotency_key;type:varchar(255);not null;uniqueIndex:uk_idempotency_key_scope,priority:3"`
	// Route 只用于排障（同一个键被用到别的路径上时摘要本来就不一致）。
	Route          string   `json:"route" gorm:"type:varchar(255);not null;default:''"`
	RequestDigest  string   `json:"request_digest" gorm:"type:varchar(64);not null;default:''"`
	Status         string   `json:"status" gorm:"type:varchar(16);not null;index:idx_idempotency_keys_status"`
	ResponseStatus int      `json:"response_status" gorm:"not null;default:0"`
	ResponseType   string   `json:"response_content_type" gorm:"type:varchar(128);not null;default:''"`
	ResponseBody   LongText `json:"-"`
	ResponseBytes  int      `json:"response_bytes" gorm:"not null;default:0"`
	Replayable     bool     `json:"replayable" gorm:"not null;default:false"`
	RequestId      string   `json:"request_id" gorm:"type:varchar(128);not null;default:''"`
}

func (IdempotencyKey) TableName() string {
	return "idempotency_keys"
}

// GetIdempotencyKey 按幂等域（用户 + 令牌 + 键）取一行；不存在返回 (nil, nil)，
// 便于调用方把「没有」和「查询失败」分开处理：查询失败不能当成没有（那会放行
// 一次可能重复扣费的请求）。
func GetIdempotencyKey(userId int, tokenId int, key string) (*IdempotencyKey, error) {
	var row IdempotencyKey
	err := DB.Where("user_id = ? AND token_id = ? AND idempotency_key = ?", userId, tokenId, key).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// GetIdempotencyKeyById 按主键取一行。
func GetIdempotencyKeyById(id int64) (*IdempotencyKey, error) {
	var row IdempotencyKey
	err := DB.Where("id = ?", id).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// CreatePendingIdempotencyKey 抢占一个幂等键。
//
// 返回 (row, true, nil) 表示本次调用抢到了键，row 是刚插入的 pending 行；
// 返回 (existing, false, nil) 表示键已存在（别人先抢到了），existing 是已有行。
//
// 冲突判定不解析驱动错误：唯一索引冲突后回查一次，查得到就是「别人先到」，
// 查不到才是真的写失败。这样 MySQL / PostgreSQL / SQLite 三种方言共用一套逻辑。
func CreatePendingIdempotencyKey(row *IdempotencyKey) (*IdempotencyKey, bool, error) {
	if row == nil {
		return nil, false, errors.New("idempotency row is nil")
	}
	now := common.GetTimestamp()
	if row.CreatedAt == 0 {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	if row.Status == "" {
		row.Status = IdempotencyStatusPending
	}
	if err := DB.Create(row).Error; err != nil {
		existing, getErr := GetIdempotencyKey(row.UserId, row.TokenId, row.Key)
		if getErr != nil {
			return nil, false, fmt.Errorf("create idempotency key: %w (lookup after conflict: %v)", err, getErr)
		}
		if existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}
	return row, true, nil
}

// TakeOverExpiredIdempotencyKey 在租约/保留期已过时接管一行。
//
// 条件更新保证「接管」只发生一次：WHERE 里带上原来的过期时间和状态，谁先
// UPDATE 成功谁就持有键，其余并发请求会看到行已被别人改成 pending（新的
// expires_at）而落空。
func TakeOverExpiredIdempotencyKey(id int64, now int64, route string, digest string, expiresAt int64) (bool, error) {
	result := DB.Model(&IdempotencyKey{}).
		Where("id = ? AND expires_at < ?", id, now).
		Updates(map[string]any{
			"status":          IdempotencyStatusPending,
			"route":           route,
			"request_digest":  digest,
			"response_status": 0,
			"response_type":   "",
			"response_body":   "",
			"response_bytes":  0,
			"replayable":      false,
			"expires_at":      expiresAt,
			"updated_at":      now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompleteIdempotencyKey 回填一次成功/可回放的响应。
func CompleteIdempotencyKey(id int64, status int, contentType string, body string, responseBytes int, replayable bool, expiresAt int64) error {
	now := common.GetTimestamp()
	result := DB.Model(&IdempotencyKey{}).
		Where("id = ? AND status = ?", id, IdempotencyStatusPending).
		Updates(map[string]any{
			"status":          IdempotencyStatusCompleted,
			"response_status": status,
			"response_type":   contentType,
			"response_body":   body,
			"response_bytes":  responseBytes,
			"replayable":      replayable,
			"expires_at":      expiresAt,
			"updated_at":      now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// 行已被清理或已被别人接管：不算错误，但要让调用方知道键没登记成功，
		// 这样下一次同键请求不会被误当成「可回放」。
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteIdempotencyKey 删除一行（失败请求释放键，允许客户端重试）。
func DeleteIdempotencyKey(id int64) error {
	return DB.Where("id = ?", id).Delete(&IdempotencyKey{}).Error
}

// ReleasePendingIdempotencyKey 释放一次失败请求持有的键。
//
// 条件里带 status=pending 与 request_digest：如果这一行的租约已经过期并被
// 下一个请求接管（digest 变了），这里就不会误删别人的登记。
func ReleasePendingIdempotencyKey(id int64, digest string) error {
	return DB.Where("id = ? AND status = ? AND request_digest = ?", id, IdempotencyStatusPending, digest).
		Delete(&IdempotencyKey{}).Error
}

// PurgeExpiredIdempotencyKeys 分批删除已过期的行，返回删除总数。
//
// 分批是为了不在一次 DELETE 里锁住大表：每批最多 batch 行，直到没有可删的。
func PurgeExpiredIdempotencyKeys(now int64, batch int) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	total := 0
	for {
		var ids []int64
		if err := DB.Model(&IdempotencyKey{}).
			Where("expires_at < ?", now).
			Order("id").
			Limit(batch).
			Pluck("id", &ids).Error; err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		result := DB.Where("id IN ? AND expires_at < ?", ids, now).Delete(&IdempotencyKey{})
		if result.Error != nil {
			return total, result.Error
		}
		total += int(result.RowsAffected)
		if len(ids) < batch {
			return total, nil
		}
	}
}

// CountIdempotencyKeys 返回表内行数（用于运维观测与容量判断）。
func CountIdempotencyKeys() (int64, error) {
	var count int64
	err := DB.Model(&IdempotencyKey{}).Count(&count).Error
	return count, err
}
