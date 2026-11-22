package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const billingReconcileTestBase = int64(1735689600) // 2025-01-01 08:00:00 +08:00

func setupBillingReconcileTest(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&BillingReconcileSnapshot{}, &BillingReconcileDiff{}))
	clearBillingReconcileTables()
	t.Cleanup(clearBillingReconcileTables)
	t.Cleanup(func() {
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM channels")
		DB.Exec("DELETE FROM logs")
		DB.Exec("DELETE FROM system_tasks")
	})
}

func clearBillingReconcileTables() {
	DB.Exec("DELETE FROM billing_reconcile_snapshots")
	DB.Exec("DELETE FROM billing_reconcile_diffs")
	DB.Exec("DELETE FROM users")
	DB.Exec("DELETE FROM channels")
	DB.Exec("DELETE FROM logs")
	DB.Exec("DELETE FROM system_tasks")
}

func seedBillingReconcileEntities(t *testing.T, userUsed int, channelUsed int) (User, Channel) {
	t.Helper()
	user := User{
		Username:  "reconcile-user",
		Password:  "unused-password-hash",
		Status:    1,
		Group:     "default",
		Quota:     1000,
		UsedQuota: userUsed,
	}
	require.NoError(t, DB.Create(&user).Error)
	channel := Channel{
		Name:      "reconcile-channel",
		Key:       "sk-test",
		UsedQuota: int64(channelUsed),
	}
	require.NoError(t, DB.Create(&channel).Error)
	return user, channel
}

func insertBillingReconcileLog(t *testing.T, userID int, channelID int, logType int, quota int, createdAt int64) {
	t.Helper()
	require.NoError(t, LOG_DB.Table("logs").Create(map[string]any{
		"user_id":    userID,
		"channel_id": channelID,
		"type":       logType,
		"quota":      quota,
		"created_at": createdAt,
		"username":   "reconcile-user",
		"model_name": "gpt-test",
	}).Error)
}

func bumpBillingReconcileUsage(t *testing.T, userID int, channelID int, userDelta int, channelDelta int) {
	t.Helper()
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).
		Update("used_quota", gorm.Expr("used_quota + ?", userDelta)).Error)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channelID).
		Update("used_quota", gorm.Expr("used_quota + ?", channelDelta)).Error)
}

func countBillingReconcileRows(t *testing.T, model any) int64 {
	t.Helper()
	var total int64
	require.NoError(t, DB.Model(model).Count(&total).Error)
	return total
}

// 首次运行只能建立基线：没有期初快照就没有「窗口增量」，此时绝不能报一致。
func TestBillingReconcileBaselineOnly(t *testing.T) {
	setupBillingReconcileTest(t)
	seedBillingReconcileEntities(t, 100, 100)

	summary, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)
	require.NotNil(t, summary)

	assert.Equal(t, BillingReconcileStatusBaseline, summary.Status)
	assert.Equal(t, 0, summary.DiffCount)
	assert.Equal(t, int64(100), summary.UsersCumulativeQuota)
	assert.Equal(t, int64(100), summary.ChannelsCumulativeQuota)
	assert.ElementsMatch(t, []string{BillingReconcileScopeUser, BillingReconcileScopeChannel}, summary.MissingSides)
	assert.Equal(t, int64(2), countBillingReconcileRows(t, &BillingReconcileSnapshot{}))
	assert.Equal(t, int64(0), countBillingReconcileRows(t, &BillingReconcileDiff{}), "基线轮不应产差异行")
}

// 三方都对得上的一天不产任何差异行。
func TestBillingReconcileNoDifferenceProducesNoRows(t *testing.T) {
	setupBillingReconcileTest(t)
	user, channel := seedBillingReconcileEntities(t, 100, 100)

	_, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)

	insertBillingReconcileLog(t, user.Id, channel.Id, LogTypeConsume, 30, billingReconcileTestBase+10)
	bumpBillingReconcileUsage(t, user.Id, channel.Id, 30, 30)

	summary, err := RunBillingReconcile(billingReconcileTestBase + 100)
	require.NoError(t, err)

	assert.Equal(t, BillingReconcileStatusMatched, summary.Status)
	assert.Equal(t, int64(30), summary.LogsNetQuota)
	assert.Equal(t, int64(30), summary.UsersDeltaQuota)
	assert.Equal(t, int64(30), summary.ChannelsDeltaQuota)
	assert.Equal(t, 0, summary.DiffCount)
	assert.Equal(t, int64(0), countBillingReconcileRows(t, &BillingReconcileDiff{}))
}

