package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// upstreamQuotaProvider encapsulates the upstream-specific quota endpoint and response format.
// Providers are resolved in registration order; the last provider is used as the fallback.
type upstreamQuotaProvider interface {
	Name() string
	Matches(baseURL string) bool
	UsageURL(baseURL string) string
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

func (deepSeekQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseDeepSeekBalanceResponse(body)
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
	for _, info := range typed.BalanceInfos {
		value, err := strconv.ParseFloat(info.TotalBalance, 64)
		if err != nil {
			return parsedUpstreamQuotaUsage{}, fmt.Errorf("deepseek total_balance %q invalid: %w", info.TotalBalance, err)
		}
		total += value
	}
	return parsedUpstreamQuotaUsage{raw: raw, balance: &total}, nil
}
