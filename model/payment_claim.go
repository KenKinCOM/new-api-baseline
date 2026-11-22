package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// ErrPaymentTradeNoConflict 表示同一笔通道侧支付已经被另一张本地订单认领过（C1）。
var ErrPaymentTradeNoConflict = errors.New("payment trade no already claimed by another order")

// PaymentTradeNoClaim 是「一笔通道侧支付只能被认领一次」的跨表互斥台账（C1）。
//
// 问题：top_ups.trade_no 与 subscription_orders.trade_no 各自唯一，但互不约束；
// 而 stripe / creem / waffo-pancake 的回调都是「先试订阅、失败再退充值」，
// 同一笔通道支付若同时能对上两边的本地单号，就会被认两次。
//
// 做法：以 (provider, provider_trade_no) 为唯一键，在进入分支前先认领。
// 认领成功才允许继续入账；同一本地单号的重复回调幂等放行；不同本地单号撞同一笔
// 通道支付时直接拒绝，并把冲突落到审计日志。
type PaymentTradeNoClaim struct {
	Id              int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	Provider        string `json:"provider" gorm:"type:varchar(50);not null;uniqueIndex:idx_payment_claim_provider_trade,priority:1"`
	ProviderTradeNo string `json:"provider_trade_no" gorm:"type:varchar(255);not null;uniqueIndex:idx_payment_claim_provider_trade,priority:2"`
	LocalTradeNo    string `json:"local_trade_no" gorm:"type:varchar(255);default:'';index"`
	Kind            string `json:"kind" gorm:"type:varchar(32);default:''"` // auto / subscription / topup
	CreatedAt       int64  `json:"created_at" gorm:"bigint;index"`
}

func (PaymentTradeNoClaim) TableName() string {
	return "payment_trade_no_claims"
}

// ClaimPaymentTradeNo 尝试以 (provider, providerTradeNo) 认领一笔通道侧支付。
// 返回 nil 表示可以继续入账（首次认领或同一本地单号重复回调）；
// 返回 ErrPaymentTradeNoConflict 表示该支付已被另一张订单认领，调用方不得入账。
func ClaimPaymentTradeNo(provider string, providerTradeNo string, localTradeNo string, kind string) error {
	if provider == "" || providerTradeNo == "" {
		// 通道流水号缺失时退化为原有行为：仅靠各自表内的 trade_no 唯一约束。
		return nil
	}
	claim := &PaymentTradeNoClaim{
		Provider:        provider,
		ProviderTradeNo: providerTradeNo,
		LocalTradeNo:    localTradeNo,
		Kind:            kind,
		CreatedAt:       common.GetTimestamp(),
	}
	if err := DB.Create(claim).Error; err == nil {
		return nil
	}

	var existing PaymentTradeNoClaim
	if err := DB.Where("provider = ? AND provider_trade_no = ?", provider, providerTradeNo).First(&existing).Error; err == nil {
		if existing.LocalTradeNo == localTradeNo {
			return nil
		}
		common.SysError(common.LogText("payment trade no conflict provider=%s provider_trade_no=%s existing_local=%s incoming_local=%s", provider, providerTradeNo, existing.LocalTradeNo, localTradeNo))
		return ErrPaymentTradeNoConflict
	}
	// 唯一键冲突之外的错误（连接失败等）交回调用方处理
	return gorm.ErrInvalidDB
}
