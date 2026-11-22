package model

import (
	"time"

	"github.com/QuantumNous/new-api/common"
)

// TopUpReconcileDiff 是一次日终对账里发现的差异行（R6）。
//
// 三个口径必须能对上：
//
//	订单口径  top_ups.credited_quota（成功订单的实际入账快照，A6）
//	流水口径  quota_ledgers.delta_quota（change_type=topup，R5）
//	日志口径  logs.quota（type=1，R2）
//
// 逐单能对上的是「订单 vs 流水」（两者都带 trade_no）；日志没有 trade_no 列，
// 只能按日区间做总额比对，因此日志口径的差异以一条 aggregated 行呈现。
type TopUpReconcileDiff struct {
	Id          int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	RunDate     string `json:"run_date" gorm:"type:varchar(10);index:idx_topup_reconcile_run_date,priority:1"`
	Kind        string `json:"kind" gorm:"type:varchar(32);index"` // order_vs_ledger / ledger_vs_logs
	TradeNo     string `json:"trade_no" gorm:"type:varchar(255);default:'';index"`
	UserId      int    `json:"user_id" gorm:"default:0"`
	OrderQuota  int64  `json:"order_quota" gorm:"not null;default:0"`
	LedgerQuota int64  `json:"ledger_quota" gorm:"not null;default:0"`
	LogQuota    int64  `json:"log_quota" gorm:"not null;default:0"`
	Message     string `json:"message" gorm:"type:varchar(500);default:''"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;index:idx_topup_reconcile_run_date,priority:2"`
}

func (TopUpReconcileDiff) TableName() string {
	return "topup_reconcile_diffs"
}

// TopUpReconcileSummary 汇总一次对账的规模与差异条数。
type TopUpReconcileSummary struct {
	StartTimestamp   int64 `json:"start_timestamp"`
	EndTimestamp     int64 `json:"end_timestamp"`
	OrderQuotaTotal  int64 `json:"order_quota_total"`
	LedgerQuotaTotal int64 `json:"ledger_quota_total"`
	LogQuotaTotal    int64 `json:"log_quota_total"`
	OrderCount       int   `json:"order_count"`
	DiffCount        int   `json:"diff_count"`
}

type topUpOrderRow struct {
	TradeNo       string
	UserId        int
	CreditedQuota int64
}

// RunTopUpReconcile 对 [start, end] 内完成的充值单做三方对账，
// 差异写入 topup_reconcile_diffs 并返回汇总。无差异时不产生任何行（不产噪音）。
func RunTopUpReconcile(runDate string, start, end int64) (*TopUpReconcileSummary, error) {
	summary := &TopUpReconcileSummary{StartTimestamp: start, EndTimestamp: end}

	var orders []topUpOrderRow
	if err := DB.Model(&TopUp{}).
		Select("trade_no, user_id, credited_quota").
		Where("status = ? AND complete_time >= ? AND complete_time <= ?", common.TopUpStatusSuccess, start, end).
		Find(&orders).Error; err != nil {
		return nil, err
	}
	summary.OrderCount = len(orders)

	type ledgerRow struct {
		RefId       string
		LedgerQuota int64
	}
	var ledgers []ledgerRow
	if err := DB.Model(&QuotaLedger{}).
		Select("ref_id, COALESCE(SUM(delta_quota), 0) AS ledger_quota").
		Where("change_type = ? AND created_at >= ? AND created_at <= ?", QuotaLedgerTypeTopup, start, end).
		Group("ref_id").Find(&ledgers).Error; err != nil {
		return nil, err
	}
	ledgerByTrade := make(map[string]int64, len(ledgers))
	for _, row := range ledgers {
		ledgerByTrade[row.RefId] = row.LedgerQuota
		summary.LedgerQuotaTotal += row.LedgerQuota
	}

	var logTotal int64
	if err := DB.Table("logs").
		Select("COALESCE(SUM(quota), 0)").
		Where("type = ? AND created_at >= ? AND created_at <= ?", LogTypeTopup, start, end).
		Scan(&logTotal).Error; err != nil {
		return nil, err
	}
	summary.LogQuotaTotal = logTotal

	diffs := make([]TopUpReconcileDiff, 0, 8)
	now := common.GetTimestamp()
	for _, order := range orders {
		summary.OrderQuotaTotal += order.CreditedQuota
		ledgerQuota := ledgerByTrade[order.TradeNo]
		if ledgerQuota == order.CreditedQuota {
			continue
		}
		diffs = append(diffs, TopUpReconcileDiff{
			RunDate:     runDate,
			Kind:        "order_vs_ledger",
			TradeNo:     order.TradeNo,
			UserId:      order.UserId,
			OrderQuota:  order.CreditedQuota,
			LedgerQuota: ledgerQuota,
			CreatedAt:   now,
			Message:     "成功订单的 credited_quota 与额度流水不一致",
		})
	}

	if summary.LedgerQuotaTotal != summary.LogQuotaTotal {
		diffs = append(diffs, TopUpReconcileDiff{
			RunDate:     runDate,
			Kind:        "ledger_vs_logs",
			LedgerQuota: summary.LedgerQuotaTotal,
			LogQuota:    summary.LogQuotaTotal,
			CreatedAt:   now,
			Message:     "当日额度流水合计与充值日志合计不一致（日志无单号，只做区间总额比对）",
		})
	}

	if len(diffs) > 0 {
		if err := DB.CreateInBatches(diffs, 100).Error; err != nil {
			return summary, err
		}
	}
	summary.DiffCount = len(diffs)
	return summary, nil
}

// TopUpReconcileRunDate 返回对账窗口所属的自然日（server 本地时区）。
func TopUpReconcileRunDate(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02")
}
