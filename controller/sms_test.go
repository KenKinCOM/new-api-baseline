package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	smssvc "github.com/QuantumNous/new-api/service/sms"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func resetSMSVerificationTestState(t *testing.T) {
	t.Helper()
	originalTurnstileEnabled := common.TurnstileCheckEnabled
	originalSMSSettings := *system_setting.GetSMSSettings()
	common.TurnstileCheckEnabled = false
	*system_setting.GetSMSSettings() = system_setting.DefaultSMSSettings()
	smsResendLimiter = smssvc.NewMemoryLimiter()
	smsPhoneLimiter = smssvc.NewMemoryLimiter()
	smsIPLimiter = smssvc.NewMemoryLimiter()

	t.Cleanup(func() {
		common.TurnstileCheckEnabled = originalTurnstileEnabled
		*system_setting.GetSMSSettings() = originalSMSSettings
		smsResendLimiter = smssvc.NewMemoryLimiter()
		smsPhoneLimiter = smssvc.NewMemoryLimiter()
		smsIPLimiter = smssvc.NewMemoryLimiter()
	})
}

func TestSendSMSVerificationRegistersPhoneCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetSMSVerificationTestState(t)

	router := gin.New()
	router.POST("/api/sms/verification", SendSMSVerification)

	body := bytes.NewBufferString(`{"phone":"13800138000"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sms/verification", body)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	_ = system_setting.GetSMSSettings()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}
	if !bytes.Contains(res.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("expected success response, got %s", res.Body.String())
	}
}

func TestSendSMSVerificationRejectsResendInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetSMSVerificationTestState(t)

	router := gin.New()
	router.POST("/api/sms/verification", SendSMSVerification)

	for i := 0; i < 2; i++ {
		body := bytes.NewBufferString(`{"phone":"13800138001"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/sms/verification", body)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)

		if i == 0 && res.Code != http.StatusOK {
			t.Fatalf("expected first request to pass, got %d: %s", res.Code, res.Body.String())
		}
		if i == 1 && !bytes.Contains(res.Body.Bytes(), []byte(`"success":false`)) {
			t.Fatalf("expected resend interval rejection, got %s", res.Body.String())
		}
	}
}
