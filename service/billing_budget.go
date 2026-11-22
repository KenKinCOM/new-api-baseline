package service

import (
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// TokenBudgetExhaustedError 表示令牌本周期预算已用完。
//
// 它与额度不足（insufficient_user_quota）是两个不同的事实：账户余额可能充足，
// 只是这个令牌自己设的每日/每月上限到了，而且到点会自动恢复，不需要充值。
// 上层把它映射成 402 + error.code=token_budget_exhausted + metadata 里的
// limit/used/remaining/period/reset_at，客户端据此能区分「充值」和「等重置」。
type TokenBudgetExhaustedError struct {
	Period   string
	Limit    int
	Used     int
	Required int
	ResetAt  int64
	// Message 是面向客户端的本地化文案；为空时 Error() 退回英文描述。
	Message string
}

func (e *TokenBudgetExhaustedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf(
		"token budget exhausted for the current %s period: %d of %d used, this request needs %d, resets at %s",
		e.Period, e.Used, e.Limit, e.Required, budgetResetAtText(e.ResetAt),
	)
}

func budgetResetAtText(resetAt int64) string {
	if resetAt <= 0 {
		return "-"
	}
	return time.Unix(resetAt, 0).In(time.Local).Format("2006-01-02 15:04")
}

func budgetPeriodLabel(lang, period string) string {
	switch period {
	case model.TokenBudgetPeriodDaily:
		return i18n.Translate(lang, i18n.MsgQuotaBudgetPeriodDaily)
	case model.TokenBudgetPeriodMonthly:
		return i18n.Translate(lang, i18n.MsgQuotaBudgetPeriodMonthly)
	default:
		return period
	}
}

// newTokenBudgetExhaustedError 构造「预算到顶」的 402 错误。
func newTokenBudgetExhaustedError(lang string, state *model.TokenBudgetState, required int) *types.NewAPIError {
	if state == nil {
		state = &model.TokenBudgetState{}
	}
	message := i18n.Translate(lang, i18n.MsgQuotaTokenBudgetExhausted, map[string]any{
		"Period":   budgetPeriodLabel(lang, state.Period),
		"Limit":    logger.FormatQuota(state.Quota),
		"Used":     logger.FormatQuota(state.Used),
		"Required": logger.FormatQuota(required),
		"ResetAt":  budgetResetAtText(state.End),
	})
	return types.NewErrorWithStatusCode(
		&TokenBudgetExhaustedError{
			Period:   state.Period,
			Limit:    state.Quota,
			Used:     state.Used,
			Required: required,
			ResetAt:  state.End,
			Message:  message,
		},
		types.ErrorCodeTokenBudgetExhausted,
		http.StatusPaymentRequired,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
		types.ErrOptionWithBudgetInfo(state.Quota, state.Used, required, state.Period, state.End),
	)
}

// reserveTokenBudget 在预扣费前判定并占用本次预估额度。
// 未配置预算的令牌是空操作；判定以数据库为准，与 Redis 缓存、信任旁路都无关。
func (s *BillingSession) reserveTokenBudget(amount int) *types.NewAPIError {
	info := s.relayInfo
	if info == nil || info.IsPlayground || info.TokenId <= 0 || amount <= 0 {
		return nil
	}
	if s.budgetChecked && !s.budgetEnabled {
		return nil
	}
	allowed, state, err := model.TryReserveTokenBudget(info.TokenId, amount)
	s.budgetChecked = true
	if err != nil {
		return types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
	}
	s.budgetEnabled = state != nil && state.Enabled()
	if !allowed {
		return newTokenBudgetExhaustedError(s.lang, state, amount)
	}
	if s.budgetEnabled {
		s.budgetReserved += amount
	}
	return nil
}

// settleTokenBudget 把本周期已用量从「预估值」调整到「实际值」。
// 实际值超过上限时仍然入账（请求已经发生），由调用方告警；
// 之后的请求会在预扣前判定处被拒绝。
func (s *BillingSession) settleTokenBudget(actualQuota int) error {
	info := s.relayInfo
	if info == nil || info.IsPlayground || info.TokenId <= 0 || !s.budgetEnabled {
		return nil
	}
	delta := actualQuota - s.budgetReserved
	if delta == 0 {
		return nil
	}
	state, err := model.AddTokenBudgetUsage(info.TokenId, delta)
	if err != nil {
		return err
	}
	s.budgetReserved = actualQuota
	if state != nil && state.Enabled() && state.Used > state.Quota {
		common.SysLog(common.LogText(
			"ALERT token budget exceeded after settlement: token=%d period=%s used=%d limit=%d request=%s",
			info.TokenId, state.Period, state.Used, state.Quota, info.RequestId))
	}
	return nil
}

// releaseTokenBudget 退还本次请求占用的预算（请求失败退款时调用）。
func (s *BillingSession) releaseTokenBudget() {
	info := s.relayInfo
	if info == nil || info.IsPlayground || info.TokenId <= 0 || !s.budgetEnabled || s.budgetReserved <= 0 {
		return
	}
	released := s.budgetReserved
	s.budgetReserved = 0
	if _, err := model.AddTokenBudgetUsage(info.TokenId, -released); err != nil {
		common.SysLog(common.LogText(
			"error releasing token budget reservation (tokenId=%d, amount=%d): %s",
			info.TokenId, released, err.Error()))
	}
}

// recordTokenBudgetDirectCharge 给「无预扣」的计费路径（实时语音逐条计费、
// 旧差额路径、违规扣费）记预算账：这些路径没有预扣点，无法在事前拦截，
// 因此按实际发生额入账，超支时只告警，由下一次预扣判定拦住后续请求。
func recordTokenBudgetDirectCharge(relayInfo *relaycommon.RelayInfo, charged int) {
	if relayInfo == nil || relayInfo.IsPlayground || relayInfo.TokenId <= 0 || charged <= 0 {
		return
	}
	state, err := model.AddTokenBudgetUsage(relayInfo.TokenId, charged)
	if err != nil {
		common.SysLog(common.LogText("error recording token budget usage (tokenId=%d, amount=%d): %s",
			relayInfo.TokenId, charged, err.Error()))
		return
	}
	if state != nil && state.Enabled() && state.Used > state.Quota {
		common.SysLog(common.LogText(
			"ALERT token budget exceeded on a post-delivery charge: token=%d period=%s used=%d limit=%d request=%s",
			relayInfo.TokenId, state.Period, state.Used, state.Quota, relayInfo.RequestId))
	}
}
