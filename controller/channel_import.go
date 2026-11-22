package controller

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// 渠道文件级导入（T3 G6）。
//
// 现状：/api/channel 的 batch 模式只按换行拆密钥，base_url / models / group
// 由同一份模板复制；全树没有 encoding/csv，迁移或扩容只能人工点 N 次，
// 也没有导入前的影响面预览。这里补一条 CSV 导入通道：
//
//	POST /api/channel/import?dry_run=true   只解析与校验，返回影响面预览
//	POST /api/channel/import?dry_run=false  校验通过后整批写入
//
// 默认 dry_run=true：不显式关掉预览就不会落库。任一行校验失败则整批不写，
// 写入走 model.BatchInsertChannels（单事务，失败回滚），不会留下半批渠道。

const (
	channelImportMaxBytes = 4 << 20 // 4 MiB
	channelImportMaxRows  = 1000
)

// channelImportColumns 是 CSV 表头允许的列；name/type/key/models 必填，
// 其余列缺省时按渠道默认值处理（group 缺省为 default）。
var channelImportColumns = []string{"name", "type", "key", "models", "base_url", "group", "priority", "weight", "tag"}

var channelImportRequiredColumns = []string{"name", "type", "key", "models"}

type channelImportRow struct {
	Line     int // CSV 物理行号（表头为第 1 行）
	Name     string
	Type     int
	Key      string
	Models   string
	BaseURL  string
	Group    string
	Priority int64
	Weight   uint
	Tag      string
}

