package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStatusDoesNotExposeAlipaySecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/status", GetStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	body := res.Body.String()
	if containsAny(body, []string{"alipay_private_key", "alipay_public_key", "sms_secret"}) {
		t.Fatalf("status leaked secret fields: %s", body)
	}
	if !containsAll(body, []string{"sms_enabled", "sms_provider", "alipay_oauth", "alipay_app_id"}) {
		t.Fatalf("status did not expose expected sms/alipay fields: %s", body)
	}
}

func containsAny(body string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(body, needle) {
			return true
		}
	}
	return false
}

func containsAll(body string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(body, needle) {
			return false
		}
	}
	return true
}
