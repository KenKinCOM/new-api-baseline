package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupPhoneLoginRouter(t *testing.T) *gin.Engine {
	t.Helper()

	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalRegisterEnabled := common.RegisterEnabled
	originalQuotaForNewUser := common.QuotaForNewUser
	originalRedisEnabled := common.RedisEnabled
	originalGenerateDefaultToken := constant.GenerateDefaultToken

	gin.SetMode(gin.TestMode)
	common.RegisterEnabled = true
	common.QuotaForNewUser = 0
	common.RedisEnabled = false
	constant.GenerateDefaultToken = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Log{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	model.DB = db
	model.LOG_DB = db

	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.RegisterEnabled = originalRegisterEnabled
		common.QuotaForNewUser = originalQuotaForNewUser
		common.RedisEnabled = originalRedisEnabled
		constant.GenerateDefaultToken = originalGenerateDefaultToken
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("phone-login-test"))))
	router.POST("/api/user/phone/login", PhoneLogin)
	return router
}

func TestPhoneLoginAutoRegistersAndLogsIn(t *testing.T) {
	router := setupPhoneLoginRouter(t)
	phone := "13800138000"
	code := "123456"
	common.RegisterVerificationCodeWithKey(phone, code, common.PhoneVerificationPurpose)

	body := bytes.NewBufferString(`{"phone":"` + phone + `","verification_code":"` + code + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/user/phone/login", body)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !payload.Success || payload.Data.ID == 0 {
		t.Fatalf("expected successful login response, got %s", res.Body.String())
	}

	var user model.User
	if err := model.DB.Where("phone = ?", phone).First(&user).Error; err != nil {
		t.Fatalf("expected user to be created by phone: %v", err)
	}
	if user.Role != common.RoleCommonUser || user.Status != common.UserStatusEnabled {
		t.Fatalf("expected enabled common user, got role=%d status=%d", user.Role, user.Status)
	}
	if common.VerifyCodeWithKey(phone, code, common.PhoneVerificationPurpose) {
		t.Fatal("expected phone verification code to be consumed after login")
	}
}
