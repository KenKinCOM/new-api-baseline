package model

import (
	"errors"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// 消费侧三方日终对账（G-12）。
//
// 参与比对的三方：
//
//	日志口径  logs 表消费日志（type=2）减退款日志（type=6）的区间合计
//	用户账    users.used_quota 的窗口增量
//	渠道账    channels.used_quota 的窗口增量
//
// 为什么必须落快照：used_quota 是终身累计标量，没有期初/期末快照就取不到
// 「窗口内增量」；而 logs 会被 log_cleanup 物理删除，累计口径不可比（删掉的
// 量无从得知）。因此对账窗口定义为「上一次快照 → 本次快照」：用户/渠道侧取
// 两次快照的差值，日志侧取同一时间区间的区间合计，两侧覆盖同一段时间。
//
// 记账口径依据（本仓代码）：
//   - service/quota.go、service/text_quota.go、service/task_billing.go、service/violation_fee.go
//     在计费成功时同时调用 UpdateUserUsedQuotaAndRequestCount / UpdateChannelUsedQuota(+quota)
//     并写一条 type=2 的消费日志；
//   - 退款（service/task_billing.go、service/midjourney.go）同时调用
//     UpdateUserUsedQuota / UpdateChannelUsedQuota(-quota) 并写一条 type=6 的退款日志。
//
// 所以窗口内三方应当满足：
//
//	users.used_quota 增量 == channels.used_quota 增量 == (消费日志合计 - 退款日志合计)
//
// 任何一方取不到（首次运行没有基线快照、日志已被清理日切）都不算「一致」。
const (
	BillingReconcileScopeUser    = "user"
	BillingReconcileScopeChannel = "channel"

	// BillingReconcileStatusMatched 仅在三方都取到且完全相等时出现。
	BillingReconcileStatusMatched = "matched"
	// BillingReconcileStatusDiffFound 三方都取到，但存在差值。
	BillingReconcileStatusDiffFound = "diff_found"
	// BillingReconcileStatusBaseline 首次运行，只建立基线快照，不对本期做判定。
	BillingReconcileStatusBaseline = "baseline_only"
	// BillingReconcileStatusUndecidable 有一方未取到（日志被清理、窗口为空等），
	// 不给出「一致」结论。
	BillingReconcileStatusUndecidable = "undecidable"

	BillingReconcileKindLogsVsUsers      = "logs_vs_users"
	BillingReconcileKindLogsVsChannels   = "logs_vs_channels"
	BillingReconcileKindUsersVsChannels  = "users_vs_channels"
	BillingReconcileKindUserDetail       = "user_detail"
	BillingReconcileKindChannelDetail    = "channel_detail"
	BillingReconcileKindSideMissing      = "side_missing"
	billingReconcileScopeDaily           = "daily"
	billingReconcileScopeCumulative      = "cumulative"
	billingReconcileMaxDetailRows        = 200
	billingReconcileMissingBaselineNote  = "首次运行：已建立基线快照，本期不做一致/不一致判定"
	billingReconcileRetentionCutoffGrace = 1
)

// BillingReconcileSnapshot 是一次对账跑批时某一方的累计值快照。
//
// 只存 used_quota <> 0 的实体：期初为 0 的实体由「快照里没有这一行」表达，
// 这样快照体量只跟「有过消费的实体数」相关，而不是全表用户数。每轮的快照
// 既充当本轮期末值，也充当下一轮的期初值，因此每轮只需要保留最近两组。
type BillingReconcileSnapshot struct {
	Id        int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	RunDate   string `json:"run_date" gorm:"type:varchar(10);index:idx_billing_reconcile_snapshot_run,priority:1"`
	Scope     string `json:"scope" gorm:"type:varchar(16);index:idx_billing_reconcile_snapshot_run,priority:2;index:idx_billing_reconcile_snapshot_entity,priority:1"`
	EntityId  int    `json:"entity_id" gorm:"index:idx_billing_reconcile_snapshot_entity,priority:2"`
	UsedQuota int64  `json:"used_quota" gorm:"not null;default:0"`
	TakenAt   int64  `json:"taken_at" gorm:"bigint;index:idx_billing_reconcile_snapshot_run,priority:3"`
}

func (BillingReconcileSnapshot) TableName() string {
	return "billing_reconcile_snapshots"
}

// BillingReconcileDiff 是一条对账差异。无差异时本表不产生任何行。
type BillingReconcileDiff struct {
	Id           int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	RunDate      string `json:"run_date" gorm:"type:varchar(10);index:idx_billing_reconcile_diff_run,priority:1"`
	Kind         string `json:"kind" gorm:"type:varchar(32);index"`
	Scope        string `json:"scope" gorm:"type:varchar(16);index"`
	UserId       int    `json:"user_id" gorm:"default:0;index"`
	ChannelId    int    `json:"channel_id" gorm:"default:0;index"`
	LogQuota     int64  `json:"log_quota" gorm:"not null;default:0"`
	UserQuota    int64  `json:"user_quota" gorm:"not null;default:0"`
	ChannelQuota int64  `json:"channel_quota" gorm:"not null;default:0"`
	DiffQuota    int64  `json:"diff_quota" gorm:"not null;default:0"`
	Message      string `json:"message" gorm:"type:varchar(500);default:''"`
	CreatedAt    int64  `json:"created_at" gorm:"bigint;index:idx_billing_reconcile_diff_run,priority:2"`
}

func (BillingReconcileDiff) TableName() string {
	return "billing_reconcile_diffs"
}

// BillingReconcileSummary 是一轮对账的汇总，写入 system_tasks.result，同时是
// 判读「到底比过没比过」的唯一依据：status=matched 才代表三方取到且相等。
type BillingReconcileSummary struct {
	RunDate                 string   `json:"run_date"`
	Status                  string   `json:"status"`
	WindowStart             int64    `json:"window_start"`
	WindowEnd               int64    `json:"window_end"`
	UserCount               int      `json:"user_count"`
	ChannelCount            int      `json:"channel_count"`
	LogsConsumeQuota        int64    `json:"logs_consume_quota"`
	LogsRefundQuota         int64    `json:"logs_refund_quota"`
	LogsNetQuota            int64    `json:"logs_net_quota"`
	UsersDeltaQuota         int64    `json:"users_delta_quota"`
	ChannelsDeltaQuota      int64    `json:"channels_delta_quota"`
	UsersCumulativeQuota    int64    `json:"users_cumulative_quota"`
	ChannelsCumulativeQuota int64    `json:"channels_cumulative_quota"`
	LogsCumulativeQuota     int64    `json:"logs_cumulative_quota"`
	MissingSides            []string `json:"missing_sides,omitempty"`
	DiffCount               int      `json:"diff_count"`
	DetailTruncated         bool     `json:"detail_truncated,omitempty"`
	Note                    string   `json:"note,omitempty"`
}

type billingReconcileLogRow struct {
	UserId       int
	ChannelId    int
	ConsumeQuota int64
	RefundQuota  int64
}

// RunBillingReconcile 以 windowEnd 为期末执行一次消费侧三方对账。
//
// 期初取上一次快照的 taken_at；没有历史快照时只建立基线，不判定一致性。
// 差异写入 billing_reconcile_diffs，返回的汇总里 status 说明本轮到底比没比过。
func RunBillingReconcile(windowEnd int64) (*BillingReconcileSummary, error) {
	if windowEnd <= 0 {
		return nil, errors.New("billing reconcile window end is required")
	}
	prevAt, err := latestBillingReconcileSnapshotAt()
	if err != nil {
		return nil, err
	}
	summary := &BillingReconcileSummary{WindowStart: prevAt, WindowEnd: windowEnd}
	if prevAt > 0 {
		summary.RunDate = BillingReconcileRunDate(prevAt)
	} else {
		summary.RunDate = BillingReconcileRunDate(windowEnd)
	}

	// 三方累计值：users/channels 直接读表，logs 只能读到还没被清理的部分，
	// 因此日志累计只作为数据点记录，不参与一致判定。
	usersNow, err := collectBillingReconcileUsedQuota(BillingReconcileScopeUser)
	if err != nil {
		return nil, err
	}
	channelsNow, err := collectBillingReconcileUsedQuota(BillingReconcileScopeChannel)
	if err != nil {
		return nil, err
	}
	summary.UserCount = len(usersNow)
	summary.ChannelCount = len(channelsNow)
	summary.UsersCumulativeQuota = sumBillingReconcileSnapshot(usersNow)
	summary.ChannelsCumulativeQuota = sumBillingReconcileSnapshot(channelsNow)
	logsCumulative, _, _, err := sumBillingReconcileLogs(0, windowEnd)
	if err != nil {
		return nil, err
	}
	summary.LogsCumulativeQuota = logsCumulative

	now := common.GetTimestamp()
	diffs := make([]BillingReconcileDiff, 0, 8)
	newDiff := func(kind string, scope string) BillingReconcileDiff {
		return BillingReconcileDiff{RunDate: summary.RunDate, Kind: kind, Scope: scope, CreatedAt: now}
	}

	// 累计口径：users 与 channels 都是终身累计标量，二者应当相等。日志累计
	// 因清理不可比，所以累计口径只做「用户账 vs 渠道账」这一对。
	if summary.UsersCumulativeQuota != summary.ChannelsCumulativeQuota {
		row := newDiff(BillingReconcileKindUsersVsChannels, billingReconcileScopeCumulative)
		row.UserQuota = summary.UsersCumulativeQuota
		row.ChannelQuota = summary.ChannelsCumulativeQuota
		row.DiffQuota = summary.UsersCumulativeQuota - summary.ChannelsCumulativeQuota
		row.Message = "累计口径：users.used_quota 合计与 channels.used_quota 合计不等（实体删除、used_quota 被重置或漏记都会造成该差值）"
		diffs = append(diffs, row)
	}

	if prevAt > 0 && windowEnd <= prevAt {
		// 空窗口：不做任何判定，也不覆盖快照，避免把上一轮的基线冲掉。
		summary.Status = BillingReconcileStatusUndecidable
		summary.MissingSides = []string{"window"}
		summary.Note = "本次窗口为空（window_end 不晚于上一次快照时间），未做比对"
		summary.DiffCount = len(diffs)
		if err := insertBillingReconcileDiffs(diffs); err != nil {
			return summary, err
		}
		return summary, nil
	}

	if prevAt == 0 {
		// 首次运行：三方的「窗口增量」都需要期初快照，此时只有期末值，
		// 不给出任何一致结论，只把基线落下来。
		summary.Status = BillingReconcileStatusBaseline
		summary.MissingSides = []string{BillingReconcileScopeUser, BillingReconcileScopeChannel}
		summary.Note = billingReconcileMissingBaselineNote
		summary.DiffCount = len(diffs)
		if err := insertBillingReconcileDiffs(diffs); err != nil {
			return summary, err
		}
		if err := saveBillingReconcileSnapshot(summary, usersNow, channelsNow); err != nil {
			return summary, err
		}
		return summary, nil
	}

	usersPrev, err := loadBillingReconcileSnapshot(prevAt, BillingReconcileScopeUser)
	if err != nil {
		return nil, err
	}
	channelsPrev, err := loadBillingReconcileSnapshot(prevAt, BillingReconcileScopeChannel)
	if err != nil {
		return nil, err
	}

	logRows, consumeTotal, refundTotal, err := loadBillingReconcileLogs(prevAt, windowEnd)
	if err != nil {
		return nil, err
	}
	summary.LogsConsumeQuota = consumeTotal
	summary.LogsRefundQuota = refundTotal
	summary.LogsNetQuota = consumeTotal - refundTotal

	logsMissing, missingNote, err := billingReconcileLogsUnavailable(prevAt)
	if err != nil {
		return nil, err
	}
	if logsMissing {
		row := newDiff(BillingReconcileKindSideMissing, billingReconcileScopeDaily)
		row.LogQuota = summary.LogsNetQuota
		row.Message = missingNote
		diffs = append(diffs, row)
		summary.MissingSides = append(summary.MissingSides, "logs")
	}

	summary.UsersDeltaQuota = sumBillingReconcileSnapshot(usersNow) - sumBillingReconcileSnapshot(usersPrev)
	summary.ChannelsDeltaQuota = sumBillingReconcileSnapshot(channelsNow) - sumBillingReconcileSnapshot(channelsPrev)

	if !logsMissing {
		if summary.LogsNetQuota != summary.UsersDeltaQuota {
			row := newDiff(BillingReconcileKindLogsVsUsers, billingReconcileScopeDaily)
			row.LogQuota = summary.LogsNetQuota
			row.UserQuota = summary.UsersDeltaQuota
			row.ChannelQuota = summary.ChannelsDeltaQuota
			row.DiffQuota = summary.LogsNetQuota - summary.UsersDeltaQuota
			row.Message = "当日口径：日志净额（消费-退款）与 users.used_quota 增量不等"
			diffs = append(diffs, row)
		}
		if summary.LogsNetQuota != summary.ChannelsDeltaQuota {
			row := newDiff(BillingReconcileKindLogsVsChannels, billingReconcileScopeDaily)
			row.LogQuota = summary.LogsNetQuota
			row.UserQuota = summary.UsersDeltaQuota
			row.ChannelQuota = summary.ChannelsDeltaQuota
			row.DiffQuota = summary.LogsNetQuota - summary.ChannelsDeltaQuota
			row.Message = "当日口径：日志净额（消费-退款）与 channels.used_quota 增量不等"
			diffs = append(diffs, row)
		}

		userDiffs, userTruncated := billingReconcileUserDetails(logRows, usersPrev, usersNow, summary.RunDate, now)
		diffs = append(diffs, userDiffs...)
		channelDiffs, channelTruncated := billingReconcileChannelDetails(logRows, channelsPrev, channelsNow, summary.RunDate, now)
		diffs = append(diffs, channelDiffs...)
		summary.DetailTruncated = userTruncated || channelTruncated
	}

	summary.DiffCount = len(diffs)
	switch {
	case logsMissing:
		summary.Status = BillingReconcileStatusUndecidable
	case summary.DiffCount > 0:
		summary.Status = BillingReconcileStatusDiffFound
	default:
		summary.Status = BillingReconcileStatusMatched
	}

	if err := insertBillingReconcileDiffs(diffs); err != nil {
		return summary, err
	}
	if err := saveBillingReconcileSnapshot(summary, usersNow, channelsNow); err != nil {
		return summary, err
	}
	return summary, nil
}

// BillingReconcileWindowEnd 返回本轮对账的期末时间戳。
//
// 留出一点「静默尾巴」：used_quota 的写入在批量模式下先进内存/Redis 队列，
// 由 InitBatchUpdater 按 BatchUpdateInterval 刷库；日志则是同步写。若窗口
// 期末取到「此刻」，尾巴上的请求会「日志有、used_quota 还没有」，产出假差异。
// 期末所以退后一个刷库周期，下一轮从同一时刻起算，窗口首尾相接不会漏账。
func BillingReconcileWindowEnd(now time.Time) int64 {
	grace := int64(10)
	if common.BatchUpdateEnabled {
		grace = int64(2*common.BatchUpdateInterval) + 30
	}
	return now.Unix() - grace
}

// BillingReconcileRunDate 返回时间戳所属自然日（server 本地时区）。
func BillingReconcileRunDate(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02")
}

// ListBillingReconcileDiffs 按运行日倒序读取差异行，runDate 为空时取最近若干条。
func ListBillingReconcileDiffs(runDate string, limit int) ([]BillingReconcileDiff, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := DB.Model(&BillingReconcileDiff{})
	if runDate != "" {
		query = query.Where("run_date = ?", runDate)
	}
	var rows []BillingReconcileDiff
	err := query.Order("id desc").Limit(limit).Find(&rows).Error
	return rows, err
}

func latestBillingReconcileSnapshotAt() (int64, error) {
	var takenAt int64
	err := DB.Model(&BillingReconcileSnapshot{}).Select("COALESCE(MAX(taken_at), 0)").Scan(&takenAt).Error
	return takenAt, err
}

func loadBillingReconcileSnapshot(takenAt int64, scope string) (map[int]int64, error) {
	values := make(map[int]int64)
	if takenAt <= 0 {
		return values, nil
	}
	var rows []BillingReconcileSnapshot
	if err := DB.Where("taken_at = ? AND scope = ?", takenAt, scope).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		values[row.EntityId] = row.UsedQuota
	}
	return values, nil
}

