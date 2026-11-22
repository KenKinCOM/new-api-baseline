package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Billing recalc statuses. A recalc row is written for every processed log so
// the coverage gap of the recalculator is as visible as the differences it
// finds: "unsupported" rows are the pricing forms the recalculator refused to
// guess at.
const (
	BillingRecalcStatusMatched     = "matched"
	BillingRecalcStatusDiff        = "diff"
	BillingRecalcStatusUnsupported = "unsupported"
	BillingRecalcStatusError       = "error"
)

// BillingRecalcDiff is one replay of a historical consume log through the
// pricing that the log itself recorded (G-04).
//
// The row keeps both sides of the comparison so it can be used as evidence:
// original_quota is what was actually deducted (logs.quota), recalculated_quota
// is what the recorded ratios/expression produce today. delta is
// recalculated - original: positive means the user was undercharged, negative
// means overcharged.
//
// pricing_version / expr_hash / quota_per_unit are copied from the log so a
// reviewer can tell which pricing table generation and which frozen expression
// the replay relied on. Note carries the coverage decision (which pricing form
// was recognised, or why the row could not be recomputed).
type BillingRecalcDiff struct {
	Id                int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	LogId             int     `json:"log_id" gorm:"uniqueIndex:idx_billing_recalc_log;not null"`
	UserId            int     `json:"user_id" gorm:"index"`
	ModelName         string  `json:"model_name" gorm:"type:varchar(255);default:'';index"`
	Group             string  `json:"group" gorm:"type:varchar(64);default:''"`
	BillingForm       string  `json:"billing_form" gorm:"type:varchar(32);default:'';index"`
	Status            string  `json:"status" gorm:"type:varchar(16);default:'';index"`
	OriginalQuota     int     `json:"original_quota" gorm:"not null;default:0"`
	RecalculatedQuota int     `json:"recalculated_quota" gorm:"not null;default:0"`
	Delta             int     `json:"delta" gorm:"not null;default:0;index"`
	PricingVersion    string  `json:"pricing_version" gorm:"type:varchar(64);default:''"`
	ExprHash          string  `json:"expr_hash" gorm:"type:varchar(64);default:''"`
	QuotaPerUnit      float64 `json:"quota_per_unit" gorm:"not null;default:0"`
	Note              string  `json:"note" gorm:"type:text"`
	CreatedAt         int64   `json:"created_at" gorm:"bigint;index"`
	UpdatedAt         int64   `json:"updated_at" gorm:"bigint"`
}

func (BillingRecalcDiff) TableName() string {
	return "billing_recalc_diffs"
}

// UpsertBillingRecalcDiff writes (or refreshes) the row for one log. Re-running
// the recalculator on the same log is idempotent: the unique log_id makes the
// second pass an update instead of a duplicate evidence row.
func UpsertBillingRecalcDiff(row *BillingRecalcDiff) error {
	if row == nil {
		return errors.New("recalc diff row is nil")
	}
	if row.LogId <= 0 {
		return fmt.Errorf("recalc diff row has invalid log id %d", row.LogId)
	}
	now := common.GetTimestamp()
	if row.CreatedAt == 0 {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "log_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "model_name", "group", "billing_form", "status",
			"original_quota", "recalculated_quota", "delta",
			"pricing_version", "expr_hash", "quota_per_unit", "note", "updated_at",
		}),
	}).Create(row).Error
}

// ListBillingRecalcDiffs returns recalc rows, newest first. status may be one
// of the BillingRecalcStatus* values or empty for all.
func ListBillingRecalcDiffs(status string, limit int) ([]BillingRecalcDiff, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	tx := DB.Model(&BillingRecalcDiff{}).Order("created_at desc, id desc").Limit(limit)
	if status != "" {
		tx = tx.Where("status = ?", status)
	}
	var rows []BillingRecalcDiff
	if err := tx.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetBillingRecalcDiffByLogId reads the latest recalc verdict for a log.
func GetBillingRecalcDiffByLogId(logId int) (*BillingRecalcDiff, error) {
	var row BillingRecalcDiff
	err := DB.Where("log_id = ?", logId).First(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// GetConsumeLogById loads one row from the log database. It lives next to the
// recalc table because only the recalculator needs single-log access.
func GetConsumeLogById(id int) (*Log, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid log id %d", id)
	}
	var log Log
	err := LOG_DB.Where("id = ?", id).First(&log).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("log %d not found", id)
		}
		return nil, err
	}
	return &log, nil
}
