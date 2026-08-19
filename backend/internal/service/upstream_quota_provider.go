package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// upstreamQuotaProvider encapsulates the upstream-specific quota endpoint and response format.
// Providers are resolved in registration order; the last provider is used as the fallback.
type upstreamQuotaProvider interface {
	Name() string
	Matches(baseURL string) bool
	UsageURL(baseURL string) string
	// AuthorizationHeader 返回该供应商配额端点的 Authorization 请求头值。
	// 多数供应商使用 "Bearer <key>"，智谱不加 Bearer 前缀。
	AuthorizationHeader(apiKey string) string
	ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error)
}

type upstreamQuotaProviderRegistry struct {
	providers []upstreamQuotaProvider
	fallback  upstreamQuotaProvider
}

func newUpstreamQuotaProviderRegistry(providers ...upstreamQuotaProvider) *upstreamQuotaProviderRegistry {
	registry := &upstreamQuotaProviderRegistry{}
	if len(providers) == 0 {
		registry.fallback = defaultSub2APIQuotaProvider{}
		return registry
	}
	registry.providers = providers[:len(providers)-1]
	registry.fallback = providers[len(providers)-1]
	return registry
}

func (r *upstreamQuotaProviderRegistry) Resolve(baseURL string) upstreamQuotaProvider {
	if r != nil {
		for _, provider := range r.providers {
			if provider != nil && provider.Matches(baseURL) {
				return provider
			}
		}
		if r.fallback != nil {
			return r.fallback
		}
	}
	return defaultSub2APIQuotaProvider{}
}

type defaultSub2APIQuotaProvider struct{}

func (defaultSub2APIQuotaProvider) Name() string {
	return "sub2api"
}

func (defaultSub2APIQuotaProvider) Matches(string) bool {
	return false
}

func (defaultSub2APIQuotaProvider) UsageURL(baseURL string) string {
	return trimUpstreamQuotaBaseURL(baseURL) + "/v1/usage"
}

func (defaultSub2APIQuotaProvider) AuthorizationHeader(apiKey string) string {
	return "Bearer " + apiKey
}

func (defaultSub2APIQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseUpstreamQuotaUsageResponse(body)
}

type deepSeekQuotaProvider struct{}

func (deepSeekQuotaProvider) Name() string {
	return "deepseek"
}

func (deepSeekQuotaProvider) Matches(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

const deepSeekQuotaUsageURL = "https://api.deepseek.com/user/balance"

func (deepSeekQuotaProvider) UsageURL(baseURL string) string {
	_ = baseURL // baseURL 仅用于域名判断，余额端点固定官方地址
	return deepSeekQuotaUsageURL
}

func (deepSeekQuotaProvider) AuthorizationHeader(apiKey string) string {
	return "Bearer " + apiKey
}

func (deepSeekQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseDeepSeekBalanceResponse(body)
}

// zhipuQuotaProvider 查询智谱 Coding Plan 配额（滚动窗口已用百分比）。
// baseURL 仅用于域名判断：open.bigmodel.cn → 国内站固定地址，api.z.ai → 国际站固定地址。
// 响应仅含已用百分比（5h + weekly），无余额/绝对额度，故解析为双窗口快照。
type zhipuQuotaProvider struct{}

func (zhipuQuotaProvider) Name() string {
	return "zhipu"
}

func (zhipuQuotaProvider) Matches(baseURL string) bool {
	return zhipuQuotaHostFromBaseURL(baseURL) != ""
}

func (zhipuQuotaProvider) UsageURL(baseURL string) string {
	return zhipuQuotaHostFromBaseURL(baseURL) + "/api/monitor/usage/quota/limit"
}

func (zhipuQuotaProvider) AuthorizationHeader(apiKey string) string {
	// 智谱配额端点鉴权不加 Bearer 前缀（对齐 cc-switch query_zhipu）。
	return apiKey
}

func (zhipuQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseZhipuQuotaResponse(body)
}

// zhipuQuotaHostFromBaseURL 只做域名判断，返回固定官方主机；未命中返回空串。
func zhipuQuotaHostFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "open.bigmodel.cn" || strings.HasSuffix(host, ".open.bigmodel.cn"):
		return "https://open.bigmodel.cn"
	case host == "api.z.ai" || strings.HasSuffix(host, ".api.z.ai"):
		return "https://api.z.ai"
	default:
		return ""
	}
}

