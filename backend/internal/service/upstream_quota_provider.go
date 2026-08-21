package service

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

// 注：fork 专用的 zhipuQuotaProvider 与 deepSeekQuotaProvider（供 openai 平台
// + bigmodel/deepseek 中转账号经「同步上游配额」探测）均已移除——zhipu/deepseek
// 已有原生平台：配额/余额由 CNProviderQuotaService / CNProviderBalanceService
//（内部周期探测 + extra 落库）负责，展示经 PlatformTypeBadge 套餐徽章 +
// 用量窗口进度条/余额行。

func trimUpstreamQuotaBaseURL(baseURL string) string {
	return trimRightSlash(baseURL)
}

func trimRightSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
