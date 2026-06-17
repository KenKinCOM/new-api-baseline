package system_setting

import "github.com/QuantumNous/new-api/setting/config"

type SMSSettings struct {
	Enabled                     bool   `json:"enabled"`
	Provider                    string `json:"provider"`
	CodeValidMinutes            int    `json:"code_valid_minutes"`
	ResendIntervalSeconds       int    `json:"resend_interval_seconds"`
	PhoneRateLimitCount         int    `json:"phone_rate_limit_count"`
	PhoneRateLimitWindowSeconds int    `json:"phone_rate_limit_window_seconds"`
	IPRateLimitCount            int    `json:"ip_rate_limit_count"`
	IPRateLimitWindowSeconds    int    `json:"ip_rate_limit_window_seconds"`
	LocalLogEnabled             bool   `json:"local_log_enabled"`
}

var smsSettings = DefaultSMSSettings()

func DefaultSMSSettings() SMSSettings {
	return SMSSettings{
		Enabled:                     true,
		Provider:                    "local",
		CodeValidMinutes:            10,
		ResendIntervalSeconds:       60,
		PhoneRateLimitCount:         5,
		PhoneRateLimitWindowSeconds: 3600,
		IPRateLimitCount:            20,
		IPRateLimitWindowSeconds:    3600,
		LocalLogEnabled:             true,
	}
}

func GetSMSSettings() *SMSSettings {
	return &smsSettings
}

func init() {
	config.GlobalConfig.Register("sms", &smsSettings)
}
