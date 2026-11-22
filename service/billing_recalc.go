package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/shopspring/decimal"
)

// G-04: historical ledger recalculation.
//
// A consume log records the per-item pricing factors (G-01 ratios, G-02
// pricing_version, G-03 expr hash, G-15 quota_per_unit, G-16 trust marker) so a
// historical charge can be replayed without re-reading today's price table.
// This file replays one log and compares the result with logs.quota.
//
// The replay deliberately reuses common/quota_math.go for every conversion from
// decimal/float to int (common.QuotaFromDecimalChecked / common.QuotaRoundChecked)
// — a recalculator with its own rounding would produce off-by-one differences
// that are impossible to defend in a billing dispute.

// Billing forms recognised by the recalculator. They map 1:1 to the settlement
// paths in service/text_quota.go and service/quota.go.
const (
	BillingFormTextToken  = "text_token"
	BillingFormTextFixed  = "text_fixed"
	BillingFormPerCall    = "per_call_fixed"
	BillingFormAudioToken = "audio_token"
	BillingFormAudioFixed = "audio_fixed"
	BillingFormTieredExpr = "tiered_expr"
	BillingFormTask       = "task"
)

// RecalcResult is the verdict for one log. Status is one of the
// model.BillingRecalcStatus* constants.
type RecalcResult struct {
	LogId             int      `json:"log_id"`
	UserId            int      `json:"user_id"`
	ModelName         string   `json:"model_name"`
	Group             string   `json:"group"`
	BillingForm       string   `json:"billing_form"`
	Status            string   `json:"status"`
	OriginalQuota     int      `json:"original_quota"`
	RecalculatedQuota int      `json:"recalculated_quota"`
	Delta             int      `json:"delta"`
	PricingVersion    string   `json:"pricing_version"`
	ExprHash          string   `json:"expr_hash"`
	QuotaPerUnit      float64  `json:"quota_per_unit"`
	Notes             []string `json:"notes"`
}

func (r *RecalcResult) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

func (r *RecalcResult) noteString() string {
	return strings.Join(r.Notes, "; ")
}

func (r *RecalcResult) finish() *RecalcResult {
	if r.Status != model.BillingRecalcStatusMatched && r.Status != model.BillingRecalcStatusDiff {
		return r
	}
	r.Delta = r.RecalculatedQuota - r.OriginalQuota
	if r.Delta == 0 {
		r.Status = model.BillingRecalcStatusMatched
	} else {
		r.Status = model.BillingRecalcStatusDiff
	}
	return r
}

func (r *RecalcResult) unsupported(format string, args ...any) *RecalcResult {
	r.Status = model.BillingRecalcStatusUnsupported
	r.note(format, args...)
	return r
}

func (r *RecalcResult) failed(err error) *RecalcResult {
	r.Status = model.BillingRecalcStatusError
	r.note("%s", err.Error())
	return r
}

// logOtherReader reads the public section of logs.other. The stored JSON is a
// flat map (public keys at the top level, privileged scopes under admin_info /
// root_info / audit_info), so only top-level keys are read here.
type logOtherReader map[string]any

func newLogOtherReader(raw string) (logOtherReader, error) {
	if strings.TrimSpace(raw) == "" {
		return logOtherReader{}, nil
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("logs.other is not valid JSON: %w", err)
	}
	return logOtherReader(values), nil
}

func (r logOtherReader) float(key string) (float64, bool) {
	value, ok := r[key]
	if !ok {
		return 0, false
	}
	result, ok := asFloat(value)
	return result, ok
}

func (r logOtherReader) floatOr(key string) float64 {
	value, _ := r.float(key)
	return value
}

func (r logOtherReader) intVal(key string) int {
	value, _ := r.float(key)
	return int(value)
}

