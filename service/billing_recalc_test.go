package service

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recalcTestLog(t *testing.T, promptTokens, completionTokens, quota int, other map[string]any) *RecalcResult {
	t.Helper()
	raw, err := json.Marshal(other)
	require.NoError(t, err)
	log := &model.Log{
		Id:               1,
		UserId:           7,
		Type:             model.LogTypeConsume,
		ModelName:        "test-model",
		Group:            "default",
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Quota:            quota,
		Other:            string(raw),
	}
	result, err := RecalculateConsumeLog(log)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// text token path: base tokens minus cache, cache priced at cache_ratio,
// completion at completion_ratio, product of model and group ratios.
//
//	base = 1000 - 200 = 800, cache = 200 * 0.1 = 20
//	prompt = 820, completion = 500 * 2 = 1000
//	quota = (820 + 1000) * 0.5 = 910
func TestRecalculateTextTokenQuota(t *testing.T) {
	result := recalcTestLog(t, 1000, 500, 910, map[string]any{
		"model_ratio":      0.5,
		"group_ratio":      1.0,
		"completion_ratio": 2.0,
		"cache_tokens":     200,
		"cache_ratio":      0.1,
		"quota_per_unit":   500000.0,
		"pricing_version":  "pv-test",
	})
	assert.Equal(t, BillingFormTextToken, result.BillingForm)
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status)
	assert.Equal(t, 910, result.RecalculatedQuota)
	assert.Equal(t, 0, result.Delta)
}

// text token path with a different actual charge: difference is
// recalculated - original, positive = undercharged.
func TestRecalculateTextTokenReportsDiff(t *testing.T) {
	result := recalcTestLog(t, 1000, 500, 900, map[string]any{
		"model_ratio":      0.5,
		"group_ratio":      1.0,
		"completion_ratio": 2.0,
		"cache_tokens":     200,
		"cache_ratio":      0.1,
	})
	assert.Equal(t, model.BillingRecalcStatusDiff, result.Status)
	assert.Equal(t, 910, result.RecalculatedQuota)
	assert.Equal(t, 10, result.Delta)
}

// fixed-price path with an extra multiplier recorded by G-01:
//
//	quota = model_price * quota_per_unit * group_ratio * n
//	      = 2 * 500000 * 1 * 2 = 2000000
func TestRecalculateFixedPriceWithOtherRatios(t *testing.T) {
	result := recalcTestLog(t, 0, 0, 2000000, map[string]any{
		"model_price":    2.0,
		"group_ratio":    1.0,
		"quota_per_unit": 500000.0,
		"other_ratios":   map[string]any{"n": 2.0},
	})
	assert.Equal(t, BillingFormPerCall, result.BillingForm)
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status)
	assert.Equal(t, 2000000, result.RecalculatedQuota)
}

// fixed price requires quota_per_unit; a log that predates G-15 must be
// reported as unsupported instead of guessed with today's coefficient.
func TestRecalculateFixedPriceWithoutQuotaPerUnitIsUnsupported(t *testing.T) {
	result := recalcTestLog(t, 0, 0, 100, map[string]any{
		"model_price": 2.0,
		"group_ratio": 1.0,
	})
	assert.Equal(t, model.BillingRecalcStatusUnsupported, result.Status)
	assert.Contains(t, result.noteString(), "quota_per_unit")
}

// tool surcharge is priced from the frozen price on the log:
//
//	tokens = 1000 * 0.5 = 500
//	surcharge = 5 * 2 / 1000 * 1 * 500000 = 5000
//	quota = 5500
func TestRecalculateToolSurcharge(t *testing.T) {
	result := recalcTestLog(t, 1000, 0, 5500, map[string]any{
		"model_ratio":    0.5,
		"group_ratio":    1.0,
		"quota_per_unit": 500000.0,
		"tool_surcharges": []any{
			map[string]any{"name": "web_search", "count": 2.0, "price": 5.0},
		},
	})
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status)
	assert.Equal(t, 5500, result.RecalculatedQuota)
}

