package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func TestSendSMSVerificationRegistersPhoneCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	common.TurnstileCheckEnabled = false

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
