package service

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertBudgetToken(t *testing.T, quota int, period string) model.Token {
	t.Helper()
	start, _, ok := model.TokenBudgetWindow(period, common.GetTimestamp())
	if period != "" {
		require.True(t, ok)
	}
	token := model.Token{
		UserId:            1,
		Key:               "svc-budget-" + common.GetRandomString(8),
		Name:              "svc-budget-test",
		Status:            common.TokenStatusEnabled,
		ExpiredTime:       -1,
		RemainQuota:       1_000_000,
		BudgetQuota:       quota,
		BudgetPeriod:      period,
		BudgetWindowStart: start,
	}
	require.NoError(t, token.Insert())
	return token
}

func newBudgetTestSession(token model.Token) *BillingSession {
	return &BillingSession{
		relayInfo: &relaycommon.RelayInfo{TokenId: token.Id, TokenKey: token.Key},
		lang:      "zh-CN",
	}
}

// G-06：预扣前的预算判定 —— 超限返回 402 + token_budget_exhausted，且响应里
// 明确区分「预算到顶」与「余额不足」。
func TestBillingSessionBudgetRejectsAndReportsReason(t *testing.T) {
	token := insertBudgetToken(t, 100, model.TokenBudgetPeriodDaily)
	session := newBudgetTestSession(token)

	require.Nil(t, session.reserveTokenBudget(80))
	assert.Equal(t, 80, session.budgetReserved)
	assert.True(t, session.budgetEnabled)

	apiErr := session.reserveTokenBudget(30)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeTokenBudgetExhausted, apiErr.GetErrorCode())
	assert.Equal(t, http.StatusPaymentRequired, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "预算")

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(apiErr.Metadata, &metadata))
	assert.Equal(t, "token_budget_exhausted", metadata["reason"])
	assert.EqualValues(t, 100, metadata["limit"])
	assert.EqualValues(t, 80, metadata["used"])
	assert.EqualValues(t, 20, metadata["remaining"])
	assert.EqualValues(t, 30, metadata["required"])
	assert.Equal(t, model.TokenBudgetPeriodDaily, metadata["period"])
	assert.Greater(t, metadata["reset_at"].(float64), float64(time.Now().Unix()))

	// 被拒绝的请求不能占用已用量
	stored, err := model.GetTokenBudgetState(token.Id)
	require.NoError(t, err)
	assert.Equal(t, 80, stored.Used)
}

// G-06：结算差额回写与失败退款退回占用。
func TestBillingSessionBudgetSettleAndRelease(t *testing.T) {
	token := insertBudgetToken(t, 200, model.TokenBudgetPeriodDaily)
	session := newBudgetTestSession(token)

	require.Nil(t, session.reserveTokenBudget(120))
	require.NoError(t, session.settleTokenBudget(50)) // 实际比预估少
	assert.Equal(t, 50, session.budgetReserved)

	stored, err := model.GetTokenBudgetState(token.Id)
	require.NoError(t, err)
	assert.Equal(t, 50, stored.Used)

	session.releaseTokenBudget()
	stored, err = model.GetTokenBudgetState(token.Id)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.Used, "退款必须把本周期已用量退回")
}

// G-06：未配置预算的令牌完全不受影响（也不产生额外写库）。
func TestBillingSessionWithoutBudgetIsNoop(t *testing.T) {
	token := insertBudgetToken(t, 0, "")
	session := newBudgetTestSession(token)

	require.Nil(t, session.reserveTokenBudget(999))
	assert.False(t, session.budgetEnabled)
	require.NoError(t, session.settleTokenBudget(999))

	stored, err := model.GetTokenBudgetState(token.Id)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.Used)
	assert.Equal(t, int64(0), stored.StoredAt)
}