// collectBillingReconcileUsedQuota 读取某一方当前的 used_quota（只取非零行，
// 零值等价于「快照里没有这一行」）。
func collectBillingReconcileUsedQuota(scope string) (map[int]int64, error) {
	values := make(map[int]int64)
	var rows []struct {
		Id        int
		UsedQuota int64
	}
	var err error
	switch scope {
	case BillingReconcileScopeUser:
		// 软删用户由 GORM 默认作用域排除：注销用户的 used_quota 不再计入累计，
		// 这会同时体现在累计差值与对应实体的明细行里。
		err = DB.Model(&User{}).Select("id, used_quota").Where("used_quota <> 0").Find(&rows).Error
	case BillingReconcileScopeChannel:
		err = DB.Model(&Channel{}).Select("id, used_quota").Where("used_quota <> 0").Find(&rows).Error
	default:
		return nil, errors.New("unknown billing reconcile scope: " + scope)
	}
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		values[row.Id] = row.UsedQuota
	}
	return values, nil
}

func sumBillingReconcileSnapshot(values map[int]int64) int64 {
	var total int64
	for _, value := range values {
		total += value
	}
	return total
}

// loadBillingReconcileLogs 读取窗口内的消费/退款日志，并按用户、渠道聚合。
// 返回按 (user_id, channel_id) 分组的原始行，供明细比对复用。
func loadBillingReconcileLogs(start, end int64) ([]billingReconcileLogRow, int64, int64, error) {
	rows := make([]billingReconcileLogRow, 0, 64)
	query := LOG_DB.Table("logs").
		Select("user_id, channel_id, "+
			"COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS consume_quota, "+
			"COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS refund_quota", LogTypeConsume, LogTypeRefund).
		Where("type IN ?", []int{LogTypeConsume, LogTypeRefund}).
		Where("created_at > ? AND created_at <= ?", start, end).
		Group("user_id, channel_id")
	if err := query.Find(&rows).Error; err != nil {
		return nil, 0, 0, err
	}
	var consumeTotal, refundTotal int64
	for _, row := range rows {
		consumeTotal += row.ConsumeQuota
		refundTotal += row.RefundQuota
	}
	return rows, consumeTotal, refundTotal, nil
}

