package operation_setting

import (
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/setting/config"
)

// IdempotencySetting 客户端幂等键配置（G-07）。
//
// 只在请求带 Idempotency-Key 头时生效：没有这个头的请求完全走原路径，不受
// 这些参数影响。所以默认开启是安全的——开关的意义是「出问题时能一键停用」，
// 而不是「需要手工打开旧客户端才能用」。
type IdempotencySetting struct {
	// Enabled=false 时忽略 Idempotency-Key 头（恢复「每个请求都执行」的旧行为）。
	Enabled bool `json:"enabled"`
	// TTLSeconds 是键的保留期：从键登记到最后一次可回放的时间。
	// 太短会在客户端重试窗口外放行重复扣费，太长会让表变大，默认 24 小时。
	TTLSeconds int `json:"ttl_seconds"`
	// PendingLeaseSeconds 是 pending 行的租约：进程崩溃后，同一幂等键在这个
	// 时间之后允许被下一次请求接管（否则崩溃的那次请求会把键锁死到 TTL）。
	PendingLeaseSeconds int `json:"pending_lease_seconds"`
	// MaxReplayBytes 是可回放响应体的上限。超过上限的响应仍然会正常返回给
	// 第一次调用者，但不会存下来；用同一个键重放时返回 409 说明首次响应超限，
	// 不会用一个被截断的 body 冒充原响应。
	MaxReplayBytes int `json:"max_replay_bytes"`
	// CleanupIntervalSeconds 是后台清理过期键的间隔。
	CleanupIntervalSeconds int `json:"cleanup_interval_seconds"`
}

const (
	idempotencyDefaultTTLSeconds      = 24 * 60 * 60
	idempotencyDefaultPendingLease    = 60
	idempotencyDefaultMaxReplayBytes  = 1 << 20 // 1 MiB
	idempotencyDefaultCleanupInterval = 10 * 60
	idempotencyMaxTTLSeconds          = 7 * 24 * 60 * 60
	idempotencyMaxPendingLeaseSeconds = 15 * 60
	idempotencyMaxReplayBytesSetting  = 16 << 20 // 16 MiB
	idempotencyMinCleanupIntervalSec  = 60
	idempotencyMaxCleanupIntervalSec  = 24 * 60 * 60
	idempotencySettingName            = "idempotency_setting"
	idempotencyEnabledOptionKey       = "idempotency_setting.enabled"
	idempotencyTTLOptionKey           = "idempotency_setting.ttl_seconds"
	idempotencyPendingOptionKey       = "idempotency_setting.pending_lease_seconds"
	idempotencyMaxReplayOptionKey     = "idempotency_setting.max_replay_bytes"
	idempotencyCleanupOptionKey       = "idempotency_setting.cleanup_interval_seconds"
)

var idempotencySetting = IdempotencySetting{
	Enabled:                true,
	TTLSeconds:             idempotencyDefaultTTLSeconds,
	PendingLeaseSeconds:    idempotencyDefaultPendingLease,
	MaxReplayBytes:         idempotencyDefaultMaxReplayBytes,
	CleanupIntervalSeconds: idempotencyDefaultCleanupInterval,
}

func init() {
	config.GlobalConfig.Register(idempotencySettingName, &idempotencySetting)
}

// GetIdempotencySetting 返回当前幂等键配置。
func GetIdempotencySetting() *IdempotencySetting {
	return &idempotencySetting
}

// ValidateIdempotencyOption 在保存配置前校验，避免写出「TTL 为 0」这类会把
// 幂等键退化成一次性的值。
func ValidateIdempotencyOption(key, value string) error {
	switch key {
	case idempotencyTTLOptionKey:
		seconds, err := parsePositiveInt(value)
		if err != nil {
			return err
		}
		if seconds > idempotencyMaxTTLSeconds {
			return fmt.Errorf("%s must not exceed %d seconds", key, idempotencyMaxTTLSeconds)
		}
	case idempotencyPendingOptionKey:
		seconds, err := parsePositiveInt(value)
		if err != nil {
			return err
		}
		if seconds > idempotencyMaxPendingLeaseSeconds {
			return fmt.Errorf("%s must not exceed %d seconds", key, idempotencyMaxPendingLeaseSeconds)
		}
	case idempotencyMaxReplayOptionKey:
		bytesValue, err := parsePositiveInt(value)
		if err != nil {
			return err
		}
		if bytesValue > idempotencyMaxReplayBytesSetting {
			return fmt.Errorf("%s must not exceed %d", key, idempotencyMaxReplayBytesSetting)
		}
	case idempotencyCleanupOptionKey:
		seconds, err := parsePositiveInt(value)
		if err != nil {
			return err
		}
		if seconds < idempotencyMinCleanupIntervalSec || seconds > idempotencyMaxCleanupIntervalSec {
			return fmt.Errorf("%s must be between %d and %d seconds", key, idempotencyMinCleanupIntervalSec, idempotencyMaxCleanupIntervalSec)
		}
	}
	return nil
}

// TTL 等运行时取值出错时回落到默认值：宁可少存一会儿，也不能让「配错了」
// 变成「键永不过期」。
func (s IdempotencySetting) EffectiveTTLSeconds() int {
	if s.TTLSeconds <= 0 {
		return idempotencyDefaultTTLSeconds
	}
	if s.TTLSeconds > idempotencyMaxTTLSeconds {
		return idempotencyMaxTTLSeconds
	}
	return s.TTLSeconds
}

func (s IdempotencySetting) EffectivePendingLeaseSeconds() int {
	if s.PendingLeaseSeconds <= 0 {
		return idempotencyDefaultPendingLease
	}
	if s.PendingLeaseSeconds > idempotencyMaxPendingLeaseSeconds {
		return idempotencyMaxPendingLeaseSeconds
	}
	return s.PendingLeaseSeconds
}

func (s IdempotencySetting) EffectiveMaxReplayBytes() int {
	if s.MaxReplayBytes <= 0 {
		return idempotencyDefaultMaxReplayBytes
	}
	if s.MaxReplayBytes > idempotencyMaxReplayBytesSetting {
		return idempotencyMaxReplayBytesSetting
	}
	return s.MaxReplayBytes
}

func (s IdempotencySetting) EffectiveCleanupIntervalSeconds() int {
	if s.CleanupIntervalSeconds < idempotencyMinCleanupIntervalSec || s.CleanupIntervalSeconds > idempotencyMaxCleanupIntervalSec {
		return idempotencyDefaultCleanupInterval
	}
	return s.CleanupIntervalSeconds
}

// parsePositiveInt 把配置里的整数字符串解析成正整数，非正数一律拒绝。
func parsePositiveInt(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %s", value, err.Error())
	}
	if number <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", value)
	}
	return number, nil
}
