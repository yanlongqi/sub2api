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

func (deepSeekQuotaProvider) UsageURL(baseURL string) string {
	return trimUpstreamQuotaBaseURL(baseURL) + "/user/balance"
}

func (deepSeekQuotaProvider) AuthorizationHeader(apiKey string) string {
	return "Bearer " + apiKey
}

func (deepSeekQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseDeepSeekBalanceResponse(body)
}

// zhipuQuotaProvider 查询智谱 Coding Plan 配额（滚动窗口已用百分比）。
// 端点路径固定为 /api/monitor/usage/quota/limit，主机随 base_url 的域名路由：
// open.bigmodel.cn 与 api.z.ai 各自使用自己的主机。响应仅含已用百分比（5h + weekly），
// 无余额/绝对额度，故解析为双窗口快照。
type zhipuQuotaProvider struct{}

func (zhipuQuotaProvider) Name() string {
	return "zhipu"
}

func (zhipuQuotaProvider) Matches(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "bigmodel.cn" || strings.HasSuffix(host, ".bigmodel.cn") ||
		host == "z.ai" || strings.HasSuffix(host, ".z.ai")
}

func (zhipuQuotaProvider) UsageURL(baseURL string) string {
	return extractSchemeHost(baseURL) + "/api/monitor/usage/quota/limit"
}

func (zhipuQuotaProvider) AuthorizationHeader(apiKey string) string {
	// 智谱配额端点鉴权不加 Bearer 前缀（对齐 cc-switch query_zhipu）。
	return apiKey
}

func (zhipuQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseZhipuQuotaResponse(body)
}

// extractSchemeHost 从 base_url 提取 scheme://host（用于在域名上拼配额路径）。
func extractSchemeHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimRightSlash(baseURL)
	}
	return u.Scheme + "://" + u.Host
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
	return parsedUpstreamQuotaUsage{raw: raw, zhipu: zq}, nil
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
