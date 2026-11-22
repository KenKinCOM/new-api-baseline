package model

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// G-06 token 周期预算：令牌在「日 / 月」周期内的额度上限。
//
// 预算记在令牌上（Token.BudgetQuota / BudgetPeriod / BudgetWindowStart /
// BudgetUsed），与「令牌一次性额度 remain_quota」相互独立：
//   - remain_quota 是令牌的总额度，用完即止，充值才能恢复；
//   - budget_quota 是本周期内的消费上限，周期切换后已用量归零，额度不消耗。
//
// 预算判定与已用量回写都走数据库条件更新（单条 UPDATE 内完成「读-判-加」），
// 因此多副本、Redis 缓存过期、批量写模式下都不会超卖：判定永远以 DB 为准，
// 缓存里的 Token 只用于展示。

const (
	TokenBudgetPeriodDaily   = "daily"
	TokenBudgetPeriodMonthly = "monthly"
)

// NormalizeTokenBudgetPeriod 校验并归一化周期类型；空串表示未启用预算。
func NormalizeTokenBudgetPeriod(period string) (string, bool) {
	switch period {
	case "":
		return "", true
	case TokenBudgetPeriodDaily, TokenBudgetPeriodMonthly:
		return period, true
	default:
		return "", false
	}
}

// TokenBudgetWindow 返回 now 所在周期的 [start, end) 时间戳（秒）。
// 周期按服务器本地时区（TZ）的自然日 / 自然月切分。
func TokenBudgetWindow(period string, now int64) (start int64, end int64, ok bool) {
	t := time.Unix(now, 0).In(time.Local)
	switch period {
	case TokenBudgetPeriodDaily:
		begin := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		return begin.Unix(), begin.AddDate(0, 0, 1).Unix(), true
	case TokenBudgetPeriodMonthly:
		begin := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)
		return begin.Unix(), begin.AddDate(0, 1, 0).Unix(), true
	default:
		return 0, 0, false
	}
}

// TokenBudgetState 是某个令牌在「当前周期」的预算快照。
type TokenBudgetState struct {
	Quota    int    // 周期额度上限，<=0 表示未配置预算
	Period   string // daily / monthly
	Used     int    // 本周期已用量（已按周期切换归零）
	Start    int64  // 当前周期起点
	End      int64  // 下一个周期起点（即预算重置时间）
	StoredAt int64  // 数据库里记录的周期起点（用于判断是否需要切换）
}

func (s TokenBudgetState) Enabled() bool {
	if s.Quota <= 0 {
		return false
	}
	// 未知周期类型（历史脏数据 / 手工改库）不参与判定，由管理端校验兜住配置入口。
	return s.Period == TokenBudgetPeriodDaily || s.Period == TokenBudgetPeriodMonthly
}

func (s TokenBudgetState) Remaining() int {
	remaining := s.Quota - s.Used
	if remaining < 0 {
		return 0
	}
	return remaining
}

// GetTokenBudgetState 读取令牌预算的权威快照。周期已切换时返回归零后的已用量，
// 但不会写库（写库发生在下一条请求预扣 / 结算时）。
func GetTokenBudgetState(id int) (*TokenBudgetState, error) {
	var token Token
	if err := DB.Select("id", "budget_quota", "budget_period", "budget_window_start", "budget_used").
		First(&token, id).Error; err != nil {
		return nil, err
	}
	state := &TokenBudgetState{
		Quota:    token.BudgetQuota,
		Period:   token.BudgetPeriod,
		Used:     token.BudgetUsed,
		StoredAt: token.BudgetWindowStart,
	}
	if !state.Enabled() {
		return state, nil
	}
	start, end, ok := TokenBudgetWindow(state.Period, common.GetTimestamp())
	if !ok {
		return state, nil
	}
	state.Start, state.End = start, end
	if state.StoredAt != start {
		state.Used = 0
	}
	return state, nil
}

// rolloverTokenBudget 把周期起点推进到 start 并清零已用量。
// 条件更新保证并发请求里只有一个真正执行切换，其余请求读到的就是新周期。
func rolloverTokenBudget(id int, storedAt int64, start int64) error {
	return DB.Model(&Token{}).
		Where("id = ? AND budget_window_start = ?", id, storedAt).
		Updates(map[string]any{
			"budget_window_start": start,
			"budget_used":         0,
		}).Error
}

