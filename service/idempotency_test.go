package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

func setupIdempotencyServiceTest(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.IdempotencyKey{}))
	model.DB.Exec("DELETE FROM idempotency_keys")
	t.Cleanup(func() {
		model.DB.Exec("DELETE FROM idempotency_keys")
	})
}

func mustBegin(t *testing.T, scope IdempotencyScope) *IdempotencyOutcome {
	t.Helper()
	outcome, err := BeginIdempotentRequest(scope)
	require.NoError(t, err)
	require.NotNil(t, outcome)
	return outcome
}

func TestBeginIdempotentRequestFirstCallOwnsKey(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 1, TokenId: 2, Key: "k-first", Route: "POST /v1/chat/completions", Digest: "digest-1", RequestId: "req-1"}
	outcome := mustBegin(t, scope)
	require.Nil(t, outcome.Replay)
	require.NotNil(t, outcome.Record)
	assert.Equal(t, model.IdempotencyStatusPending, outcome.Record.Status)
	assert.Equal(t, "req-1", outcome.Record.RequestId)

	CompleteIdempotentRequest(outcome.Record, 200, "application/json", []byte(`{"answer":42}`), true)

	// 同键同内容再来：直接回放首次结果。
	replay := mustBegin(t, scope)
	require.NotNil(t, replay.Replay)
	assert.Nil(t, replay.Record, "replay must not re-own the key")
	assert.Equal(t, 200, replay.Replay.Status)
	assert.Equal(t, `{"answer":42}`, string(replay.Replay.Body))
	assert.Equal(t, "application/json", replay.Replay.ContentType)
	assert.Equal(t, "req-1", replay.Replay.RequestId)
}

func TestBeginIdempotentRequestRejectsDifferentDigest(t *testing.T) {
	setupIdempotencyServiceTest(t)

	first := IdempotencyScope{UserId: 1, TokenId: 2, Key: "k-reuse", Route: "POST /v1/chat/completions", Digest: "digest-a", RequestId: "req-a"}
	outcome := mustBegin(t, first)
	CompleteIdempotentRequest(outcome.Record, 200, "application/json", []byte(`{"answer":"a"}`), true)

	second := IdempotencyScope{UserId: 1, TokenId: 2, Key: "k-reuse", Route: "POST /v1/chat/completions", Digest: "digest-b", RequestId: "req-b"}
	_, err := BeginIdempotentRequest(second)
	require.Error(t, err)
	var conflict *IdempotencyConflictError
	require.True(t, errors.As(err, &conflict))
	assert.Equal(t, IdempotencyReasonDigestMismatch, conflict.Reason)
	assert.Equal(t, "digest-a", conflict.FirstDigest)
	assert.Equal(t, "req-a", conflict.FirstRequestId)
	// 关键口径：内容不一致时绝不能返回第一次的结果。
	assert.Contains(t, err.Error(), "different request payload")
}

func TestBeginIdempotentRequestBlocksConcurrentDuplicate(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 3, TokenId: 4, Key: "k-inflight", Route: "POST /v1/embeddings", Digest: "digest", RequestId: "req-inflight"}
	outcome := mustBegin(t, scope)
	require.NotNil(t, outcome.Record)

	_, err := BeginIdempotentRequest(scope)
	require.Error(t, err)
	var conflict *IdempotencyConflictError
	require.True(t, errors.As(err, &conflict))
	assert.Equal(t, IdempotencyReasonInProgress, conflict.Reason)
}

func TestAbortIdempotentRequestAllowsRetry(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 5, TokenId: 6, Key: "k-abort", Route: "POST /v1/chat/completions", Digest: "digest", RequestId: "req-fail"}
	outcome := mustBegin(t, scope)
	require.NotNil(t, outcome.Record)

	AbortIdempotentRequest(outcome.Record, "upstream failure")

	// 释放后同键重试必须能重新持有（客户端重试不应该被自己的失败挡住）。
	retry := mustBegin(t, scope)
	require.NotNil(t, retry.Record)
	assert.Equal(t, model.IdempotencyStatusPending, retry.Record.Status)
}

