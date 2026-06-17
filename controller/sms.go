package controller

import (
	"encoding/json"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	smssvc "github.com/QuantumNous/new-api/service/sms"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

var (
	smsPhoneLimiter = smssvc.NewMemoryLimiter()
	smsIPLimiter    = smssvc.NewMemoryLimiter()
)

type SMSVerificationRequest struct {
	Phone string `json:"phone"`
}

func SendSMSVerification(c *gin.Context) {
	settings := system_setting.GetSMSSettings()
	if settings == nil || !settings.Enabled {
		common.ApiErrorMsg(c, "SMS login is disabled")
		return
	}

	var req SMSVerificationRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}

	phone, err := smssvc.NormalizePhone(req.Phone)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	if !smsPhoneLimiter.Allow(phone, settings.PhoneRateLimitCount, settings.PhoneRateLimitWindowSeconds) {
		common.ApiErrorMsg(c, "too many SMS requests for this phone")
		return
	}
	if !smsIPLimiter.Allow(c.ClientIP(), settings.IPRateLimitCount, settings.IPRateLimitWindowSeconds) {
		common.ApiErrorMsg(c, "too many SMS requests from this IP")
		return
	}

	provider, err := smssvc.GetProvider(settings)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	code := common.GenerateVerificationCode(6)
	if err := provider.SendVerificationCode(c.Request.Context(), phone, code); err != nil {
		common.ApiError(c, err)
		return
	}

	common.RegisterVerificationCodeWithKeyMinutes(phone, code, common.PhoneVerificationPurpose, settings.CodeValidMinutes)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"phone": smssvc.MaskPhone(phone),
		},
	})
}