// sumBillingReconcileLogs 返回给定区间内消费/退款日志的净额（消费-退款）。
func sumBillingReconcileLogs(start, end int64) (int64, int64, int64, error) {
	var totals struct {
		ConsumeQuota int64
		RefundQuota  int64
	}
	query := LOG_DB.Table("logs").
		Select("COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS consume_quota, "+
			"COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS refund_quota", LogTypeConsume, LogTypeRefund).
		Where("type IN ? AND created_at >= ? AND created_at <= ?", []int{LogTypeConsume, LogTypeRefund}, start, end)
	if err := query.Scan(&totals).Error; err != nil {
		return 0, 0, 0, err
	}
	return totals.ConsumeQuota - totals.RefundQuota, totals.ConsumeQuota, totals.RefundQuota, nil
}

// billingReconcileLogsUnavailable 判断窗口内的日志是否已被 log_cleanup 清掉。
//
// logs 表没有「删除水位」列，但清理任务的行里带着 target_timestamp（删除所有
// created_at < target 的日志）。只要最近一次成功的清理水位已经越过窗口期初，
// 窗口头部就有日志缺失，此时任何「日志 vs 账」的结论都不成立。
func billingReconcileLogsUnavailable(windowStart int64) (bool, string, error) {
	var tasks []SystemTask
	err := DB.Where("type = ? AND status = ?", SystemTaskTypeLogCleanup, SystemTaskStatusSucceeded).
		Order("id desc").Limit(1).Find(&tasks).Error
	if err != nil {
		return false, "", err
	}
	if len(tasks) == 0 {
		return false, "", nil
	}
	task := tasks[0]
	var payload struct {
		TargetTimestamp int64 `json:"target_timestamp"`
	}
	if err := task.DecodePayload(&payload); err != nil || payload.TargetTimestamp == 0 {
		// 解析不出水位时不臆测「有缺失」，也不臆测「没问题」：交给调用方按
		// 正常路径比对，并在文档里注明该判定依赖清理任务的 payload 结构。
		return false, "", nil
	}
	if payload.TargetTimestamp >= windowStart+billingReconcileRetentionCutoffGrace {
		return true, "日志侧未取到：窗口期初早于最近一次日志清理水位，窗口内日志已被 log_cleanup 删除，本期不做一致判定", nil
	}
	return false, "", nil
}

