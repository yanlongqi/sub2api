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

func TestDeepSeekQuotaProviderBuildsBalanceURL(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	require.Equal(t, "https://api.deepseek.com/user/balance", provider.UsageURL("https://api.deepseek.com/"))
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
}

func TestDeepSeekQuotaProviderRejectsMissingOrInvalidBalance(t *testing.T) {
	provider := deepSeekQuotaProvider{}

	_, err := provider.ParseResponse([]byte(`{"balance_infos": []}`))
	require.Error(t, err)

	_, err = provider.ParseResponse([]byte(`{"balance_infos":[{"total_balance":"not-a-number"}]}`))
	require.Error(t, err)
}
