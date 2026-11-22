package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// AdminRecalculateConsumeLog replays one historical consume log and records the
// verdict in billing_recalc_diffs (G-04). The recalculator only trusts the
// pricing factors frozen on the log; it never reads today's price table, so the
// result answers "what should this request have cost under the pricing it was
// charged with".
func AdminRecalculateConsumeLog(c *gin.Context) {
	var request struct {
		LogId int `json:"log_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiErrorMsg(c, "invalid request body: "+err.Error())
		return
	}
	if request.LogId <= 0 {
		common.ApiErrorMsg(c, "log_id is required")
		return
	}
	result, err := service.RecalculateConsumeLogById(request.LogId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data":    result,
	})
}

// AdminListBillingRecalcDiffs returns the recorded price-difference rows,
// including the "unsupported" rows that document uncovered pricing forms.
func AdminListBillingRecalcDiffs(c *gin.Context) {
	status := c.Query("status")
	limit := 0
	if value := c.Query("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			limit = parsed
		}
	}
	rows, err := model.ListBillingRecalcDiffs(status, limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"items":  rows,
			"status": status,
		},
	})
}
