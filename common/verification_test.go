package common

import (
	"testing"
	"time"
)

func TestRegisterVerificationCodeWithKeyMinutesUsesCustomTTL(t *testing.T) {
	key := "13800138000"
	code := "123456"
	RegisterVerificationCodeWithKeyMinutes(key, code, PhoneVerificationPurpose, 1)

	if !VerifyCodeWithKey(key, code, PhoneVerificationPurpose) {
		t.Fatal("expected code to be valid before ttl expires")
	}

	verificationMutex.Lock()
	verificationMap[PhoneVerificationPurpose+key] = verificationValue{
		code:         code,
		time:         time.Now().Add(-2 * time.Minute),
		validMinutes: 1,
	}
	verificationMutex.Unlock()

	if VerifyCodeWithKey(key, code, PhoneVerificationPurpose) {
		t.Fatal("expected code to expire after custom ttl")
	}
}