// audio token path:
//
//	text_in 1000 + text_out 200*2 + audio_in 100*10 + audio_out 50*10*2
//	= 1000 + 400 + 1000 + 1000 = 3400, * (0.5 * 1) = 1700
func TestRecalculateAudioTokenQuota(t *testing.T) {
	result := recalcTestLog(t, 1200, 250, 1700, map[string]any{
		"audio":                  true,
		"text_input":             1000,
		"text_output":            200,
		"audio_input":            100,
		"audio_output":           50,
		"model_ratio":            0.5,
		"group_ratio":            1.0,
		"completion_ratio":       2.0,
		"audio_ratio":            10.0,
		"audio_completion_ratio": 2.0,
		"quota_per_unit":         500000.0,
	})
	assert.Equal(t, BillingFormAudioToken, result.BillingForm)
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status)
	assert.Equal(t, 1700, result.RecalculatedQuota)
}

func tieredOther(t *testing.T, expr string, extra map[string]any) map[string]any {
	t.Helper()
	other := map[string]any{
		"billing_mode":   "tiered_expr",
		"expr_b64":       base64.StdEncoding.EncodeToString([]byte(expr)),
		"expr_hash":      billingexpr.ExprHashString(expr),
		"expr_version":   1,
		"model_ratio":    0.0,
		"group_ratio":    1.0,
		"quota_per_unit": 500000.0,
		"matched_tier":   "standard",
	}
	for key, value := range extra {
		other[key] = value
	}
	return other
}

// tiered expression: cost = p*0.5 + c*1 = 500 + 500 = 1000
// quota = cost / 1e6 * quota_per_unit = 1000 / 1e6 * 500000 = 500
func TestRecalculateTieredExpr(t *testing.T) {
	expr := `tier("standard", p * 0.5 + c * 1)`
	result := recalcTestLog(t, 1000, 500, 500, tieredOther(t, expr, nil))
	assert.Equal(t, BillingFormTieredExpr, result.BillingForm)
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status)
	assert.Equal(t, 500, result.RecalculatedQuota)
	assert.Equal(t, billingexpr.ExprHashString(expr), result.ExprHash)
}

// tiered expression that prices image tokens: the replay must rebuild the
// same token parameters (p excludes img) as BuildTieredTokenParams.
//
//	p = 900, img = 100 -> cost = 900*1 + 100*2 = 1100 -> quota = 550
func TestRecalculateTieredRebuildsImageTokens(t *testing.T) {
	expr := `tier("standard", p * 1 + img * 2)`
	result := recalcTestLog(t, 1000, 0, 550, tieredOther(t, expr, map[string]any{
		"image_output": 100,
	}))
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
	assert.Equal(t, 550, result.RecalculatedQuota)
}

// billing_tokens is the exact parameter snapshot; when present it wins over
// the reconstruction.
func TestRecalculateTieredPrefersBillingTokens(t *testing.T) {
	expr := `tier("standard", p * 1 + img_cr * 2)`
	result := recalcTestLog(t, 1000, 0, 100, tieredOther(t, expr, map[string]any{
		"billing_tokens": map[string]any{
			"p": 0.0, "c": 0.0, "len": 1000.0, "cr": 0.0, "cc": 0.0, "cc1h": 0.0,
			"cc12h": 0.0, "img": 0.0, "img_cr": 100.0, "img_o": 0.0, "ai": 0.0, "ao": 0.0,
		},
	}))
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
	assert.Equal(t, 100, result.RecalculatedQuota)
}

// img_cr without the billing_tokens snapshot cannot be rebuilt: refuse.
func TestRecalculateTieredImgCRWithoutSnapshotIsUnsupported(t *testing.T) {
	expr := `tier("standard", p * 1 + img_cr * 2)`
	result := recalcTestLog(t, 1000, 0, 100, tieredOther(t, expr, nil))
	assert.Equal(t, model.BillingRecalcStatusError, result.Status)
	assert.Contains(t, result.noteString(), "img_cr")
}

