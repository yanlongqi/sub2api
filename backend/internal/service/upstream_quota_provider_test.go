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

// 国产供应商原生平台账号走同一套上游配额同步：base URL 解析复用
// GetOpenAIBaseURL（凭证 base_url / adaptive 分协议地址 / 平台默认）。
// 智谱/DeepSeek 的配额/余额由原生 CN 服务负责，同步通道域名未命中
// 官方域时回落 sub2api fallback，此处仅验证 base 解析。
func TestUpstreamQuotaSyncBaseURLSupportsCNPlatforms(t *testing.T) {
	withBaseURL := func(platform string) *Account {
		return &Account{
			Platform: platform,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_key": "sk-test",
				"base_url": "https://relay.example.com/v1",
			},
		}
	}

	// 凭证 base_url 优先（自适应/中转地址），域名未命中官方域时回落 sub2api fallback。
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOpenAI} {
		account := withBaseURL(platform)
		require.Equal(t, "https://relay.example.com/v1", upstreamQuotaSyncBaseURL(account), platform)
	}

	// 无凭证 base_url 时按平台默认端点解析（zhipu 配额由原生 CNProviderQuotaService
	// 负责，不再走同步上游通道，仅验证 base 解析）。
	zhipuDefault := &Account{
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"account_mode": AccountModeCoding,
		},
	}
	require.Equal(t, DefaultZhipuCodingBaseURL, upstreamQuotaSyncBaseURL(zhipuDefault))

	deepseekDefault := &Account{
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}
	require.Equal(t, DefaultDeepseekBaseURL, upstreamQuotaSyncBaseURL(deepseekDefault))
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