// TryReserveTokenBudget 在预扣费前判定并占用本次预估额度。
//
// 返回 allowed=false 时 state 是拒绝时刻的预算快照（used/limit/end 用于构造 402）。
// 未配置预算的令牌直接放行，且不产生任何写库。已配置预算时用单条条件 UPDATE
// 完成「已用量 + 本次预估 <= 上限」的判定与累加，超限时不写入任何已用量。
func TryReserveTokenBudget(id int, estimate int) (bool, *TokenBudgetState, error) {
	if id <= 0 {
		return true, nil, nil
	}
	if estimate < 0 {
		estimate = 0
	}
	state, err := GetTokenBudgetState(id)
	if err != nil {
		return false, nil, err
	}
	if !state.Enabled() {
		return true, state, nil
	}
	if state.StoredAt != state.Start {
		if err := rolloverTokenBudget(id, state.StoredAt, state.Start); err != nil {
			return false, nil, err
		}
	}
	if estimate == 0 {
		return true, state, nil
	}

	// 两轮：第一轮可能因为并发请求刚切换了周期而失败，重读后再判定一次。
	for attempt := 0; attempt < 2; attempt++ {
		result := DB.Model(&Token{}).
			Where("id = ? AND budget_quota > 0 AND budget_window_start = ? AND budget_used + ? <= budget_quota",
				id, state.Start, estimate).
			Update("budget_used", gorm.Expr("budget_used + ?", estimate))
		if result.Error != nil {
			return false, nil, result.Error
		}
		if result.RowsAffected == 1 {
			state.Used += estimate
			return true, state, nil
		}
		refreshed, err := GetTokenBudgetState(id)
		if err != nil {
			return false, nil, err
		}
		if !refreshed.Enabled() {
			// 预算在并发过程中被关闭，按未配置处理。
			return true, refreshed, nil
		}
		state = refreshed
		if state.StoredAt != state.Start {
			continue // 周期被并发切换，按新周期重试一次
		}
		if state.Used+estimate > state.Quota {
			return false, state, nil
		}
		// 行存在但条件不成立且已用量允许：说明令牌被并发删除或窗口再次变化，
		// 交给下一轮重试；两轮都失败按拒绝处理，避免绕过上限。
	}
	return false, state, nil
}

// AddTokenBudgetUsage 回写本周期已用量（结算差额、退款、以及无预扣路径的直接计费）。
// delta 可为负（退款），已用量按 0 截断，不会出现负的已用量。
// 未配置预算的令牌是空操作，返回 nil 快照。
func AddTokenBudgetUsage(id int, delta int) (*TokenBudgetState, error) {
	if id <= 0 || delta == 0 {
		return nil, nil
	}
	state, err := GetTokenBudgetState(id)
	if err != nil {
		return nil, err
	}
	if !state.Enabled() {
		return state, nil
	}
	if state.StoredAt != state.Start {
		if err := rolloverTokenBudget(id, state.StoredAt, state.Start); err != nil {
			return nil, err
		}
		state.Used = 0
	}
	// 统一的 CASE 表达式保证 SQLite / MySQL / PostgreSQL 下都能把已用量截断在 0。
	result := DB.Model(&Token{}).
		Where("id = ? AND budget_window_start = ?", id, state.Start).
		Update("budget_used", gorm.Expr(
			"CASE WHEN budget_used + ? < 0 THEN 0 ELSE budget_used + ? END", delta, delta))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		// 周期被并发切换：已用量已在新周期归零，本次差额不再回写（旧周期的账已结束）。
		refreshed, refreshErr := GetTokenBudgetState(id)
		if refreshErr != nil {
			return nil, refreshErr
		}
		return refreshed, nil
	}
	state.Used += delta
	if state.Used < 0 {
		state.Used = 0
	}
	return state, nil
}

// ResetTokenBudgetWindow 把令牌预算窗口重置到 now 所在周期（配置变更时使用）。
func ResetTokenBudgetWindow(token *Token, now int64) {
	if token == nil || token.BudgetQuota <= 0 || token.BudgetPeriod == "" {
		token.BudgetWindowStart = 0
		token.BudgetUsed = 0
		return
	}
	start, _, ok := TokenBudgetWindow(token.BudgetPeriod, now)
	if !ok {
		token.BudgetWindowStart = 0
		token.BudgetUsed = 0
		return
	}
	token.BudgetWindowStart = start
	token.BudgetUsed = 0
}