// request-dependent multiplier cannot be replayed from the log alone.
func TestRecalculateTieredRequestRuleIsUnsupported(t *testing.T) {
	expr := `tier("standard", p * 1) * (param("fast") == true ? 2 : 1)`
	result := recalcTestLog(t, 1000, 0, 100, tieredOther(t, expr, map[string]any{
		"request_rules": []any{
			map[string]any{"cond": `param("fast") == true`, "multiplier": 2.0, "matched": true},
		},
	}))
	assert.Equal(t, model.BillingRecalcStatusError, result.Status)
	assert.Contains(t, result.noteString(), "request-dependent")
}

// a tampered expression must not be replayed under a different hash.
func TestRecalculateTieredExprHashMismatch(t *testing.T) {
	expr := `tier("standard", p * 1)`
	other := tieredOther(t, expr, map[string]any{"expr_hash": "deadbeef"})
	result := recalcTestLog(t, 1000, 0, 100, other)
	assert.Equal(t, model.BillingRecalcStatusError, result.Status)
	assert.Contains(t, result.noteString(), "does not match")
}

// task logs price from task usage facts; the recalculator refuses them.
func TestRecalculateTaskLogUnsupported(t *testing.T) {
	result := recalcTestLog(t, 0, 0, 100, map[string]any{
		"is_task":     true,
		"model_price": 2.0,
		"group_ratio": 1.0,
	})
	assert.Equal(t, model.BillingRecalcStatusUnsupported, result.Status)
	assert.Contains(t, result.noteString(), "is_task")
}

// a log without any pricing snapshot (pre G-01) is unsupported, not "0 cost".
func TestRecalculateEmptyOtherUnsupported(t *testing.T) {
	log := &model.Log{Id: 1, Type: model.LogTypeConsume, Quota: 100}
	result, err := RecalculateConsumeLog(log)
	require.NoError(t, err)
	assert.Equal(t, model.BillingRecalcStatusUnsupported, result.Status)
}

// Cross-check: the replay must land on the same integer as the live text
// settlement path for the same inputs. This is the fidelity guarantee for the
// non-tiered forms.
func TestRecalcMatchesLiveTextSettlement(t *testing.T) {
	previousQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = previousQuotaPerUnit })

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	t.Run("token pricing with cache", func(t *testing.T) {
		info := &relaycommon.RelayInfo{
			OriginModelName: "recalc-live-model",
			StartTime:       time.Now(),
			PriceData: hosttypes.PriceData{
				ModelRatio:      0.5,
				CompletionRatio: 2,
				CacheRatio:      0.1,
				GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
			},
		}
		usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
		usage.PromptTokensDetails.CachedTokens = 200

		summary := calculateTextQuotaSummary(c, info, usage)
		require.Equal(t, 910, summary.Quota)

		result := recalcTestLog(t, summary.PromptTokens, summary.CompletionTokens, summary.Quota, map[string]any{
			"model_ratio":      summary.ModelRatio,
			"group_ratio":      summary.GroupRatio,
			"completion_ratio": summary.CompletionRatio,
			"cache_tokens":     summary.CacheTokens,
			"cache_ratio":      summary.CacheRatio,
			"quota_per_unit":   common.QuotaPerUnit,
		})
		assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
		assert.Equal(t, summary.Quota, result.RecalculatedQuota)
	})

	t.Run("fixed price", func(t *testing.T) {
		info := &relaycommon.RelayInfo{
			OriginModelName: "recalc-live-fixed",
			StartTime:       time.Now(),
			PriceData: hosttypes.PriceData{
				UsePrice:       true,
				ModelPrice:     2,
				GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1},
			},
		}
		usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 0, TotalTokens: 1000}

		summary := calculateTextQuotaSummary(c, info, usage)
		require.Equal(t, 1000000, summary.Quota)

		result := recalcTestLog(t, summary.PromptTokens, summary.CompletionTokens, summary.Quota, map[string]any{
			"model_price":      summary.ModelPrice,
			"model_ratio":      summary.ModelRatio,
			"group_ratio":      summary.GroupRatio,
			"completion_ratio": summary.CompletionRatio,
			"quota_per_unit":   common.QuotaPerUnit,
		})
		assert.Equal(t, BillingFormTextFixed, result.BillingForm)
		assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
		assert.Equal(t, summary.Quota, result.RecalculatedQuota)
	})
}