// channelImportError 是一条行级错误，Row 为 0 表示整表级错误（表头/编码）。
type channelImportError struct {
	Row     int    `json:"row"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type channelImportPreview struct {
	DryRun     bool                 `json:"dry_run"`
	Rows       int                  `json:"rows"`
	Channels   int                  `json:"channels"`
	Abilities  int                  `json:"abilities"`
	Importable bool                 `json:"importable"`
	Errors     []channelImportError `json:"errors"`
	Warnings   []string             `json:"warnings"`
	Models     []string             `json:"models,omitempty"`
	IDs        []int                `json:"ids,omitempty"`
}

// channelImportColumnIndex map[列名]列下标；缺表头与未知列都在这里报出。
func channelImportColumnIndex(header []string) (map[string]int, []channelImportError) {
	index := make(map[string]int, len(header))
	known := make(map[string]struct{}, len(channelImportColumns))
	for _, name := range channelImportColumns {
		known[name] = struct{}{}
	}
	errs := make([]channelImportError, 0)
	for column, raw := range header {
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff")))
		if name == "" {
			continue
		}
		if _, ok := known[name]; !ok {
			errs = append(errs, channelImportError{Field: name, Message: fmt.Sprintf("unknown column %q, allowed: %s", name, strings.Join(channelImportColumns, ","))})
			continue
		}
		if _, duplicate := index[name]; duplicate {
			errs = append(errs, channelImportError{Field: name, Message: fmt.Sprintf("column %q appears more than once", name)})
			continue
		}
		index[name] = column
	}
	for _, name := range channelImportRequiredColumns {
		if _, ok := index[name]; !ok {
			errs = append(errs, channelImportError{Field: name, Message: fmt.Sprintf("required column %q is missing, allowed: %s", name, strings.Join(channelImportColumns, ","))})
		}
	}
	return index, errs
}

// parseChannelImportCSV 只做结构解析（列、类型、行数上限），业务校验在
// buildChannelImportChannels 里用与单条新增相同的 validateChannel 完成。
func parseChannelImportCSV(r io.Reader) ([]channelImportRow, []channelImportError) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, []channelImportError{{Message: fmt.Sprintf("cannot read CSV header: %s", err.Error())}}
	}
	index, errs := channelImportColumnIndex(header)
	if len(errs) > 0 {
		return nil, errs
	}

	rows := make([]channelImportRow, 0)
	cell := func(record []string, name string) string {
		column, ok := index[name]
		if !ok || column >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[column])
	}

	for line := 2; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if parseErr, ok := err.(*csv.ParseError); ok {
				errs = append(errs, channelImportError{Row: parseErr.Line, Message: fmt.Sprintf("invalid CSV: %s", parseErr.Err)})
				continue
			}
			return rows, append(errs, channelImportError{Message: fmt.Sprintf("cannot read CSV: %s", err.Error())})
		}
		if len(strings.TrimSpace(strings.Join(record, ""))) == 0 {
			continue
		}
		if len(rows) >= channelImportMaxRows {
			errs = append(errs, channelImportError{Row: line, Message: fmt.Sprintf("too many rows, the limit is %d", channelImportMaxRows)})
			break
		}
		row := channelImportRow{
			Name:    cell(record, "name"),
			Key:     cell(record, "key"),
			Models:  cell(record, "models"),
			BaseURL: cell(record, "base_url"),
			Group:   cell(record, "group"),
			Tag:     cell(record, "tag"),
		}
		typeText := cell(record, "type")
		if typeText == "" {
			errs = append(errs, channelImportError{Row: line, Field: "type", Message: "type is required"})
		} else if channelType, convErr := strconv.Atoi(typeText); convErr != nil {
			errs = append(errs, channelImportError{Row: line, Field: "type", Message: fmt.Sprintf("type %q is not an integer channel type", typeText)})
		} else {
			row.Type = channelType
		}
		if priorityText := cell(record, "priority"); priorityText != "" {
			priority, convErr := strconv.ParseInt(priorityText, 10, 64)
			if convErr != nil {
				errs = append(errs, channelImportError{Row: line, Field: "priority", Message: fmt.Sprintf("priority %q is not an integer", priorityText)})
			} else {
				row.Priority = priority
			}
		}
		if weightText := cell(record, "weight"); weightText != "" {
			weight, convErr := strconv.ParseUint(weightText, 10, 32)
			if convErr != nil {
				errs = append(errs, channelImportError{Row: line, Field: "weight", Message: fmt.Sprintf("weight %q is not a non-negative integer", weightText)})
			} else {
				row.Weight = uint(weight)
			}
		}
		row.Line = line
		rows = append(rows, row)
	}
	return rows, errs
}

// channelAbilityRowCount 复算这条渠道会生成多少条能力行，口径与
// model.AddAbilities 一致（models × groups 去重）。
func channelAbilityRowCount(channel *model.Channel) int {
	seen := make(map[string]struct{})
	for _, m := range channel.GetModels() {
		for _, group := range channel.GetGroups() {
			seen[group+"|"+m] = struct{}{}
		}
	}
	return len(seen)
}

// buildChannelImportChannels 把解析后的行变成待写入渠道：逐行跑与单条新增
// 相同的 validateChannel，错误按行号聚合；任一行不通过则调用方整批不写。
func buildChannelImportChannels(rows []channelImportRow) ([]model.Channel, []channelImportError, []string) {
	channels := make([]model.Channel, 0, len(rows))
	errs := make([]channelImportError, 0)
	warnings := make([]string, 0)
	keyLines := make(map[string]int, len(rows))

	for _, row := range rows {
		lineErrs := 0
		fail := func(field, message string) {
			lineErrs++
			errs = append(errs, channelImportError{Row: row.Line, Field: field, Message: message})
		}
		if row.Name == "" {
			fail("name", "name is required")
		}
		if row.Key == "" {
			fail("key", "key is required")
		}
		if previous, exists := keyLines[row.Key]; exists && row.Key != "" {
			warnings = append(warnings, fmt.Sprintf("row %d: key is a duplicate of row %d; the same credential will be billed twice", row.Line, previous))
		}
		if row.Key != "" {
			if _, exists := keyLines[row.Key]; !exists {
				keyLines[row.Key] = row.Line
			}
		}

		group := row.Group
		if group == "" {
			group = "default"
		}
		channel := model.Channel{
			Name:        row.Name,
			Type:        row.Type,
			Key:         row.Key,
			Models:      row.Models,
			Group:       group,
			Status:      common.ChannelStatusEnabled,
			CreatedTime: common.GetTimestamp(),
		}
		if row.BaseURL != "" {
			baseURL := row.BaseURL
			channel.BaseURL = &baseURL
		}
		priority := row.Priority
		channel.Priority = &priority
		weight := row.Weight
		channel.Weight = &weight
		if row.Tag != "" {
			tag := row.Tag
			channel.Tag = &tag
		}

		// 与单条新增共用同一套校验：空模型列表、类型必填项、插件凭据格式等。
		if err := validateChannel(&channel, true); err != nil {
			fail("", err.Error())
		}
		if lineErrs > 0 {
			continue
		}
		for _, warning := range channelModelNameWarnings(&channel) {
			warnings = append(warnings, fmt.Sprintf("row %d: %s", row.Line, warning))
		}
		channels = append(channels, channel)
	}

	return channels, errs, warnings
}

// readChannelImportCSV 接受三种载荷：multipart 的 file 字段、JSON {"csv":"..."}、
// 或请求体直接就是 text/csv。大小上限 4 MiB。
func readChannelImportCSV(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, channelImportMaxBytes)
	contentType := c.ContentType()
	switch {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		file, err := c.FormFile("file")
		if err != nil {
			return nil, fmt.Errorf("multipart field \"file\" is required: %w", err)
		}
		opened, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		return io.ReadAll(opened)
	case strings.HasPrefix(contentType, "application/json"):
		var payload struct {
			CSV string `json:"csv"`
		}
		if err := c.ShouldBindJSON(&payload); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		if strings.TrimSpace(payload.CSV) == "" {
			return nil, fmt.Errorf("JSON field \"csv\" is empty")
		}
		return []byte(payload.CSV), nil
	default:
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil, fmt.Errorf("empty CSV body")
		}
		return raw, nil
	}
}

// ImportChannels 是 CSV 导入入口：默认只预览（dry_run=true），显式传
// dry_run=false 才整批写入；写入失败由 BatchInsertChannels 单事务回滚。
func ImportChannels(c *gin.Context) {
	dryRun := true
	if raw := strings.TrimSpace(c.Query("dry_run")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			common.ApiErrorT(c, "Invalid parameters: dry_run must be true or false")
			return
		}
		dryRun = parsed
	}

	raw, err := readChannelImportCSV(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	rows, parseErrs := parseChannelImportCSV(strings.NewReader(string(raw)))
	channels, buildErrs, warnings := buildChannelImportChannels(rows)

	abilities := 0
	modelSet := make(map[string]struct{})
	for index := range channels {
		abilities += channelAbilityRowCount(&channels[index])
		for _, name := range channels[index].GetModels() {
			modelSet[name] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}
	sort.Strings(models)

	errs := append(parseErrs, buildErrs...)
	if len(rows) == 0 && len(errs) == 0 {
		errs = append(errs, channelImportError{Message: "the CSV has no data rows"})
	}
	sort.SliceStable(errs, func(a, b int) bool { return errs[a].Row < errs[b].Row })

	preview := channelImportPreview{
		DryRun:     dryRun,
		Rows:       len(rows),
		Channels:   len(channels),
		Abilities:  abilities,
		Importable: len(errs) == 0,
		Errors:     errs,
		Warnings:   warnings,
		Models:     models,
	}

	if dryRun || len(errs) > 0 {
		common.ApiSuccess(c, preview)
		return
	}

	if len(channels) == 0 {
		common.ApiErrorT(c, "Nothing to import")
		return
	}
	if err := model.BatchInsertChannels(channels); err != nil {
		common.ApiError(c, err)
		return
	}

	preview.DryRun = false
	preview.IDs = make([]int, len(channels))
	for index := range channels {
		preview.IDs[index] = channels[index].Id
	}
	recordManageAudit(c, "channel.import", map[string]any{
		"count":     len(channels),
		"abilities": abilities,
		"models":    len(models),
	})
	common.ApiSuccess(c, preview)
}
