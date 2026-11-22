package model

import (
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// QuotaLedger 记录 users.quota 的每一次结构化变动（R5）。
//
// 此前额度变动只落在 users.quota 一个标量上：没有期初/期末快照，也没有流水表，
// 事后只能靠滚动的业务日志（签到甚至完全不写日志）推断余额是怎么变成现在这样的。
// 本表是"额度流水"，一行对应一次已提交的额度变动：
//
//	change_type = topup / checkin / adjust / aff_transfer / redemption
//	delta_quota = 本次变动量（可为负）
//	before/after = 同一事务内实测的期初与期末余额
//
// 与 logs 表的分工：logs 是对外可见的业务日志（含消费），本表只记钱包额度的
// 结构化增减，供日终轧账（R6）与区间汇总使用。
type QuotaLedger struct {
	Id          int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	UserId      int    `json:"user_id" gorm:"not null;index:idx_quota_ledger_user_created,priority:1"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;not null;index:idx_quota_ledger_user_created,priority:2;index:idx_quota_ledger_created_type,priority:1"`
	ChangeType  string `json:"change_type" gorm:"type:varchar(32);not null;index;index:idx_quota_ledger_created_type,priority:2"`
	DeltaQuota  int64  `json:"delta_quota" gorm:"not null"`
	BeforeQuota int64  `json:"before_quota" gorm:"not null"`
	AfterQuota  int64  `json:"after_quota" gorm:"not null"`
	Source      string `json:"source" gorm:"type:varchar(50);default:'';index"`
	RefId       string `json:"ref_id" gorm:"type:varchar(255);default:'';index"`
	Operator    string `json:"operator" gorm:"type:varchar(100);default:''"`
	Remark      string `json:"remark" gorm:"type:varchar(255);default:''"`
}

func (QuotaLedger) TableName() string {
	return "quota_ledgers"
}

const (
	QuotaLedgerTypeTopup       = "topup"
	QuotaLedgerTypeCheckin     = "checkin"
	QuotaLedgerTypeAdjust      = "adjust"
	QuotaLedgerTypeAffTransfer = "aff_transfer"
	QuotaLedgerTypeRedemption  = "redemption"
)

// RecordQuotaLedger 在调用方事务内写入一行额度流水。
// 传 nil 事务时直接用默认连接写入。
func RecordQuotaLedger(tx *gorm.DB, entry *QuotaLedger) error {
	if entry == nil {
		return nil
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = common.GetTimestamp()
	}
	db := tx
	if db == nil {
		db = DB
	}
	return db.Create(entry).Error
}

// recordQuotaLedgerInTx 读取用户当前额度并按 creditedDelta 反推期初值后落一行流水。
// 必须在已经完成额度增减的同一事务内调用（同一连接能读到自己的写入）。
func recordQuotaLedgerInTx(tx *gorm.DB, userId int, delta int64, changeType string, source string, refId string, operator string) error {
	var after int
	if err := tx.Model(&User{}).Select("quota").Where("id = ?", userId).Scan(&after).Error; err != nil {
		return err
	}
	return RecordQuotaLedger(tx, &QuotaLedger{
		UserId:      userId,
		CreatedAt:   common.GetTimestamp(),
		ChangeType:  changeType,
		DeltaQuota:  delta,
		BeforeQuota: int64(after) - delta,
		AfterQuota:  int64(after),
		Source:      source,
		RefId:       refId,
		Operator:    operator,
	})
}

// SumQuotaLedgerDelta 按用户/类型/时间区间汇总额度变动，供日终轧账使用。
func SumQuotaLedgerDelta(changeType string, startTimestamp int64, endTimestamp int64) (int64, error) {
	var total int64
	query := DB.Model(&QuotaLedger{}).Select("COALESCE(SUM(delta_quota), 0)")
	if changeType != "" {
		query = query.Where("change_type = ?", changeType)
	}
	if startTimestamp != 0 {
		query = query.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		query = query.Where("created_at <= ?", endTimestamp)
	}
	err := query.Scan(&total).Error
	return total, err
}
