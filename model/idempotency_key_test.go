package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/QuantumNous/new-api/common"
)

func clearIdempotencyKeys(t *testing.T) {
	t.Helper()
	DB.Exec("DELETE FROM idempotency_keys")
	t.Cleanup(func() {
		DB.Exec("DELETE FROM idempotency_keys")
	})
}

func TestCreatePendingIdempotencyKeyKeepsFirstOwner(t *testing.T) {
	clearIdempotencyKeys(t)
	now := common.GetTimestamp()

	first := &IdempotencyKey{
		UserId:        1,
		TokenId:       2,
		Key:           "key-first-owner",
		Route:         "POST /v1/chat/completions",
		RequestDigest: "digest-a",
		ExpiresAt:     now + 60,
	}
	created, owned, err := CreatePendingIdempotencyKey(first)
	require.NoError(t, err)
	require.True(t, owned, "first insert must own the key")
	require.NotZero(t, created.Id)

	second := &IdempotencyKey{
		UserId:        1,
		TokenId:       2,
		Key:           "key-first-owner",
		Route:         "POST /v1/chat/completions",
		RequestDigest: "digest-b",
		ExpiresAt:     now + 60,
	}
	existing, owned, err := CreatePendingIdempotencyKey(second)
	require.NoError(t, err)
	assert.False(t, owned, "second insert must not own the key")
	require.NotNil(t, existing)
	assert.Equal(t, created.Id, existing.Id)
	// 落库内容仍是第一个请求的摘要：后来的请求不能覆盖先到者的内容。
	assert.Equal(t, "digest-a", existing.RequestDigest)
	assert.Equal(t, IdempotencyStatusPending, existing.Status)
}

func TestIdempotencyKeyScopeIsPerToken(t *testing.T) {
	clearIdempotencyKeys(t)
	now := common.GetTimestamp()

	_, owned, err := CreatePendingIdempotencyKey(&IdempotencyKey{
		UserId: 1, TokenId: 10, Key: "shared-uuid", RequestDigest: "d", ExpiresAt: now + 60,
	})
	require.NoError(t, err)
	require.True(t, owned)

	// 同一个用户、另一个令牌用同名键：不同幂等域，必须能各自持有。
	_, owned, err = CreatePendingIdempotencyKey(&IdempotencyKey{
		UserId: 1, TokenId: 11, Key: "shared-uuid", RequestDigest: "d", ExpiresAt: now + 60,
	})
	require.NoError(t, err)
	assert.True(t, owned)
}

func TestTakeOverExpiredIdempotencyKeyOnlyOnce(t *testing.T) {
	clearIdempotencyKeys(t)
	now := common.GetTimestamp()

	row, owned, err := CreatePendingIdempotencyKey(&IdempotencyKey{
		UserId: 3, TokenId: 4, Key: "stale-lease", RequestDigest: "old", ExpiresAt: now - 5,
	})
	require.NoError(t, err)
	require.True(t, owned)

	taken, err := TakeOverExpiredIdempotencyKey(row.Id, now, "POST /v1/embeddings", "new-digest", now+60)
	require.NoError(t, err)
	require.True(t, taken)

	// 再抢一次必须落空：expires_at 已经不是过期值了。
	taken, err = TakeOverExpiredIdempotencyKey(row.Id, now, "POST /v1/embeddings", "other-digest", now+60)
	require.NoError(t, err)
	assert.False(t, taken)

	stored, err := GetIdempotencyKeyById(row.Id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "new-digest", stored.RequestDigest)
	assert.Equal(t, "POST /v1/embeddings", stored.Route)
}

func TestCompleteIdempotencyKeyStoresResponseOnce(t *testing.T) {
	clearIdempotencyKeys(t)
	now := common.GetTimestamp()

	row, owned, err := CreatePendingIdempotencyKey(&IdempotencyKey{
		UserId: 5, TokenId: 6, Key: "complete-me", RequestDigest: "d", ExpiresAt: now + 60,
	})
	require.NoError(t, err)
	require.True(t, owned)

	require.NoError(t, CompleteIdempotencyKey(row.Id, 200, "application/json", `{"id":"x"}`, 11, true, now+3600))

	stored, err := GetIdempotencyKeyById(row.Id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, IdempotencyStatusCompleted, stored.Status)
	assert.Equal(t, 200, stored.ResponseStatus)
	assert.Equal(t, `{"id":"x"}`, string(stored.ResponseBody))
	assert.True(t, stored.Replayable)

	// 已完成的行使不能再次回填（状态已不是 pending）。
	assert.ErrorIs(t, CompleteIdempotencyKey(row.Id, 500, "text/plain", "late", 4, false, now+3600), gorm.ErrRecordNotFound)
}

func TestPurgeExpiredIdempotencyKeysDeletesOnlyExpired(t *testing.T) {
	clearIdempotencyKeys(t)
	now := common.GetTimestamp()

	for _, key := range []string{"expired-1", "expired-2", "alive"} {
		expiresAt := now + 3600
		if key != "alive" {
			expiresAt = now - 1
		}
		_, owned, err := CreatePendingIdempotencyKey(&IdempotencyKey{
			UserId: 7, TokenId: 8, Key: key, RequestDigest: "d", ExpiresAt: expiresAt,
		})
		require.NoError(t, err)
		require.True(t, owned)
	}

	// batch=1 验证分批删除不会漏删。
	deleted, err := PurgeExpiredIdempotencyKeys(now, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)

	count, err := CountIdempotencyKeys()
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	alive, err := GetIdempotencyKey(7, 8, "alive")
	require.NoError(t, err)
	assert.NotNil(t, alive)
}