func TestBeginIdempotentRequestTakesOverExpiredLease(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 7, TokenId: 8, Key: "k-crash", Route: "POST /v1/chat/completions", Digest: "digest", RequestId: "req-crashed"}
	outcome := mustBegin(t, scope)
	require.NotNil(t, outcome.Record)

	// 模拟进程崩溃：占位行留在库里且租约已过期。
	require.NoError(t, model.DB.Model(&model.IdempotencyKey{}).
		Where("id = ?", outcome.Record.Id).
		Update("expires_at", common.GetTimestamp()-1).Error)

	retry := mustBegin(t, scope)
	require.NotNil(t, retry.Record, "expired lease must be takeable again")
	assert.Equal(t, outcome.Record.Id, retry.Record.Id)

	// 租约过期后换成不同内容：也允许接管（旧响应已经过期作废）。
	other := IdempotencyScope{UserId: 7, TokenId: 8, Key: "k-crash", Route: "POST /v1/chat/completions", Digest: "digest-other", RequestId: "req-other"}
	require.NoError(t, model.DB.Model(&model.IdempotencyKey{}).
		Where("id = ?", retry.Record.Id).
		Update("expires_at", common.GetTimestamp()-1).Error)
	taken := mustBegin(t, other)
	require.NotNil(t, taken.Record)
}

func TestBeginIdempotentRequestNotReplayableReturnsConflict(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 9, TokenId: 10, Key: "k-big", Route: "POST /v1/chat/completions", Digest: "digest", RequestId: "req-big"}
	outcome := mustBegin(t, scope)
	// 首次响应超过回放上限：只记状态，不存 body。
	CompleteIdempotentRequest(outcome.Record, 200, "text/event-stream", nil, false)

	_, err := BeginIdempotentRequest(scope)
	require.Error(t, err)
	var conflict *IdempotencyConflictError
	require.True(t, errors.As(err, &conflict))
	assert.Equal(t, IdempotencyReasonNotReplayable, conflict.Reason)
}

func TestRunIdempotencyKeyCleanupOnceRemovesExpiredKeys(t *testing.T) {
	setupIdempotencyServiceTest(t)

	scope := IdempotencyScope{UserId: 11, TokenId: 12, Key: "k-cleanup", Route: "POST /v1/chat/completions", Digest: "digest", RequestId: "req-cleanup"}
	outcome := mustBegin(t, scope)
	require.NotNil(t, outcome.Record)
	require.NoError(t, model.DB.Model(&model.IdempotencyKey{}).
		Where("id = ?", outcome.Record.Id).
		Update("expires_at", common.GetTimestamp()-1).Error)

	assert.Equal(t, 1, RunIdempotencyKeyCleanupOnce())

	count, err := model.CountIdempotencyKeys()
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

func TestIdempotencyRequestDigestIsStableButContentSensitive(t *testing.T) {
	setupIdempotencyServiceTest(t)

	bodyA, err := common.CreateBodyStorage([]byte(`{"model":"gpt-4o","messages":[]}`))
	require.NoError(t, err)
	bodyB, err := common.CreateBodyStorage([]byte(`{"model":"gpt-4o","messages":[]}`))
	require.NoError(t, err)
	bodyC, err := common.CreateBodyStorage([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)

	digestA, err := IdempotencyRequestDigest("POST", "/v1/chat/completions", bodyA)
	require.NoError(t, err)
	digestB, err := IdempotencyRequestDigest("POST", "/v1/chat/completions", bodyB)
	require.NoError(t, err)
	digestC, err := IdempotencyRequestDigest("POST", "/v1/chat/completions", bodyC)
	require.NoError(t, err)
	otherPath, err := IdempotencyRequestDigest("POST", "/v1/embeddings", bodyA)
	require.NoError(t, err)

	assert.Equal(t, digestA, digestB, "同一内容必须得到同一摘要")
	assert.NotEqual(t, digestA, digestC, "内容不同摘要必须不同")
	assert.NotEqual(t, digestA, otherPath, "路径不同摘要必须不同")
	assert.Len(t, digestA, model.IdempotencyDigestLength)
}