func billingReconcileUserDetails(rows []billingReconcileLogRow, prev, now map[int]int64, runDate string, timestamp int64) ([]BillingReconcileDiff, bool) {
	logsByUser := make(map[int]int64, len(rows))
	for _, row := range rows {
		logsByUser[row.UserId] += row.ConsumeQuota - row.RefundQuota
	}
	ids := billingReconcileEntityIDs(prev, now, logsByUser)
	diffs := make([]BillingReconcileDiff, 0, 4)
	truncated := false
	for _, id := range ids {
		delta := now[id] - prev[id]
		logQuota := logsByUser[id]
		if delta == logQuota {
			continue
		}
		if len(diffs) >= billingReconcileMaxDetailRows {
			truncated = true
			break
		}
		diffs = append(diffs, BillingReconcileDiff{
			RunDate:   runDate,
			Kind:      BillingReconcileKindUserDetail,
			Scope:     billingReconcileScopeDaily,
			UserId:    id,
			LogQuota:  logQuota,
			UserQuota: delta,
			DiffQuota: logQuota - delta,
			Message:   "用户维度：该用户窗口内 used_quota 增量与日志净额不等",
			CreatedAt: timestamp,
		})
	}
	return diffs, truncated
}

func billingReconcileChannelDetails(rows []billingReconcileLogRow, prev, now map[int]int64, runDate string, timestamp int64) ([]BillingReconcileDiff, bool) {
	logsByChannel := make(map[int]int64, len(rows))
	for _, row := range rows {
		logsByChannel[row.ChannelId] += row.ConsumeQuota - row.RefundQuota
	}
	ids := billingReconcileEntityIDs(prev, now, logsByChannel)
	diffs := make([]BillingReconcileDiff, 0, 4)
	truncated := false
	for _, id := range ids {
		delta := now[id] - prev[id]
		logQuota := logsByChannel[id]
		if delta == logQuota {
			continue
		}
		if len(diffs) >= billingReconcileMaxDetailRows {
			truncated = true
			break
		}
		diffs = append(diffs, BillingReconcileDiff{
			RunDate:      runDate,
			Kind:         BillingReconcileKindChannelDetail,
			Scope:        billingReconcileScopeDaily,
			ChannelId:    id,
			LogQuota:     logQuota,
			ChannelQuota: delta,
			DiffQuota:    logQuota - delta,
			Message:      "渠道维度：该渠道窗口内 used_quota 增量与日志净额不等",
			CreatedAt:    timestamp,
		})
	}
	return diffs, truncated
}

