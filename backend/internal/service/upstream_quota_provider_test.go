//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type testUpstreamQuotaProvider struct {
	name    string
	matches bool
}

func (p testUpstreamQuotaProvider) Name() string {
	return p.name
}

func (p testUpstreamQuotaProvider) Matches(string) bool {
	return p.matches
}

func (p testUpstreamQuotaProvider) UsageURL(baseURL string) string {
	return baseURL
}

func (p testUpstreamQuotaProvider) AuthorizationHeader(apiKey string) string {
	return "Bearer " + apiKey
}

func (p testUpstreamQuotaProvider) ParseResponse([]byte) (parsedUpstreamQuotaUsage, error) {
	return parsedUpstreamQuotaUsage{}, nil
}

func TestUpstreamQuotaProviderRegistryUsesFirstMatchingProvider(t *testing.T) {
	registry := newUpstreamQuotaProviderRegistry(
		testUpstreamQuotaProvider{name: "first", matches: true},
		testUpstreamQuotaProvider{name: "second", matches: true},
	)

	provider := registry.Resolve("https://upstream.example.com")

	require.Equal(t, "first", provider.Name())
}

func TestUpstreamQuotaProviderRegistryFallsBackToDefaultProvider(t *testing.T) {
	defaultProvider := testUpstreamQuotaProvider{name: "default"}
	registry := newUpstreamQuotaProviderRegistry(defaultProvider)

	provider := registry.Resolve("https://upstream.example.com")

	require.Equal(t, defaultProvider, provider)
}

func TestDeepSeekQuotaProviderMatchesOnlyDeepSeekDomains(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	require.True(t, provider.Matches("https://api.deepseek.com"))
	require.True(t, provider.Matches("https://tenant.deepseek.com/v1"))
	require.False(t, provider.Matches("https://deepseek.com.evil.example"))
	require.False(t, provider.Matches("https://deepseek.example.com"))
}

func TestDeepSeekQuotaProviderUsesFixedBalanceURL(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	require.Equal(t, "https://api.deepseek.com/user/balance", provider.UsageURL("https://tenant.deepseek.com/custom/path"))
}

func TestDeepSeekQuotaProviderParsesAllBalances(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	parsed, err := provider.ParseResponse([]byte(`{
		"is_available": true,
		"balance_infos": [
			{"currency": "CNY", "total_balance": "5.000000"},
			{"currency": "USD", "total_balance": "2.5"}
		]
	}`))

	require.NoError(t, err)
	require.NotNil(t, parsed.balance)
	require.InDelta(t, 7.5, *parsed.balance, 0.000001)
	require.Equal(t, "CNY", parsed.currency)
}

func TestDeepSeekQuotaProviderRejectsMissingOrInvalidBalance(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	_, err := provider.ParseResponse([]byte(`{"balance_infos": []}`))
	require.Error(t, err)

	_, err = provider.ParseResponse([]byte(`{"balance_infos":[{"total_balance":"not-a-number"}]}`))
	require.Error(t, err)
}

func TestParseUpstreamQuotaUsageResponseExtractsBalanceAndUnit(t *testing.T) {
	parsed, err := parseUpstreamQuotaUsageResponse([]byte(`{
		"mode": "unrestricted",
		"isValid": true,
		"planName": "钱包余额",
		"remaining": 28.2,
		"unit": "CNY",
		"balance": 28.2
	}`))

	require.NoError(t, err)
	require.NotNil(t, parsed.balance)
	require.InDelta(t, 28.2, *parsed.balance, 0.000001)
	require.Equal(t, "CNY", parsed.currency)
}

func TestNormalizeUpstreamQuotaCurrencyDefaultsToCNY(t *testing.T) {
	require.Equal(t, "CNY", normalizeUpstreamQuotaCurrency(""))
	require.Equal(t, "CNY", normalizeUpstreamQuotaCurrency("  "))
	require.Equal(t, "USD", normalizeUpstreamQuotaCurrency("usd"))
	require.Equal(t, "CNY", normalizeUpstreamQuotaCurrency("cny"))
}