func (r logOtherReader) str(key string) string {
	value, ok := r[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func (r logOtherReader) hasKey(key string) bool {
	_, ok := r[key]
	return ok
}

func (r logOtherReader) boolean(key string) bool {
	value, ok := r[key]
	if !ok {
		return false
	}
	flag, _ := value.(bool)
	return flag
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// otherRatios returns the per-item multipliers recorded by G-01. An absent map
// means the request had no extra multipliers (not that the data is missing).
func (r logOtherReader) otherRatios() map[string]float64 {
	value, ok := r["other_ratios"]
	if !ok {
		return nil
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	ratios := make(map[string]float64, len(raw))
	for key, item := range raw {
		if ratio, ok := asFloat(item); ok {
			ratios[key] = ratio
		}
	}
	if len(ratios) == 0 {
		return nil
	}
	return ratios
}

// floatMap reads a nested map of numeric values (billing_tokens).
func (r logOtherReader) floatMap(key string) (map[string]float64, bool) {
	value, ok := r[key]
	if !ok {
		return nil, false
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	values := make(map[string]float64, len(raw))
	for name, item := range raw {
		if number, ok := asFloat(item); ok {
			values[name] = number
		}
	}
	return values, true
}

// toolSurcharges reads the tool-call surcharge lines written by the text and
// audio settlement paths. Prices are the ones charged at the time, so the
// replay does not have to consult the (mutable) tool price setting.
func (r logOtherReader) toolSurcharges() []ToolSurchargeItem {
	value, ok := r["tool_surcharges"]
	if !ok {
		return nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	items := make([]ToolSurchargeItem, 0, len(raw))
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fields["name"].(string)
		count, _ := asFloat(fields["count"])
		price, _ := asFloat(fields["price"])
		items = append(items, ToolSurchargeItem{Name: name, Count: int(count), Price: price})
	}
	return items
}

func applyLoggedOtherRatios(value decimal.Decimal, ratios map[string]float64) decimal.Decimal {
	for _, ratio := range ratios {
		if ratio > 0 && !math.IsInf(ratio, 1) && ratio != 1.0 {
			value = value.Mul(decimal.NewFromFloat(ratio))
		}
	}
	return value
}

// toolSurchargeQuota mirrors calculateTextToolCallSurcharge using the prices
// frozen on the log.
func toolSurchargeQuota(items []ToolSurchargeItem, groupRatio float64, quotaPerUnit float64) decimal.Decimal {
	var surcharge decimal.Decimal
	for _, item := range items {
		surcharge = surcharge.Add(decimal.NewFromFloat(item.Price).
			Mul(decimal.NewFromInt(int64(item.Count))).
			Div(decimal.NewFromInt(1000)).
			Mul(decimal.NewFromFloat(groupRatio)).
			Mul(decimal.NewFromFloat(quotaPerUnit)))
	}
	return surcharge
}

// RecalculateConsumeLog replays one consume log. A non-nil error means the log
// itself could not be read (invalid JSON, negative id); pricing forms the
// recalculator refuses to guess at come back as status "unsupported" with the
// reason in Notes, never as a fabricated number.
func RecalculateConsumeLog(log *model.Log) (*RecalcResult, error) {
	if log == nil {
		return nil, fmt.Errorf("log is nil")
	}
	result := &RecalcResult{
		LogId:         log.Id,
		UserId:        log.UserId,
		ModelName:     log.ModelName,
		Group:         log.Group,
		OriginalQuota: log.Quota,
		Status:        model.BillingRecalcStatusMatched,
	}
	if log.Type != model.LogTypeConsume {
		return result.unsupported("log type %d is not a consume log", log.Type), nil
	}
	other, err := newLogOtherReader(log.Other)
	if err != nil {
		return result.failed(err), nil
	}
	if len(other) == 0 {
		return result.unsupported("logs.other is empty; the log predates the G-01/G-02/G-03/G-15 pricing snapshot"), nil
	}
	result.PricingVersion = other.str("pricing_version")
	result.QuotaPerUnit = other.floatOr("quota_per_unit")
	quotaPerUnit := result.QuotaPerUnit
	if quotaPerUnit <= 0 {
		// Older logs did not freeze the coefficient. It is only needed by the
		// forms that price in money (fixed price, tool surcharge, audio input),
		// so it is validated by those forms rather than here.
		result.note("logs.other has no quota_per_unit; money-denominated forms cannot be recomputed")
	}
	otherRatios := other.otherRatios()

	// Async task / video logs price from task usage facts at submit time and do
	// not carry a self-sufficient settlement snapshot. Refusing them is honest:
	// the alternative is to guess a quota for a form the log does not define.
	if other.boolean("is_task") {
		return result.unsupported("async task (is_task) billing is out of scope: task quota comes from task usage facts, not from a consume-log pricing snapshot"), nil
	}

	if other.str("billing_mode") == "tiered_expr" {
		result.BillingForm = BillingFormTieredExpr
		quota, err := recalcTieredExpr(log, other, result)
		if err != nil {
			return result.failed(err), nil
		}
		if quota < 0 && quota != 0 {
			// A conservative guard: quota is int and the clamp path returns a
			// non-negative value. A negative result would mean the replay is
			// producing nonsense, which must not be recorded as a diff.
			return result.failed(fmt.Errorf("tiered replay produced negative quota %d", quota)), nil
		}
		result.RecalculatedQuota = quota
		return result.finish(), nil
	}

	modelPrice := other.floatOr("model_price")
	isAudio := other.boolean("audio")
	usePrice := modelPrice != 0

	if isAudio {
		if usePrice {
			result.BillingForm = BillingFormAudioFixed
			if quotaPerUnit <= 0 {
				return result.unsupported("audio fixed-price log has no quota_per_unit (predates G-15)"), nil
			}
		} else {
			result.BillingForm = BillingFormAudioToken
		}
	} else if usePrice {
		if hasTextPricingMarkers(other) {
			result.BillingForm = BillingFormTextFixed
		} else {
			// Per-call billing (Midjourney / fixed-price image calls) never went
			// through textQuotaSummary, so the "no billable usage -> 0" rule
			// must not be applied to it.
			result.BillingForm = BillingFormPerCall
		}
		if quotaPerUnit <= 0 {
			return result.unsupported("fixed-price log has no quota_per_unit (predates G-15)"), nil
		}
	} else {
		result.BillingForm = BillingFormTextToken
	}

	var quota int
	if isAudio {
		quota, err = recalcAudioPath(log, other, result, otherRatios)
	} else {
		quota, err = recalcTextPath(log, other, result, otherRatios, usePrice, result.BillingForm != BillingFormPerCall)
	}
	if err != nil {
		return result.failed(err), nil
	}
	result.RecalculatedQuota = quota
	return result.finish(), nil
}

// recalcTextPath mirrors calculateTextQuotaSummary in service/text_quota.go.
// It reads the ratios from the log instead of from setting/ratio_setting, so it
// reproduces the pricing in force when the request was charged.
func recalcTextPath(log *model.Log, other logOtherReader, result *RecalcResult, otherRatios map[string]float64, usePrice bool, textPath bool) (int, error) {
	quotaPerUnit := result.QuotaPerUnit
	groupRatio := other.floatOr("group_ratio")
	modelRatio := other.floatOr("model_ratio")
	modelPrice := other.floatOr("model_price")
	completionRatio := other.floatOr("completion_ratio")
	cacheRatio := other.floatOr("cache_ratio")
	imageRatio := other.floatOr("image_ratio")

	promptTokens := log.PromptTokens
	completionTokens := log.CompletionTokens
	cacheTokens := other.intVal("cache_tokens")
	cacheCreationTokens := other.intVal("cache_creation_tokens")
	cacheCreationRatio := other.floatOr("cache_creation_ratio")
	cacheCreation5m := other.intVal("cache_creation_tokens_5m")
	cacheCreationRatio5m := other.floatOr("cache_creation_ratio_5m")
	cacheCreation1h := other.intVal("cache_creation_tokens_1h")
	cacheCreationRatio1h := other.floatOr("cache_creation_ratio_1h")
	cacheCreation12h := other.intVal("cache_creation_tokens_12h")
	cacheCreationRatio12h := other.floatOr("cache_creation_ratio_12h")
	imageTokens := other.intVal("image_output")
	audioTokens := other.intVal("audio_input_token_count")
	audioInputPrice := other.floatOr("audio_input_price")

	isClaude := other.str("usage_semantic") == "anthropic"
	// isLegacyClaudeDerivedOpenAIUsage: an OpenAI-format usage that carries the
	// Claude 5m/1h cache-write split without a usage semantic. The settlement
	// path treats it with Anthropic subtraction rules; the log preserves the
	// same evidence (split fields present, no semantic), so the replay can too.
	legacyClaudeDerived := !isClaude && (cacheCreation5m > 0 || cacheCreation1h > 0)

	toolItems := other.toolSurcharges()
	if (len(toolItems) > 0 || (audioTokens != 0 && audioInputPrice > 0)) && quotaPerUnit <= 0 {
		return 0, fmt.Errorf("tool surcharge / audio input pricing needs quota_per_unit, which this log does not record")
	}
	toolQuota := toolSurchargeQuota(toolItems, groupRatio, quotaPerUnit)

	dQuotaPerUnit := decimal.NewFromFloat(quotaPerUnit)
	dGroupRatio := decimal.NewFromFloat(groupRatio)
	dModelRatio := decimal.NewFromFloat(modelRatio)
	dModelPrice := decimal.NewFromFloat(modelPrice)
	dCompletionRatio := decimal.NewFromFloat(completionRatio)
	dCacheRatio := decimal.NewFromFloat(cacheRatio)
	dImageRatio := decimal.NewFromFloat(imageRatio)
	ratio := dModelRatio.Mul(dGroupRatio)

	var audioInputQuota decimal.Decimal
	if !usePrice {
		audioInputQuota = recalcAudioInputQuota(audioInputPrice, audioTokens, groupRatio, quotaPerUnit, dQuotaPerUnit)
	}

	var total decimal.Decimal
	if !usePrice {
		total = recalcTokenQuota(
			promptTokens, completionTokens,
			cacheTokens, cacheCreationTokens, cacheCreation5m, cacheCreation1h, cacheCreation12h,
			imageTokens, audioTokens,
			isClaude, legacyClaudeDerived,
			dCacheRatio, dImageRatio, dCompletionRatio,
			dCacheCreationRatioOf(cacheCreationRatio, other.hasKey("cache_creation_ratio")),
			dCacheCreationRatioOf(cacheCreationRatio5m, other.hasKey("cache_creation_ratio_5m")),
			dCacheCreationRatioOf(cacheCreationRatio1h, other.hasKey("cache_creation_ratio_1h")),
			dCacheCreationRatioOf(cacheCreationRatio12h, other.hasKey("cache_creation_ratio_12h")),
			ratio, audioInputQuota,
		)
	} else {
		total = dModelPrice.Mul(dQuotaPerUnit).Mul(dGroupRatio)
	}
	total = applyLoggedOtherRatios(total, otherRatios)
	total = total.Add(toolQuota)
	if !usePrice {
		if !ratio.IsZero() && total.LessThanOrEqual(decimal.Zero) {
			total = decimal.NewFromInt(1)
		}
	}

	quota, clamp := common.QuotaFromDecimalChecked(total)
	if clamp != nil {
		result.note("quota saturation during replay: %s", clamp.Error())
	}

	// Mirrors textQuotaSummary.hasBillableUsage: a request with no billable
	// tokens (and no cache/tool contribution) settles at 0 even when the model
	// has a price. usage_missing logs are not zero: G-09 charged the estimated
	// prompt tokens, which are what the log records.
	if textPath && !recalcTextHasBillableUsage(log, cacheTokens, cacheCreationTokens, cacheCreation5m, cacheCreation1h, isClaude, toolQuota) {
		result.note("no billable usage on this log; settlement charged 0")
		quota = 0
	}
	if other.boolean("usage_missing") {
		result.note("usage_missing: the original charge was computed from estimated prompt tokens")
	}
	return quota, nil
}

// dCacheCreationRatioOf distinguishes "not recorded" from a configured 0:
// settlement defaults the cache-write ratio to 1 (no premium), but a 0 on the
// log is a legitimate free-write configuration and must be honoured.
func dCacheCreationRatioOf(ratio float64, present bool) decimal.Decimal {
	if !present {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromFloat(ratio)
}

func recalcAudioInputQuota(audioInputPrice float64, audioTokens int, groupRatio float64, quotaPerUnit float64, dQuotaPerUnit decimal.Decimal) decimal.Decimal {
	if audioTokens == 0 || audioInputPrice <= 0 {
		return decimal.Zero
	}
	return decimal.NewFromFloat(audioInputPrice).
		Div(decimal.NewFromInt(1000000)).
		Mul(decimal.NewFromInt(int64(audioTokens))).
		Mul(decimal.NewFromFloat(groupRatio)).
		Mul(dQuotaPerUnit)
}

// recalcTokenQuota is the token-pricing branch of calculateTextQuotaSummary,
// parameterised by the values frozen on the log.
func recalcTokenQuota(
	promptTokens, completionTokens int,
	cacheTokens, cacheCreationTokens, cacheCreation5m, cacheCreation1h, cacheCreation12h int,
	imageTokens, audioTokens int,
	isClaude, legacyClaudeDerived bool,
	dCacheRatio, dImageRatio, dCompletionRatio decimal.Decimal,
	dCacheCreationRatio, dCacheCreationRatio5m, dCacheCreationRatio1h, dCacheCreationRatio12h decimal.Decimal,
	ratio decimal.Decimal,
	audioInputQuota decimal.Decimal,
) decimal.Decimal {
	baseTokens := decimal.NewFromInt(int64(promptTokens))
	dCacheTokens := decimal.NewFromInt(int64(cacheTokens))
	dImageTokens := decimal.NewFromInt(int64(imageTokens))
	dAudioTokens := decimal.NewFromInt(int64(audioTokens))
	dCompletionTokens := decimal.NewFromInt(int64(completionTokens))
	dCacheCreationTokens := decimal.NewFromInt(int64(cacheCreationTokens))

	var cachedTokensWithRatio decimal.Decimal
	if !dCacheTokens.IsZero() {
		if !isClaude && !legacyClaudeDerived {
			baseTokens = baseTokens.Sub(dCacheTokens)
		}
		cachedTokensWithRatio = dCacheTokens.Mul(dCacheRatio)
	}

	var cachedCreationTokensWithRatio decimal.Decimal
	hasSplitCacheCreationTokens := cacheCreation5m > 0 || cacheCreation1h > 0
	if !dCacheCreationTokens.IsZero() || hasSplitCacheCreationTokens {
		if !isClaude && !legacyClaudeDerived {
			baseTokens = baseTokens.Sub(dCacheCreationTokens)
			dCacheCreationTokens12h := decimal.NewFromInt(int64(cacheCreation12h))
			cachedCreationTokensWithRatio = dCacheCreationTokens.Sub(dCacheCreationTokens12h).Mul(dCacheCreationRatio)
			cachedCreationTokensWithRatio = cachedCreationTokensWithRatio.Add(dCacheCreationTokens12h.Mul(dCacheCreationRatio12h))
		} else {
			remaining := cacheCreationTokens - cacheCreation5m - cacheCreation1h
			if remaining < 0 {
				remaining = 0
			}
			cachedCreationTokensWithRatio = decimal.NewFromInt(int64(remaining)).Mul(dCacheCreationRatio)
			cachedCreationTokensWithRatio = cachedCreationTokensWithRatio.Add(decimal.NewFromInt(int64(cacheCreation5m)).Mul(dCacheCreationRatio5m))
			cachedCreationTokensWithRatio = cachedCreationTokensWithRatio.Add(decimal.NewFromInt(int64(cacheCreation1h)).Mul(dCacheCreationRatio1h))
		}
	}

	var imageTokensWithRatio decimal.Decimal
	if !dImageTokens.IsZero() {
		baseTokens = baseTokens.Sub(dImageTokens)
		imageTokensWithRatio = dImageTokens.Mul(dImageRatio)
	}

	if !dAudioTokens.IsZero() && !audioInputQuota.IsZero() {
		baseTokens = baseTokens.Sub(dAudioTokens)
	}

	if baseTokens.IsNegative() {
		baseTokens = decimal.Zero
	}

	promptQuota := baseTokens.Add(cachedTokensWithRatio).Add(imageTokensWithRatio).Add(cachedCreationTokensWithRatio)
	completionQuota := dCompletionTokens.Mul(dCompletionRatio)
	total := promptQuota.Add(completionQuota).Mul(ratio)
	return total.Add(audioInputQuota)
}

// hasTextPricingMarkers reports whether the log came from the text settlement
// path. That path always writes model_ratio/completion_ratio/frt, while the
// per-call (Midjourney) path writes only model_price + group_ratio.
func hasTextPricingMarkers(other logOtherReader) bool {
	for _, key := range []string{"model_ratio", "completion_ratio", "cache_ratio", "frt", "usage_semantic"} {
		if other.hasKey(key) {
			return true
		}
	}
	return false
}

func recalcTextHasBillableUsage(log *model.Log, cacheTokens, cacheCreationTokens, cacheCreation5m, cacheCreation1h int, isClaude bool, toolQuota decimal.Decimal) bool {
	if log.PromptTokens+log.CompletionTokens > 0 {
		return true
	}
	if isClaude && (cacheTokens > 0 || cacheCreationTokens > 0 || cacheCreation5m > 0 || cacheCreation1h > 0) {
		return true
	}
	return !toolQuota.IsZero()
}

// recalcAudioPath mirrors calculateAudioQuota (both branches) using the ratios
// frozen on the log.
func recalcAudioPath(log *model.Log, other logOtherReader, result *RecalcResult, otherRatios map[string]float64) (int, error) {
	groupRatio := other.floatOr("group_ratio")
	modelRatio := other.floatOr("model_ratio")
	modelPrice := other.floatOr("model_price")
	completionRatio := other.floatOr("completion_ratio")
	audioRatio := other.floatOr("audio_ratio")
	audioCompletionRatio := other.floatOr("audio_completion_ratio")

	textInput := other.intVal("text_input")
	textOutput := other.intVal("text_output")
	audioInput := other.intVal("audio_input")
	audioOutput := other.intVal("audio_output")

	var total decimal.Decimal
	usePrice := modelPrice != 0
	if usePrice {
		total = decimal.NewFromFloat(modelPrice).
			Mul(decimal.NewFromFloat(result.QuotaPerUnit)).
			Mul(decimal.NewFromFloat(groupRatio))
	} else {
		ratio := decimal.NewFromFloat(groupRatio).Mul(decimal.NewFromFloat(modelRatio))
		total = decimal.NewFromInt(int64(textInput))
		total = total.Add(decimal.NewFromInt(int64(textOutput)).Mul(decimal.NewFromFloat(completionRatio)))
		total = total.Add(decimal.NewFromInt(int64(audioInput)).Mul(decimal.NewFromFloat(audioRatio)))
		total = total.Add(decimal.NewFromInt(int64(audioOutput)).
			Mul(decimal.NewFromFloat(audioRatio)).
			Mul(decimal.NewFromFloat(audioCompletionRatio)))
		total = total.Mul(ratio)
		if !ratio.IsZero() && total.LessThanOrEqual(decimal.Zero) {
			total = decimal.NewFromInt(1)
		}
	}

	quota, clamp := common.QuotaFromDecimalChecked(total)
	if clamp != nil {
		result.note("quota saturation during replay: %s", clamp.Error())
	}

	// PostAudioConsumeQuota settles a zero-token request at 0 unless a fixed
	// price expression billed it (tiered is handled before this function).
	if log.PromptTokens+log.CompletionTokens == 0 {
		result.note("no tokens on this audio log; settlement charged 0")
		quota = 0
	}
	return quota, nil
}

// recalcTieredExpr replays a tiered_expr charge. The expression string and its
// hash are frozen on the log, so the replay runs the same program the charge
// ran. Group ratio and quota_per_unit come from the log, not from today's
// settings.
func recalcTieredExpr(log *model.Log, other logOtherReader, result *RecalcResult) (int, error) {
	exprB64 := other.str("expr_b64")
	if exprB64 == "" {
		return 0, fmt.Errorf("tiered_expr log has no expr_b64")
	}
	exprBytes, err := base64.StdEncoding.DecodeString(exprB64)
	if err != nil {
		return 0, fmt.Errorf("expr_b64 is not valid base64: %w", err)
	}
	expr := string(exprBytes)
	hash := billingexpr.ExprHashString(expr)
	loggedHash := other.str("expr_hash")
	if loggedHash != "" && loggedHash != hash {
		return 0, fmt.Errorf("expr_hash %s does not match expr_b64 (%s); the expression snapshot is corrupt", loggedHash, hash)
	}
	result.ExprHash = hash

	quotaPerUnit := result.QuotaPerUnit
	if quotaPerUnit <= 0 {
		quotaPerUnit = common.QuotaPerUnit
		result.note("log has no quota_per_unit; replay used the current global coefficient %g", quotaPerUnit)
	}

	// Expression-level request multipliers were evaluated against the request
	// body, which the log does not keep. A matched multiplier cannot be
	// reproduced from the log alone.
	if rules, err := otherRequestRules(other); err == nil {
		for _, rule := range rules {
			if rule.Matched && rule.Multiplier != 1 {
				return 0, fmt.Errorf("expression uses request-dependent multiplier %q (x%g); the request body is not in the log, so the tiered charge cannot be replayed", rule.Cond, rule.Multiplier)
			}
		}
	}

	version := other.intVal("expr_version")
	if version == 0 {
		version = billingexpr.ExprVersion(expr)
	}
	snap := &billingexpr.BillingSnapshot{
		BillingMode:   "tiered_expr",
		ModelName:     log.ModelName,
		ExprString:    expr,
		ExprHash:      hash,
		GroupRatio:    other.floatOr("group_ratio"),
		QuotaPerUnit:  quotaPerUnit,
		ExprVersion:   version,
		EstimatedTier: other.str("estimated_tier"),
	}

	params, notes, err := rebuildTieredTokenParams(log, other, expr, hash)
	if err != nil {
		return 0, err
	}
	result.Notes = append(result.Notes, notes...)

	requestInput := billingexpr.RequestInput{}
	if _, ok := other.float("image_count"); ok {
		imageCount := other.intVal("image_count")
		requestInput.ImageCount = &imageCount
	}

	tiered, err := billingexpr.ComputeTieredQuotaWithRequest(snap, params, requestInput)
	if err != nil {
		return 0, fmt.Errorf("tiered expression replay failed: %w", err)
	}
	if tiered.Clamp != nil {
		result.note("quota saturation during tiered replay: %s", tiered.Clamp.Error())
	}
	if tiered.MatchedTier != "" {
		result.note("matched tier %q", tiered.MatchedTier)
	}

	// Mirror composeTieredTextQuota: a tool surcharge is added to the
	// pre-group tiered amount and saturated once.
	surcharge := toolSurchargeQuota(other.toolSurcharges(), snap.GroupRatio, quotaPerUnit)
	if surcharge.IsZero() {
		return tiered.ActualQuotaAfterGroup, nil
	}
	total, clamp := common.QuotaFromDecimalChecked(
		decimal.NewFromFloat(tiered.ActualQuotaBeforeGroup).
			Mul(decimal.NewFromFloat(snap.GroupRatio)).
			Add(surcharge),
	)
	if clamp != nil {
		result.note("quota saturation during surcharge replay: %s", clamp.Error())
	}
	return total, nil
}

func otherRequestRules(other logOtherReader) ([]billingexpr.RequestRuleTrace, error) {
	value, ok := other["request_rules"]
	if !ok {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("request_rules is not a list")
	}
	rules := make([]billingexpr.RequestRuleTrace, 0, len(raw))
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		cond, _ := fields["cond"].(string)
		multiplier, _ := asFloat(fields["multiplier"])
		matched, _ := fields["matched"].(bool)
		rules = append(rules, billingexpr.RequestRuleTrace{Cond: cond, Multiplier: multiplier, Matched: matched})
	}
	return rules, nil
}

// rebuildTieredTokenParams reconstructs the token parameters the tiered
// settlement ran with. billing_tokens (written when the expression prices
// image cache reads separately) is exact; otherwise the aggregates on the log
// are enough for every dimension except img_cr / img_o / ao, which are refused
// rather than approximated.
func rebuildTieredTokenParams(log *model.Log, other logOtherReader, expr, hash string) (billingexpr.TokenParams, []string, error) {
	usedVars := billingexpr.UsedVarsByHash(expr, hash)

	var notes []string
	if tokens, ok := other.floatMap("billing_tokens"); ok && len(tokens) > 0 {
		notes = append(notes, "tiered parameters read from the logged billing_tokens snapshot")
		return billingexpr.TokenParams{
			P: tokens["p"], C: tokens["c"], Len: tokens["len"],
			CR: tokens["cr"], CC: tokens["cc"], CC1h: tokens["cc1h"], CC12h: tokens["cc12h"],
			Img: tokens["img"], ImgCR: tokens["img_cr"], ImgO: tokens["img_o"],
			AI: tokens["ai"], AO: tokens["ao"],
		}, notes, nil
	}

	if usedVars["img_cr"] {
		return billingexpr.TokenParams{}, nil, fmt.Errorf("expression prices image cache reads (img_cr) but the log has no billing_tokens snapshot; the cached-image breakdown was never recorded")
	}
	if usedVars["img_o"] {
		return billingexpr.TokenParams{}, nil, fmt.Errorf("expression prices image output tokens (img_o), which the consume log does not record")
	}

	isClaude := other.str("usage_semantic") == "anthropic"
	p := float64(log.PromptTokens)
	c := float64(log.CompletionTokens)
	cr := float64(other.intVal("cache_tokens"))
	ccTotal := other.intVal("cache_creation_tokens")
	cc5m := other.intVal("cache_creation_tokens_5m")
	cc1h := other.intVal("cache_creation_tokens_1h")
	cc12h := other.intVal("cache_creation_tokens_12h")
	img := float64(other.intVal("image_output"))
	ai := float64(other.intVal("audio_input_token_count"))
	if ai == 0 {
		ai = float64(other.intVal("audio_input"))
	}
	ao := float64(other.intVal("audio_output"))

	cc := float64(ccTotal)
	cc5 := float64(0)
	cc1 := float64(0)
	if isClaude {
		// NormalizeCacheCreationSplit: an unlabelled remainder is billed as 5m.
		split1h := cc1h
		split5m := cc5m
		if ccTotal > split5m+split1h {
			split5m = ccTotal - split1h
		}
		cc5 = float64(split5m)
		cc1 = float64(split1h)
		cc = cc5
		cc12h = 0
	}

	inputLen := p
	if isClaude {
		inputLen = p + cr + cc + cc1
	}

	if isClaude {
		if !usedVars["cr"] {
			p += cr
		}
		if !usedVars["cc1h"] {
			cc += cc1 * hosttypes.ClaudeCacheCreation1hMultiplier
			cc1 = 0
		}
		if !usedVars["cc"] {
			p += cc
			cc = 0
		}
	} else {
		if usedVars["cr"] {
			p -= cr
		}
		cc12 := float64(cc12h)
		cc -= cc12
		ccTokens := cc
		if usedVars["cc12h"] {
			p -= cc12
		} else {
			ccTokens += cc12
			cc += cc12 * hosttypes.OpenAICacheCreation12hMultiplier
			cc12 = 0
		}
		if usedVars["cc"] {
			p -= ccTokens
		} else {
			p += cc - ccTokens
			cc = 0
		}
		if usedVars["cc1h"] {
			p -= cc1
		}
		if usedVars["img"] {
			p -= img
		}
		if usedVars["ai"] {
			p -= ai
		}
		if usedVars["ao"] && ao == 0 {
			return billingexpr.TokenParams{}, nil, fmt.Errorf("expression prices audio output tokens (ao), which the consume log does not record")
		}
		if usedVars["ao"] {
			c -= ao
		}
	}
	if p < 0 {
		p = 0
	}
	if c < 0 {
		c = 0
	}

	return billingexpr.TokenParams{
		P: p, C: c, Len: inputLen,
		CR: cr, CC: cc, CC1h: cc1, CC12h: float64(cc12h),
		Img: img, ImgCR: 0, ImgO: 0, AI: ai, AO: ao,
	}, notes, nil
}

// RecalculateAndRecordConsumeLog replays a log and persists the verdict as a
// price-difference row.
func RecalculateAndRecordConsumeLog(log *model.Log) (*RecalcResult, error) {
	result, err := RecalculateConsumeLog(log)
	if err != nil {
		return nil, err
	}
	row := &model.BillingRecalcDiff{
		LogId:             result.LogId,
		UserId:            result.UserId,
		ModelName:         result.ModelName,
		Group:             result.Group,
		BillingForm:       result.BillingForm,
		Status:            result.Status,
		OriginalQuota:     result.OriginalQuota,
		RecalculatedQuota: result.RecalculatedQuota,
		Delta:             result.Delta,
		PricingVersion:    result.PricingVersion,
		ExprHash:          result.ExprHash,
		QuotaPerUnit:      result.QuotaPerUnit,
		Note:              result.noteString(),
	}
	if err := model.UpsertBillingRecalcDiff(row); err != nil {
		return result, fmt.Errorf("failed to record recalculation: %w", err)
	}
	return result, nil
}

// RecalculateConsumeLogById loads one consume log and records the verdict.
func RecalculateConsumeLogById(logId int) (*RecalcResult, error) {
	log, err := model.GetConsumeLogById(logId)
	if err != nil {
		return nil, err
	}
	return RecalculateAndRecordConsumeLog(log)
}
