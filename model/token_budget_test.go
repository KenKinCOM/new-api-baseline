package model

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// G-06：令牌周期预算的判定、回写与周期切换。
func createBudgetTestToken(t *testing.T, quota int, period string, used int, windowStart int64) Token {
	t.Helper()
	token := Token{
		UserId:            1,
		Key:               "budget-token-" + common.GetRandomString(8),
		Name:              "budget-test",
		Status:            common.TokenStatusEnabled,
		ExpiredTime:       -1,
		RemainQuota:       100000,
		BudgetQuota:       quota,
		BudgetPeriod:      period,
		BudgetUsed:        used,
		BudgetWindowStart: windowStart,
	}
	require.NoError(t, token.Insert())
	return token
}

func currentWindowStart(t *testing.T, period string) int64 {
	t.Helper()
	start, _, ok := TokenBudgetWindow(period, common.GetTimestamp())
	require.True(t, ok)
	return start
}

func TestTokenBudgetWithoutConfigurationAlwaysAllows(t *testing.T) {
	truncateTables(t)
	token := createBudgetTestToken(t, 0, "", 0, 0)

	allowed, state, err := TryReserveTokenBudget(token.Id, 500)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.False(t, state.Enabled())

	_, err = AddTokenBudgetUsage(token.Id, 500)
	require.NoError(t, err)

	stored := getTokenFromDB(t, token.Id)
	assert.Equal(t, 0, stored.BudgetUsed, "未配置预算的令牌不应产生已用量")
	assert.Equal(t, int64(0), stored.BudgetWindowStart)
}

func TestTokenBudgetRejectsOverLimitAndKeepsUsed(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodDaily
	token := createBudgetTestToken(t, 100, period, 0, currentWindowStart(t, period))

	allowed, state, err := TryReserveTokenBudget(token.Id, 60)
	require.NoError(t, err)
	require.True(t, allowed)
	assert.Equal(t, 60, state.Used)

	allowed, state, err = TryReserveTokenBudget(token.Id, 50)
	require.NoError(t, err)
	assert.False(t, allowed, "已用 60 + 预估 50 超过上限 100，必须拒绝")
	assert.Equal(t, 100, state.Quota)
	assert.Equal(t, 60, state.Used)
	assert.Equal(t, 40, state.Remaining())
	assert.True(t, state.End > state.Start)

	stored := getTokenFromDB(t, token.Id)
	assert.Equal(t, 60, stored.BudgetUsed, "被拒绝的请求不得写入已用量")

	// 刚好用满仍然允许
	allowed, _, err = TryReserveTokenBudget(token.Id, 40)
	require.NoError(t, err)
	assert.True(t, allowed)
	allowed, _, err = TryReserveTokenBudget(token.Id, 1)
	require.NoError(t, err)
	assert.False(t, allowed, "额度用满后必须拒绝")
}

func TestTokenBudgetSettleAdjustsToActualUsage(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodMonthly
	token := createBudgetTestToken(t, 100, period, 0, currentWindowStart(t, period))

	_, _, err := TryReserveTokenBudget(token.Id, 30)
	require.NoError(t, err)

	// 结算实际消耗 45：差额 +15 回写
	_, err = AddTokenBudgetUsage(token.Id, 15)
	require.NoError(t, err)
	assert.Equal(t, 45, getTokenFromDB(t, token.Id).BudgetUsed)

	// 结算实际消耗 10：差额 -35 回写
	state, err := AddTokenBudgetUsage(token.Id, -35)
	require.NoError(t, err)
	assert.Equal(t, 10, getTokenFromDB(t, token.Id).BudgetUsed)
	assert.Equal(t, 10, state.Used)

	// 负数回写不会让已用量变负
	_, err = AddTokenBudgetUsage(token.Id, -500)
	require.NoError(t, err)
	assert.Equal(t, 0, getTokenFromDB(t, token.Id).BudgetUsed)
}

