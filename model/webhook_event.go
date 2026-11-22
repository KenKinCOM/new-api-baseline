package model

import "github.com/QuantumNous/new-api/common"

// WebhookEvent 留存每一次支付回调的原始证据（R4）。
//
// 此前验签结果、原始 body、来源 IP 只进滚动日志文件：日志一滚，争议时无法还原
// "同一笔支付被回调了几次、每次验签是否通过、body 长什么样"。本表把回调当事件
// 落一行，先落证再处理，处理结果回填到同一行。
//
// 与 payment_trade_no_claims 的分工：claims 管"这笔支付归谁"的互斥判定，
// 本表管"回调发生过什么"的举证。
type WebhookEvent struct {
	Id         int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	Provider   string `json:"provider" gorm:"type:varchar(50);not null;index:idx_webhook_event_provider_created,priority:1"`
	EventType  string `json:"event_type" gorm:"type:varchar(100);default:''"`
	TradeNo    string `json:"trade_no" gorm:"type:varchar(255);default:'';index"`
	HttpMethod string `json:"http_method" gorm:"type:varchar(16);default:''"`
	RemoteIp   string `json:"remote_ip" gorm:"type:varchar(64);default:'';index"`
	Signature  string `json:"signature" gorm:"type:text"`
	Body       string `json:"body" gorm:"type:longtext"`
	Verified   bool   `json:"verified"`
	Outcome    string `json:"outcome" gorm:"type:varchar(32);default:'';index"`
	Message    string `json:"message" gorm:"type:text"`
	CreatedAt  int64  `json:"created_at" gorm:"bigint;not null;index:idx_webhook_event_provider_created,priority:2"`
}

func (WebhookEvent) TableName() string {
	return "webhook_events"
}

// 回调处理结果口径，固定枚举，便于按结果筛选举证。
const (
	WebhookOutcomePending   = "pending"
	WebhookOutcomeCredited  = "credited"
	WebhookOutcomeDuplicate = "duplicate"
	WebhookOutcomeRejected  = "rejected"
	WebhookOutcomeIgnored   = "ignored"
	WebhookOutcomeError     = "error"
)

// RecordWebhookEvent 先落一行回调证据，返回带主键的记录供后续回填结果。
// 落库失败不阻断业务处理（举证缺失不应导致用户钱到不了账），只记错误日志。
func RecordWebhookEvent(event *WebhookEvent) *WebhookEvent {
	if event == nil {
		return nil
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = common.GetTimestamp()
	}
	if event.Outcome == "" {
		event.Outcome = WebhookOutcomePending
	}
	if err := DB.Create(event).Error; err != nil {
		common.SysError(common.LogText("failed to record webhook event provider=%s trade_no=%s: %s", event.Provider, event.TradeNo, err.Error()))
		return nil
	}
	return event
}

// FinishWebhookEvent 回填处理结果。event 为 nil（落库失败）时安全跳过。
func FinishWebhookEvent(event *WebhookEvent, verified bool, outcome string, tradeNo string, message string) {
	if event == nil {
		return
	}
	updates := map[string]any{
		"verified": verified,
		"outcome":  outcome,
		"message":  message,
	}
	if tradeNo != "" {
		updates["trade_no"] = tradeNo
	}
	if err := DB.Model(&WebhookEvent{}).Where("id = ?", event.Id).Updates(updates).Error; err != nil {
		common.SysError(common.LogText("failed to finish webhook event id=%d: %s", event.Id, err.Error()))
	}
}