// Legacy Claude-derived OpenAI usage: the 5m/1h split is present but no usage
// semantic, so settlement keeps prompt_tokens intact and prices the split:
//
//	base 1000 + (500*1.25 + 400*1.25 + 100*2) = 1000 + 1325 = 2325
func TestRecalculateLegacyClaudeSplit(t *testing.T) {
	result := recalcTestLog(t, 1000, 0, 2325, map[string]any{
		"model_ratio":              1.0,
		"group_ratio":              1.0,
		"cache_creation_tokens":    1000,
		"cache_creation_ratio":     1.25,
		"cache_creation_tokens_5m": 400,
		"cache_creation_ratio_5m":  1.25,
		"cache_creation_tokens_1h": 100,
		"cache_creation_ratio_1h":  2.0,
		"quota_per_unit":           500000.0,
	})
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
	assert.Equal(t, 2325, result.RecalculatedQuota)
}

// End-to-end persistence: load a consume log from the log DB, replay it, write
// the verdict, and verify re-running updates instead of duplicating the row.
func TestRecalculateAndRecordPersistsDiff(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	oldDB, oldLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() { model.DB, model.LOG_DB = oldDB, oldLogDB })

	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.BillingRecalcDiff{}))
	other, err := json.Marshal(map[string]any{
		"model_ratio":      0.5,
		"group_ratio":      1.0,
		"completion_ratio": 2.0,
		"cache_tokens":     200,
		"cache_ratio":      0.1,
		"quota_per_unit":   500000.0,
		"pricing_version":  "pv-test",
	})
	require.NoError(t, err)
	log := &model.Log{
		UserId:           7,
		CreatedAt:        time.Now().Unix(),
		Type:             model.LogTypeConsume,
		ModelName:        "recalc-db-model",
		Group:            "default",
		PromptTokens:     1000,
		CompletionTokens: 500,
		Quota:            900,
		Other:            string(other),
	}
	require.NoError(t, db.Create(log).Error)

	first, err := RecalculateConsumeLogById(log.Id)
	require.NoError(t, err)
	assert.Equal(t, model.BillingRecalcStatusDiff, first.Status)
	assert.Equal(t, 10, first.Delta)

	row, err := model.GetBillingRecalcDiffByLogId(log.Id)
	require.NoError(t, err)
	assert.Equal(t, 910, row.RecalculatedQuota)
	assert.Equal(t, 900, row.OriginalQuota)
	assert.Equal(t, 10, row.Delta)
	assert.Equal(t, "pv-test", row.PricingVersion)

	_, err = RecalculateConsumeLogById(log.Id)
	require.NoError(t, err)
	var count int64
	require.NoError(t, db.Model(&model.BillingRecalcDiff{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// A configured 0 cache-write ratio means free cache writes; it must not be
// confused with a missing field (which defaults to 1).
//
//	base = 2000 - 1000 = 1000, cache writes = 1000 * 0 = 0 -> quota 1000
func TestRecalculateZeroCacheCreationRatio(t *testing.T) {
	result := recalcTestLog(t, 2000, 0, 1000, map[string]any{
		"model_ratio":           1.0,
		"group_ratio":           1.0,
		"cache_creation_tokens": 1000,
		"cache_creation_ratio":  0.0,
		"quota_per_unit":        500000.0,
	})
	assert.Equal(t, model.BillingRecalcStatusMatched, result.Status, result.noteString())
	assert.Equal(t, 1000, result.RecalculatedQuota)
}