func billingReconcileEntityIDs(maps ...map[int]int64) []int {
	seen := make(map[int]struct{})
	for _, values := range maps {
		for id := range values {
			seen[id] = struct{}{}
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func insertBillingReconcileDiffs(diffs []BillingReconcileDiff) error {
	if len(diffs) == 0 {
		return nil
	}
	return DB.CreateInBatches(diffs, 100).Error
}

// saveBillingReconcileSnapshot 写入本轮期末快照，并只保留最近两组（本轮与
// 上一轮），避免快照表按天无限增长。
func saveBillingReconcileSnapshot(summary *BillingReconcileSummary, users, channels map[int]int64) error {
	takenAt := summary.WindowEnd
	previousAt := summary.WindowStart
	rows := make([]BillingReconcileSnapshot, 0, len(users)+len(channels))
	appendRows := func(scope string, values map[int]int64) {
		ids := make([]int, 0, len(values))
		for id, used := range values {
			if used == 0 {
				continue
			}
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			rows = append(rows, BillingReconcileSnapshot{
				RunDate:   summary.RunDate,
				Scope:     scope,
				EntityId:  id,
				UsedQuota: values[id],
				TakenAt:   takenAt,
			})
		}
	}
	appendRows(BillingReconcileScopeUser, users)
	appendRows(BillingReconcileScopeChannel, channels)

	if len(rows) > 0 {
		if err := DB.CreateInBatches(rows, 200).Error; err != nil {
			return err
		}
	}
	if previousAt > 0 && previousAt < takenAt {
		// 留两组：上一轮（下一轮的期初）与本轮。
		if err := DB.Where("taken_at < ?", previousAt).Delete(&BillingReconcileSnapshot{}).Error; err != nil {
			return err
		}
	}
	return nil
}
