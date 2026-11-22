package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
)

// Billing anomaly kinds. Each one names a charge that could not be applied
// consistently across every ledger it touches.
const (
	// BillingAnomalyTokenQuotaAdjustFailed means the funding source settled
	// (wallet or subscription already debited/credited) but the token quota
	// adjustment failed, so the wallet ledger and the token ledger disagree.
	BillingAnomalyTokenQuotaAdjustFailed = "token_quota_adjust_failed"
)

const (
	BillingAnomalyStatusPending  = "pending"
	BillingAnomalyStatusResolved = "resolved"
)

// BillingAnomaly is the durable record of a billing step that failed after the
// charge had already been committed elsewhere. It replaces the previous
// behaviour of writing a SysLog line and forgetting about it: the imbalance now
// has an owner-visible row and a retry path (RetryPendingBillingAnomalies).
//
// The token key is deliberately NOT stored: retries resolve it from tokens by
// id so no credential is duplicated into this table.
type BillingAnomaly struct {
	Id        int    `json:"id" gorm:"primaryKey"`
	CreatedAt int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt int64  `json:"updated_at" gorm:"bigint"`
	Type      string `json:"type" gorm:"type:varchar(64);index"`
	Status    string `json:"status" gorm:"type:varchar(16);index"`
	UserId    int    `json:"user_id" gorm:"index"`
	TokenId   int    `json:"token_id" gorm:"index"`
	RequestId string `json:"request_id" gorm:"type:varchar(128);index"`
	// Delta is the token quota adjustment that still has to be applied:
	// positive = token quota must decrease, negative = must increase.
	Delta     int    `json:"delta"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error" gorm:"type:text"`
	LastTryAt int64  `json:"last_try_at" gorm:"bigint"`
}

func (BillingAnomaly) TableName() string {
	return "billing_anomalies"
}

// RecordBillingAnomaly persists one anomaly row.
func RecordBillingAnomaly(anomaly *BillingAnomaly) error {
	if anomaly == nil {
		return errors.New("anomaly is nil")
	}
	now := common.GetTimestamp()
	anomaly.CreatedAt = now
	anomaly.UpdatedAt = now
	if anomaly.Status == "" {
		anomaly.Status = BillingAnomalyStatusPending
	}
	return DB.Create(anomaly).Error
}

// RecordTokenQuotaAdjustFailure stores the token-quota imbalance left behind by
// a Settle whose funding side had already been committed.
func RecordTokenQuotaAdjustFailure(userId, tokenId int, requestId string, delta int, cause error) error {
	if delta == 0 || cause == nil {
		return nil
	}
	return RecordBillingAnomaly(&BillingAnomaly{
		Type:      BillingAnomalyTokenQuotaAdjustFailed,
		Status:    BillingAnomalyStatusPending,
		UserId:    userId,
		TokenId:   tokenId,
		RequestId: requestId,
		Delta:     delta,
		LastError: cause.Error(),
	})
}

// ListBillingAnomalies returns anomalies filtered by status ("" = any),
// newest first.
func ListBillingAnomalies(status string, limit int) ([]BillingAnomaly, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := DB.Model(&BillingAnomaly{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var rows []BillingAnomaly
	err := query.Order("id desc").Limit(limit).Find(&rows).Error
	return rows, err
}

// RetryPendingBillingAnomalies re-applies the token quota adjustment for every
// pending anomaly, up to limit rows. It is the retry entry for G-10: the wallet
// ledger and the token ledger converge without a manual SQL correction.
func RetryPendingBillingAnomalies(limit int) (resolved int, failed int, err error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []BillingAnomaly
	if err = DB.Where("status = ?", BillingAnomalyStatusPending).Order("id asc").Limit(limit).Find(&rows).Error; err != nil {
		return 0, 0, err
	}
	for i := range rows {
		row := &rows[i]
		retryErr := applyAnomalyDelta(row)
		now := common.GetTimestamp()
		updates := map[string]any{
			"attempts":    row.Attempts + 1,
			"last_try_at": now,
			"updated_at":  now,
		}
		if retryErr != nil {
			updates["last_error"] = retryErr.Error()
			failed++
			if dbErr := DB.Model(&BillingAnomaly{}).Where("id = ?", row.Id).Updates(updates).Error; dbErr != nil {
				return resolved, failed, dbErr
			}
			continue
		}
		updates["status"] = BillingAnomalyStatusResolved
		updates["last_error"] = ""
		resolved++
		if dbErr := DB.Model(&BillingAnomaly{}).Where("id = ?", row.Id).Updates(updates).Error; dbErr != nil {
			return resolved, failed, dbErr
		}
	}
	return resolved, failed, nil
}

// applyAnomalyDelta retries the failed ledger operation itself.
func applyAnomalyDelta(row *BillingAnomaly) error {
	switch row.Type {
	case BillingAnomalyTokenQuotaAdjustFailed:
		if row.TokenId <= 0 {
			return fmt.Errorf("anomaly %d has no token id", row.Id)
		}
		token, lookupErr := GetTokenById(row.TokenId)
		if lookupErr != nil {
			return fmt.Errorf("load token %d: %w", row.TokenId, lookupErr)
		}
		if row.Delta > 0 {
			return DecreaseTokenQuota(token.Id, token.Key, row.Delta)
		}
		return IncreaseTokenQuota(token.Id, token.Key, -row.Delta)
	default:
		return fmt.Errorf("unknown billing anomaly type %q", row.Type)
	}
}

// CountPendingBillingAnomalies reports how many anomalies still need attention.
func CountPendingBillingAnomalies() (int64, error) {
	var count int64
	err := DB.Model(&BillingAnomaly{}).Where("status = ?", BillingAnomalyStatusPending).Count(&count).Error
	return count, err
}