// 人为造 1 单位差异：日志 30、用户账 29、渠道账 30，必须报出来并落差异表。
func TestBillingReconcileDetectsOneUnitDifference(t *testing.T) {
	setupBillingReconcileTest(t)
	user, channel := seedBillingReconcileEntities(t, 100, 100)

	_, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)

	insertBillingReconcileLog(t, user.Id, channel.Id, LogTypeConsume, 30, billingReconcileTestBase+10)
	bumpBillingReconcileUsage(t, user.Id, channel.Id, 29, 30)

	summary, err := RunBillingReconcile(billingReconcileTestBase + 100)
	require.NoError(t, err)

	assert.Equal(t, BillingReconcileStatusDiffFound, summary.Status)
	assert.Equal(t, int64(30), summary.LogsNetQuota)
	assert.Equal(t, int64(29), summary.UsersDeltaQuota)
	assert.Equal(t, int64(30), summary.ChannelsDeltaQuota)

	var diffs []BillingReconcileDiff
	require.NoError(t, DB.Order("id").Find(&diffs).Error)

	byKind := map[string][]BillingReconcileDiff{}
	for _, diff := range diffs {
		byKind[diff.Kind] = append(byKind[diff.Kind], diff)
	}

	daily := byKind[BillingReconcileKindLogsVsUsers]
	require.Len(t, daily, 1, "日志与用户账的 1 单位差异必须报出")
	assert.Equal(t, int64(1), daily[0].DiffQuota)
	assert.Equal(t, int64(30), daily[0].LogQuota)
	assert.Equal(t, int64(29), daily[0].UserQuota)

	detail := byKind[BillingReconcileKindUserDetail]
	require.Len(t, detail, 1)
	assert.Equal(t, user.Id, detail[0].UserId)
	assert.Equal(t, int64(1), detail[0].DiffQuota)

	assert.Empty(t, byKind[BillingReconcileKindLogsVsChannels], "渠道账与日志一致，不应报差异")
	assert.Len(t, byKind[BillingReconcileKindUsersVsChannels], 1, "累计口径同样应看出 1 单位差")
	assert.Equal(t, int64(-1), byKind[BillingReconcileKindUsersVsChannels][0].DiffQuota)

	assert.Equal(t, len(diffs), summary.DiffCount)
}

// 退款日志按净额（消费-退款）参与对账，退款当天不应产出假差异。
func TestBillingReconcileNetsRefundLogs(t *testing.T) {
	setupBillingReconcileTest(t)
	user, channel := seedBillingReconcileEntities(t, 100, 100)

	_, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)

	insertBillingReconcileLog(t, user.Id, channel.Id, LogTypeConsume, 30, billingReconcileTestBase+10)
	insertBillingReconcileLog(t, user.Id, channel.Id, LogTypeRefund, 10, billingReconcileTestBase+20)
	bumpBillingReconcileUsage(t, user.Id, channel.Id, 20, 20)

	summary, err := RunBillingReconcile(billingReconcileTestBase + 100)
	require.NoError(t, err)

	assert.Equal(t, BillingReconcileStatusMatched, summary.Status)
	assert.Equal(t, int64(30), summary.LogsConsumeQuota)
	assert.Equal(t, int64(10), summary.LogsRefundQuota)
	assert.Equal(t, int64(20), summary.LogsNetQuota)
	assert.Equal(t, 0, summary.DiffCount)
}

// 日志已被 log_cleanup 清到窗口内部时，日志侧「未取到」，不能给出一致结论。
func TestBillingReconcileMarksUndecidableWhenLogsPurged(t *testing.T) {
	setupBillingReconcileTest(t)
	user, channel := seedBillingReconcileEntities(t, 100, 100)

	_, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)

	require.NoError(t, DB.Create(&SystemTask{
		TaskID:  "systask_cleanup_test",
		Type:    SystemTaskTypeLogCleanup,
		Status:  SystemTaskStatusSucceeded,
		Payload: `{"target_timestamp":1735689660,"batch_size":100}`,
	}).Error)

	bumpBillingReconcileUsage(t, user.Id, channel.Id, 30, 30)

	summary, err := RunBillingReconcile(billingReconcileTestBase + 200)
	require.NoError(t, err)

	assert.Equal(t, BillingReconcileStatusUndecidable, summary.Status)
	assert.Contains(t, summary.MissingSides, "logs")

	var diffs []BillingReconcileDiff
	require.NoError(t, DB.Find(&diffs).Error)
	require.Len(t, diffs, 1)
	assert.Equal(t, BillingReconcileKindSideMissing, diffs[0].Kind)
	assert.Contains(t, diffs[0].Message, "日志侧未取到")
}

// 同一个期末时间重复触发不应把上一轮基线冲掉。
func TestBillingReconcileRejectsEmptyWindow(t *testing.T) {
	setupBillingReconcileTest(t)
	seedBillingReconcileEntities(t, 100, 100)

	_, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)
	before := countBillingReconcileRows(t, &BillingReconcileSnapshot{})

	summary, err := RunBillingReconcile(billingReconcileTestBase)
	require.NoError(t, err)

	assert.Equal(t, BillingReconcileStatusUndecidable, summary.Status)
	assert.Contains(t, summary.MissingSides, "window")
	assert.Equal(t, before, countBillingReconcileRows(t, &BillingReconcileSnapshot{}))
}
