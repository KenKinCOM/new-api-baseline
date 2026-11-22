package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateIdempotencyOptionRejectsNonPositiveTTL(t *testing.T) {
	// TTL 为 0 会让键立刻过期，等于没有幂等；必须在校验阶段拦下。
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.ttl_seconds", "0"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.ttl_seconds", "-1"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.ttl_seconds", "abc"))
	assert.NoError(t, ValidateIdempotencyOption("idempotency_setting.ttl_seconds", "86400"))
}

func TestValidateIdempotencyOptionUpperBounds(t *testing.T) {
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.ttl_seconds", "604801"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.pending_lease_seconds", "901"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.max_replay_bytes", "16777217"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.cleanup_interval_seconds", "30"))
	assert.Error(t, ValidateIdempotencyOption("idempotency_setting.cleanup_interval_seconds", "86401"))
	// 别的键不归本校验管，不能误伤。
	assert.NoError(t, ValidateIdempotencyOption("quota_setting.trust_quota_usd", "10"))
}

func TestIdempotencySettingEffectiveValuesFallBackToDefaults(t *testing.T) {
	var zero IdempotencySetting
	assert.Equal(t, idempotencyDefaultTTLSeconds, zero.EffectiveTTLSeconds())
	assert.Equal(t, idempotencyDefaultPendingLease, zero.EffectivePendingLeaseSeconds())
	assert.Equal(t, idempotencyDefaultMaxReplayBytes, zero.EffectiveMaxReplayBytes())
	assert.Equal(t, idempotencyDefaultCleanupInterval, zero.EffectiveCleanupIntervalSeconds())

	// 超出上限的运行时取值收敛到上限，而不是放任「永不过期」。
	over := IdempotencySetting{
		TTLSeconds:             idempotencyMaxTTLSeconds + 1,
		PendingLeaseSeconds:    idempotencyMaxPendingLeaseSeconds + 1,
		MaxReplayBytes:         idempotencyMaxReplayBytesSetting + 1,
		CleanupIntervalSeconds: 1,
	}
	assert.Equal(t, idempotencyMaxTTLSeconds, over.EffectiveTTLSeconds())
	assert.Equal(t, idempotencyMaxPendingLeaseSeconds, over.EffectivePendingLeaseSeconds())
	assert.Equal(t, idempotencyMaxReplayBytesSetting, over.EffectiveMaxReplayBytes())
	assert.Equal(t, idempotencyDefaultCleanupInterval, over.EffectiveCleanupIntervalSeconds())

	require.NotNil(t, GetIdempotencySetting())
}