func TestTokenBudgetAllowsOverageOnSettlement(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodDaily
	token := createBudgetTestToken(t, 100, period, 90, currentWindowStart(t, period))

	// 请求已放行后实际用量超过上限：允许回写（服务已经发生），但记账必须真实。
	_, err := AddTokenBudgetUsage(token.Id, 25)
	require.NoError(t, err)
	assert.Equal(t, 115, getTokenFromDB(t, token.Id).BudgetUsed)

	allowed, state, err := TryReserveTokenBudget(token.Id, 1)
	require.NoError(t, err)
	assert.False(t, allowed, "超支后新的请求必须被拒绝")
	assert.Equal(t, 0, state.Remaining())
}

func TestTokenBudgetRolloverResetsUsageDaily(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodDaily
	yesterday := time.Now().AddDate(0, 0, -1)
	staleStart := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.Local).Unix()
	token := createBudgetTestToken(t, 100, period, 100, staleStart)

	state, err := GetTokenBudgetState(token.Id)
	require.NoError(t, err)
	assert.Equal(t, 0, state.Used, "跨天后已用量应视为归零")

	allowed, state, err := TryReserveTokenBudget(token.Id, 70)
	require.NoError(t, err)
	require.True(t, allowed)
	assert.Equal(t, 70, state.Used)

	stored := getTokenFromDB(t, token.Id)
	assert.Equal(t, 70, stored.BudgetUsed)
	assert.Equal(t, currentWindowStart(t, period), stored.BudgetWindowStart, "周期起点应推进到当天 0 点")
}

func TestTokenBudgetRolloverResetsUsageMonthly(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodMonthly
	lastMonth := time.Now().AddDate(0, -1, 0)
	staleStart := time.Date(lastMonth.Year(), lastMonth.Month(), 1, 0, 0, 0, 0, time.Local).Unix()
	token := createBudgetTestToken(t, 500, period, 500, staleStart)

	allowed, state, err := TryReserveTokenBudget(token.Id, 200)
	require.NoError(t, err)
	require.True(t, allowed)
	assert.Equal(t, 200, state.Used)
	assert.Equal(t, currentWindowStart(t, period), getTokenFromDB(t, token.Id).BudgetWindowStart)
}

func TestTokenBudgetInvalidPeriodIsTreatedAsDisabled(t *testing.T) {
	truncateTables(t)
	token := createBudgetTestToken(t, 10, "weekly", 0, 0)

	allowed, state, err := TryReserveTokenBudget(token.Id, 1000)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.False(t, state.Enabled(), "未知周期类型不参与判定（由管理端校验拦截）")
}

func TestTokenBudgetConcurrentReserveNeverExceedsLimit(t *testing.T) {
	truncateTables(t)
	period := TokenBudgetPeriodDaily
	token := createBudgetTestToken(t, 100, period, 0, currentWindowStart(t, period))

	var wg sync.WaitGroup
	results := make([]bool, 10)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			allowed, _, err := TryReserveTokenBudget(token.Id, 20)
			if err != nil {
				t.Errorf("reserve %d failed: %v", idx, err)
				return
			}
			results[idx] = allowed
		}(i)
	}
	wg.Wait()

	allowedCount := 0
	for _, allowed := range results {
		if allowed {
			allowedCount++
		}
	}
	assert.Equal(t, 5, allowedCount, "上限 100 / 每次 20 只能放行 5 笔")
	assert.Equal(t, 100, getTokenFromDB(t, token.Id).BudgetUsed)
}

func TestNormalizeTokenBudgetPeriod(t *testing.T) {
	for _, period := range []string{"", TokenBudgetPeriodDaily, TokenBudgetPeriodMonthly} {
		normalized, ok := NormalizeTokenBudgetPeriod(period)
		assert.True(t, ok, period)
		assert.Equal(t, period, normalized)
	}
	for _, period := range []string{"weekly", "DAILY", "yearly"} {
		_, ok := NormalizeTokenBudgetPeriod(period)
		assert.False(t, ok, period)
	}
}

func intPtr(v int) *int { return &v }

func strPtr(v string) *string { return &v }

