package controller

import (
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// webhookRecorder 把一次支付回调的原始证据落库（R4），并在处理结束时回填结果。
//
// 用法：
//
//	rec := newWebhookRecorder(c, model.PaymentProviderEpay, body, signature)
//	defer rec.finish()
//	rec.markVerified(true)
//	rec.setTradeNo(tradeNo)
//	rec.setOutcome(model.WebhookOutcomeCredited, "")
type webhookRecorder struct {
	event   *model.WebhookEvent
	tradeNo string
}

func newWebhookRecorder(c *gin.Context, provider string, body []byte, signature string) *webhookRecorder {
	event := model.RecordWebhookEvent(&model.WebhookEvent{
		Provider:   provider,
		HttpMethod: c.Request.Method,
		RemoteIp:   c.ClientIP(),
		Signature:  signature,
		Body:       string(body),
	})
	return &webhookRecorder{event: event}
}

func (r *webhookRecorder) markVerified(verified bool) {
	if r != nil && r.event != nil {
		r.event.Verified = verified
	}
}

func (r *webhookRecorder) setTradeNo(tradeNo string) {
	if r != nil && tradeNo != "" {
		r.tradeNo = tradeNo
	}
}

// finish 回填验签结果、处理结果与单号。outcome 为空时按 ignored 落库。
func (r *webhookRecorder) finish(outcome string, message string) {
	if r == nil || r.event == nil {
		return
	}
	if outcome == "" {
		outcome = model.WebhookOutcomeIgnored
	}
	model.FinishWebhookEvent(r.event, r.event.Verified, outcome, r.tradeNo, message)
}
