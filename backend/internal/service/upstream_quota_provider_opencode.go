package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
)

// OpenCode Zen Go 订阅配额同步 provider。
//
// OpenCode Go 是订阅制服务（首月 $5 / 之后每月 $10），推理 base_url 形如
// https://opencode.ai/zen/go/v1（端点 /chat/completions、/messages、/responses）。
// 配额端点为同域数据面 GET https://opencode.ai/zen/go/v1/usage，
// Bearer 认证直接复用推理 api_key（无签名、无额外凭据，比火山简单得多）。
//
// 响应结构（官方文档未列出，源码 packages/console/app/src/routes/zen/go/v1/usage.ts）：
//
//	{"usage":{
//	  "rolling": {"status":"ok","percent":0,"resetsAt":"2026-08-19T20:34:26.643Z"},
//	  "weekly":  {"status":"ok","percent":0,"resetsAt":"2026-08-24T00:00:00.643Z"},
//	  "monthly": {"status":"ok","percent":0,"resetsAt":"2026-09-19T15:26:40.643Z"}}}
//
// rolling = 5 小时滚动窗口、weekly = 周窗口、monthly = 月窗口；
// percent 为已用百分比（0-100，美元额度口径：5h $12 / 周 $30 / 月 $60），
// resetsAt 为 ISO8601 重置时间。status 取 ok | rate-limited。
// 无 Go 订阅的 Key 返回 403 EntitlementError（由通用 http_error 分支记录）。
//
// 注意：Zen 主站按量付费（base_url https://opencode.ai/zen/v1）没有任何
// Key 认证的余额/用量端点（余额走控制台 OAuth 会话），Matches 仅匹配
// /zen/go 路径，主站账号回落 default sub2api provider（探测 404 → unsupported）。
const (
	openCodeZenGoUsagePathPrefix = "/zen/go"
	openCodeZenGoUsageURL        = "https://opencode.ai/zen/go/v1/usage"
)

// openCodeZenGoQuotaProvider 查询 OpenCode Zen Go 订阅配额（三窗口已用百分比）。
type openCodeZenGoQuotaProvider struct{}

func (openCodeZenGoQuotaProvider) Name() string {
	return "opencode"
}

func (openCodeZenGoQuotaProvider) Matches(baseURL string) bool {
	return openCodeZenGoBaseURLIsGo(baseURL)
}

func (openCodeZenGoQuotaProvider) UsageURL(baseURL string) string {
	_ = baseURL // 端点固定官方地址；baseURL 仅用于路径判断（见 Matches）
	return openCodeZenGoUsageURL
}

func (openCodeZenGoQuotaProvider) AuthorizationHeader(apiKey string) string {
	return "Bearer " + apiKey
}

func (openCodeZenGoQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseOpenCodeZenGoQuotaResponse(body)
}

// openCodeZenGoBaseURLIsGo 判断 base_url 是否为 OpenCode Zen Go 订阅端点：
// host 为 opencode.ai（或子域）且 path 以 /zen/go 开头。
// 主站按量付费（/zen/v1）不匹配。
func openCodeZenGoBaseURLIsGo(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "opencode.ai" && !strings.HasSuffix(host, ".opencode.ai") {
		return false
	}
	return strings.HasPrefix(strings.ToLower(u.Path), openCodeZenGoUsagePathPrefix)
}

// parseOpenCodeZenGoQuotaResponse 解析 GET /zen/go/v1/usage 响应。
// 输出统一为 parsedOpenCodeZenGoQuota（三窗口已用百分比 + 重置时间）。
func parseOpenCodeZenGoQuotaResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	// 错误响应：{"type":"error","error":{"type":"...","message":"..."}}
	if errType := gjson.GetBytes(body, "error.type"); errType.Exists() {
		msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if msg == "" {
			msg = "unknown opencode quota error"
		}
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("opencode quota error %s: %s", errType.String(), msg)
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("opencode quota response missing usage")
	}
	oq := &parsedOpenCodeZenGoQuota{}
	for _, tier := range parseOpenCodeZenGoTiers(usage) {
		switch tier.Window {
		case "5h":
			oq.FiveHourPercent, oq.FiveHourResetAt = tier.UsedPercent, tier.ResetAt
		case "weekly":
			oq.WeeklyPercent, oq.WeeklyResetAt = tier.UsedPercent, tier.ResetAt
		case "monthly":
			oq.MonthlyPercent, oq.MonthlyResetAt = tier.UsedPercent, tier.ResetAt
		}
	}
	if oq.FiveHourPercent <= 0 && oq.WeeklyPercent <= 0 && oq.MonthlyPercent <= 0 &&
		oq.FiveHourResetAt == "" && oq.WeeklyResetAt == "" && oq.MonthlyResetAt == "" {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("opencode quota response has no parseable windows")
	}
	return parsedUpstreamQuotaUsage{raw: raw, opencode: oq}, nil
}

// openCodeZenGoTier 表示 Zen Go 的一个用量窗口档位（百分比）。
type openCodeZenGoTier struct {
	Window      string  // "5h" | "weekly" | "monthly"
	UsedPercent float64
	ResetAt     string  // RFC3339
}

// parseOpenCodeZenGoTiers 解析 usage 对象的 rolling/weekly/monthly 三窗口。
func parseOpenCodeZenGoTiers(usage gjson.Result) []openCodeZenGoTier {
	var tiers []openCodeZenGoTier
	for _, key := range []struct {
		field  string
		window string
	}{
		{"rolling", "5h"},
		{"weekly", "weekly"},
		{"monthly", "monthly"},
	} {
		win := usage.Get(key.field)
		if !win.Exists() {
			continue
		}
		percent, valid := cnParseF64(win.Get("percent").Value())
		if !valid {
			continue
		}
		tiers = append(tiers, openCodeZenGoTier{
			Window:      key.window,
			UsedPercent: percent,
			ResetAt:     cnNormalizeResetTime(win.Get("resetsAt").Value()),
		})
	}
	return tiers
}
