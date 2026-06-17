package system_setting

import "testing"

func TestSMSSettingsDefaults(t *testing.T) {
	settings := DefaultSMSSettings()

	if !settings.Enabled {
		t.Fatalf("expected SMS login to be enabled by default for local development")
	}
	if settings.Provider != "local" {
		t.Fatalf("expected local provider, got %q", settings.Provider)
	}
	if settings.CodeValidMinutes != 10 {
		t.Fatalf("expected 10 minute code validity, got %d", settings.CodeValidMinutes)
	}
	if settings.ResendIntervalSeconds != 60 {
		t.Fatalf("expected 60 second resend interval, got %d", settings.ResendIntervalSeconds)
	}
	if settings.PhoneRateLimitCount != 5 {
		t.Fatalf("expected phone rate limit count 5, got %d", settings.PhoneRateLimitCount)
	}
	if settings.PhoneRateLimitWindowSeconds != 3600 {
		t.Fatalf("expected phone rate limit window 3600 seconds, got %d", settings.PhoneRateLimitWindowSeconds)
	}
	if settings.IPRateLimitCount != 20 {
		t.Fatalf("expected IP rate limit count 20, got %d", settings.IPRateLimitCount)
	}
	if settings.IPRateLimitWindowSeconds != 3600 {
		t.Fatalf("expected IP rate limit window 3600 seconds, got %d", settings.IPRateLimitWindowSeconds)
	}
	if !settings.LocalLogEnabled {
		t.Fatalf("expected local log provider enabled")
	}
}

func TestGetSMSSettingsReturnsSingleton(t *testing.T) {
	if GetSMSSettings() != &smsSettings {
		t.Fatalf("expected GetSMSSettings to return registered singleton")
	}
}