// parseZhipuQuotaResponse 解析智谱 GET /api/monitor/usage/quota/limit 响应。
// 复用 parseZhipuTokenTiers 把 data.limits 归类为 5h / weekly 双窗口（百分比）。
func parseZhipuQuotaResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	if success := gjson.GetBytes(body, "success"); success.Exists() && !success.Bool() {
		msg := strings.TrimSpace(gjson.GetBytes(body, "msg").String())
		if msg == "" {
			msg = "unknown zhipu quota error"
		}
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("zhipu quota error: %s", msg)
	}
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("zhipu quota response missing data")
	}

	zq := &parsedZhipuQuota{
		Level: strings.TrimSpace(data.Get("level").String()),
	}
	for _, tier := range parseZhipuTokenTiers(data) {
		switch tier.Window {
		case "5h":
			zq.FiveHourPercent = tier.UsedPercent
			zq.FiveHourResetAt = tier.ResetAt
		case "weekly":
			zq.WeeklyPercent = tier.UsedPercent
			zq.WeeklyResetAt = tier.ResetAt
		}
	}
	// TIME_LIMIT：订阅周期额度（如 MCP 工具额度），带绝对值 usage/remaining 与
	// 周期重置时间。老套餐无 weekly 窗口时，这是除 5h 外唯一的第二档配额。
	if period, resetAt, ok := parseZhipuPeriodLimit(data); ok {
		zq.PeriodPercent = period
		zq.PeriodResetAt = resetAt
	}
	return parsedUpstreamQuotaUsage{raw: raw, zhipu: zq}, nil
}

// parseZhipuPeriodLimit 解析智谱响应中的 TIME_LIMIT 条目（订阅周期额度）。
// 返回已用百分比与重置时间；无该条目或字段缺失时 ok=false。
func parseZhipuPeriodLimit(data gjson.Result) (percent float64, resetAt string, ok bool) {
	var found gjson.Result
	data.Get("limits").ForEach(func(_, item gjson.Result) bool {
		if strings.ToUpper(strings.TrimSpace(item.Get("type").String())) == "TIME_LIMIT" {
			found = item
			return false // 取首条 TIME_LIMIT
		}
		return true
	})
	if !found.Exists() {
		return 0, "", false
	}
	p, valid := cnParseF64(found.Get("percentage").Value())
	if !valid {
		return 0, "", false
	}
	resetISO := ""
	if nr := found.Get("nextResetTime"); nr.Exists() {
		switch nr.Type {
		case gjson.Number:
			resetISO = cnMillisToRFC3339(nr.Int())
		case gjson.String:
			resetISO = cnNormalizeResetTime(nr.String())
		}
	}
	return p, resetISO, true
}

func trimUpstreamQuotaBaseURL(baseURL string) string {
	return trimRightSlash(baseURL)
}

func trimRightSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}

// parseDeepSeekBalanceResponse parses DeepSeek GET /user/balance responses.
func parseDeepSeekBalanceResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	var typed struct {
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &typed); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	if len(typed.BalanceInfos) == 0 {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("deepseek balance response has no balance_infos")
	}
	var total float64
	currency := ""
	for _, info := range typed.BalanceInfos {
		value, err := strconv.ParseFloat(info.TotalBalance, 64)
		if err != nil {
			return parsedUpstreamQuotaUsage{}, fmt.Errorf("deepseek total_balance %q invalid: %w", info.TotalBalance, err)
		}
		total += value
		if currency == "" && info.Currency != "" {
			currency = info.Currency
		}
	}
	return parsedUpstreamQuotaUsage{raw: raw, balance: &total, currency: currency}, nil
}