func TestZhipuQuotaProviderMatchesOnlyOfficialDomains(t *testing.T) {
	provider := zhipuQuotaProvider{}

	require.True(t, provider.Matches("https://open.bigmodel.cn/api/paas/v4"))
	require.True(t, provider.Matches("https://api.z.ai/api/paas/v4"))
	require.False(t, provider.Matches("https://bigmodel.cn"))
	require.False(t, provider.Matches("https://z.ai"))
	require.False(t, provider.Matches("https://evilbigmodel.cn"))
	require.False(t, provider.Matches("https://open.bigmodel.cn.evil.example"))
	require.False(t, provider.Matches("https://api.deepseek.com"))
	require.False(t, provider.Matches("https://example.com"))
}

func TestZhipuQuotaProviderUsesFixedOfficialURLs(t *testing.T) {
	provider := zhipuQuotaProvider{}

	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit", provider.UsageURL("https://open.bigmodel.cn/custom/path"))
	require.Equal(t, "https://api.z.ai/api/monitor/usage/quota/limit", provider.UsageURL("https://api.z.ai/custom/path"))
}

func TestZhipuQuotaProviderAuthorizationHeaderOmitsBearer(t *testing.T) {
	provider := zhipuQuotaProvider{}
	require.Equal(t, "sk-123", provider.AuthorizationHeader("sk-123"))
}

func TestZhipuQuotaProviderParsesTwoWindows(t *testing.T) {
	provider := zhipuQuotaProvider{}

	parsed, err := provider.ParseResponse([]byte(`{
		"success": true,
		"data": {
			"level": "pro",
			"limits": [
				{"type": "TOKENS_LIMIT", "unit": 3, "number": 5, "percentage": 26.0, "nextResetTime": 1774967594803},
				{"type": "TOKENS_LIMIT", "unit": 6, "number": 1, "percentage": 5.0, "nextResetTime": 1780143594803}
			]
		}
	}`))

	require.NoError(t, err)
	require.NotNil(t, parsed.zhipu)
	require.Equal(t, "pro", parsed.zhipu.Level)
	require.InDelta(t, 26.0, parsed.zhipu.FiveHourPercent, 0.000001)
	require.NotEmpty(t, parsed.zhipu.FiveHourResetAt)
	require.InDelta(t, 5.0, parsed.zhipu.WeeklyPercent, 0.000001)
	require.NotEmpty(t, parsed.zhipu.WeeklyResetAt)
}

func TestZhipuQuotaProviderParsesPeriodLimit(t *testing.T) {
	provider := zhipuQuotaProvider{}

	// 老套餐形态：TIME_LIMIT（订阅周期额度）+ 单条 TOKENS_LIMIT（5h）。
	parsed, err := provider.ParseResponse([]byte(`{
		"success": true,
		"data": {
			"level": "max",
			"limits": [
				{"type": "TIME_LIMIT", "unit": 5, "number": 1, "usage": 4000, "remaining": 3890, "percentage": 2, "currentValue": 110, "nextResetTime": 1789207208997},
				{"type": "TOKENS_LIMIT", "unit": 3, "number": 5, "percentage": 34, "nextResetTime": 1787136499883}
			]
		}
	}`))

	require.NoError(t, err)
	require.NotNil(t, parsed.zhipu)
	require.Equal(t, "max", parsed.zhipu.Level)
	require.InDelta(t, 34.0, parsed.zhipu.FiveHourPercent, 0.000001)
	require.InDelta(t, 2.0, parsed.zhipu.PeriodPercent, 0.000001)
	require.NotEmpty(t, parsed.zhipu.PeriodResetAt)
	require.Equal(t, 0.0, parsed.zhipu.WeeklyPercent)
}

func TestZhipuQuotaProviderRejectsBusinessError(t *testing.T) {
	provider := zhipuQuotaProvider{}

	_, err := provider.ParseResponse([]byte(`{"success": false, "msg": "invalid api key"}`))
	require.Error(t, err)
}

func TestZhipuQuotaProviderRejectsMissingData(t *testing.T) {
	provider := zhipuQuotaProvider{}

	_, err := provider.ParseResponse([]byte(`{"success": true}`))
	require.Error(t, err)
}
