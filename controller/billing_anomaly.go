package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// AdminListBillingAnomalies lists ledger anomalies (charges that could not be
// applied consistently). This is the visibility half of G-10: an imbalance
// between the wallet ledger and the token ledger must be observable, not just a
// runtime log line.
func AdminListBillingAnomalies(c *gin.Context) {
	status := c.Query("status")
	if status == "" {
		status = model.BillingAnomalyStatusPending
	}
	limit := 0
	if value := c.Query("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			limit = parsed
		}
	}
	rows, err := model.ListBillingAnomalies(status, limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pending, err := model.CountPendingBillingAnomalies()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"items":   rows,
			"pending": pending,
			"status":  status,
		},
	})
}

// AdminRetryBillingAnomalies re-applies the pending ledger adjustments. This is
// the retry half of G-10; it is idempotent per row (a resolved row is skipped).
func AdminRetryBillingAnomalies(c *gin.Context) {
	limit := 0
	if value := c.Query("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			limit = parsed
		}
	}
	resolved, failed, err := model.RetryPendingBillingAnomalies(limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pending, countErr := model.CountPendingBillingAnomalies()
	if countErr != nil {
		common.ApiError(c, countErr)
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"resolved": resolved,
			"failed":   failed,
			"pending":  pending,
		},
	})
}