func TestApplyTokenBudgetChangeValidation(t *testing.T) {
	now := common.GetTimestamp()
	dailyStart, _, ok := TokenBudgetWindow(TokenBudgetPeriodDaily, now)
	require.True(t, ok)

	t.Run("negative quota rejected", func(t *testing.T) {
		token := Token{}
		err := ApplyTokenBudgetChange(&token, intPtr(-1), strPtr("daily"), nil, 0, now)
		assert.ErrorIs(t, err, ErrTokenBudgetQuotaNegative)
	})

	t.Run("missing period rejected", func(t *testing.T) {
		token := Token{}
		err := ApplyTokenBudgetChange(&token, intPtr(100), nil, nil, 0, now)
		assert.ErrorIs(t, err, ErrTokenBudgetPeriodInvalid)
	})

	t.Run("unknown period rejected", func(t *testing.T) {
		token := Token{}
		err := ApplyTokenBudgetChange(&token, intPtr(100), strPtr("weekly"), nil, 0, now)
		assert.ErrorIs(t, err, ErrTokenBudgetPeriodInvalid)
	})

	t.Run("quota over maximum rejected", func(t *testing.T) {
		token := Token{}
		err := ApplyTokenBudgetChange(&token, intPtr(1000), strPtr("daily"), nil, 500, now)
		assert.ErrorIs(t, err, ErrTokenBudgetQuotaTooLarge)
	})

	t.Run("new budget starts a fresh window", func(t *testing.T) {
		token := Token{BudgetUsed: 42}
		require.NoError(t, ApplyTokenBudgetChange(&token, intPtr(100), strPtr("daily"), nil, 0, now))
		assert.Equal(t, 100, token.BudgetQuota)
		assert.Equal(t, TokenBudgetPeriodDaily, token.BudgetPeriod)
		assert.Equal(t, dailyStart, token.BudgetWindowStart)
		assert.Equal(t, 0, token.BudgetUsed)
	})

	t.Run("unchanged budget keeps usage", func(t *testing.T) {
		previous := Token{BudgetQuota: 100, BudgetPeriod: TokenBudgetPeriodDaily, BudgetWindowStart: dailyStart, BudgetUsed: 66}
		token := Token{}
		require.NoError(t, ApplyTokenBudgetChange(&token, intPtr(100), strPtr("daily"), &previous, 0, now))
		assert.Equal(t, 66, token.BudgetUsed, "只改名字不应清零预算账")
		assert.Equal(t, dailyStart, token.BudgetWindowStart)
	})

	t.Run("changing quota resets usage", func(t *testing.T) {
		previous := Token{BudgetQuota: 100, BudgetPeriod: TokenBudgetPeriodDaily, BudgetWindowStart: dailyStart, BudgetUsed: 66}
		token := Token{}
		require.NoError(t, ApplyTokenBudgetChange(&token, intPtr(200), nil, &previous, 0, now))
		assert.Equal(t, 200, token.BudgetQuota)
		assert.Equal(t, 0, token.BudgetUsed)
	})

	t.Run("omitting budget fields preserves previous", func(t *testing.T) {
		previous := Token{BudgetQuota: 100, BudgetPeriod: TokenBudgetPeriodDaily, BudgetWindowStart: dailyStart, BudgetUsed: 66}
		token := Token{}
		require.NoError(t, ApplyTokenBudgetChange(&token, nil, nil, &previous, 0, now))
		assert.Equal(t, 100, token.BudgetQuota)
		assert.Equal(t, 66, token.BudgetUsed)
	})

	t.Run("zero quota disables and clears", func(t *testing.T) {
		previous := Token{BudgetQuota: 100, BudgetPeriod: TokenBudgetPeriodDaily, BudgetWindowStart: dailyStart, BudgetUsed: 66}
		token := Token{}
		require.NoError(t, ApplyTokenBudgetChange(&token, intPtr(0), nil, &previous, 0, now))
		assert.Equal(t, 0, token.BudgetQuota)
		assert.Equal(t, "", token.BudgetPeriod)
		assert.Equal(t, 0, token.BudgetUsed)
		assert.Equal(t, int64(0), token.BudgetWindowStart)
	})
}
