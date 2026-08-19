package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── OpenCode Zen Go 配额 provider 测试 ──

func TestOpenCodeZenGoProviderMatches(t *testing.T) {
	p := openCodeZenGoQuotaProvider{}
	require.True(t, p.Matches("https://opencode.ai/zen/go/v1"))
	require.True(t, p.Matches("https://opencode.ai/zen/go"))
	require.True(t, p.Matches("https://opencode.ai/zen/go/v1/chat/completions"))
	// 主站按量付费不匹配（无 Key 认证的余额端点）
	require.False(t, p.Matches("https://opencode.ai/zen/v1"))
	require.False(t, p.Matches("https://opencode.ai/zen/v1/chat/completions"))
	require.False(t, p.Matches("https://api.deepseek.com"))
	require.False(t, p.Matches("https://ark.cn-beijing.volces.com/api/v3"))
	require.Equal(t, "opencode", p.Name())
	require.Equal(t, "https://opencode.ai/zen/go/v1/usage", p.UsageURL("https://opencode.ai/zen/go/v1"))
	require.Equal(t, "Bearer sk-test", p.AuthorizationHeader("sk-test"))
}

func TestParseOpenCodeZenGoQuotaResponse(t *testing.T) {
	// 实测响应结构（2026-08-19 用真实 Key 验证）
	body := []byte(`{
		"usage": {
			"rolling":  {"status": "ok", "percent": 12.5, "resetsAt": "2026-08-19T20:34:26.643Z"},
			"weekly":   {"status": "ok", "percent": 4.2,  "resetsAt": "2026-08-24T00:00:00.643Z"},
			"monthly":  {"status": "ok", "percent": 1.1,  "resetsAt": "2026-09-19T15:26:40.643Z"}
		}
	}`)
	parsed, err := parseOpenCodeZenGoQuotaResponse(body)
	require.NoError(t, err)
	require.NotNil(t, parsed.opencode)
	require.Equal(t, float64(12.5), parsed.opencode.FiveHourPercent)
	require.Equal(t, "2026-08-19T20:34:26Z", parsed.opencode.FiveHourResetAt)
	require.Equal(t, float64(4.2), parsed.opencode.WeeklyPercent)
	require.Equal(t, "2026-08-24T00:00:00Z", parsed.opencode.WeeklyResetAt)
	require.Equal(t, float64(1.1), parsed.opencode.MonthlyPercent)
	require.Equal(t, "2026-09-19T15:26:40Z", parsed.opencode.MonthlyResetAt)
	// 原始数据保留
	require.NotNil(t, parsed.raw)
}

func TestParseOpenCodeZenGoQuotaResponseZeroPercent(t *testing.T) {
	// 全零百分比（新订阅未使用）仍应解析成功（有 resetsAt）
	body := []byte(`{
		"usage": {
			"rolling":  {"status": "ok", "percent": 0, "resetsAt": "2026-08-19T20:34:26.643Z"},
			"weekly":   {"status": "ok", "percent": 0, "resetsAt": "2026-08-24T00:00:00.643Z"},
			"monthly":  {"status": "ok", "percent": 0, "resetsAt": "2026-09-19T15:26:40.643Z"}
		}
	}`)
	parsed, err := parseOpenCodeZenGoQuotaResponse(body)
	require.NoError(t, err)
	require.NotNil(t, parsed.opencode)
	require.Equal(t, float64(0), parsed.opencode.FiveHourPercent)
	require.NotEmpty(t, parsed.opencode.FiveHourResetAt)
}

func TestParseOpenCodeZenGoQuotaResponseError(t *testing.T) {
	// 无 Go 订阅：403 EntitlementError 响应体
	body := []byte(`{"type": "error", "error": {"type": "EntitlementError", "message": "OpenCode Go subscription required."}}`)
	_, err := parseOpenCodeZenGoQuotaResponse(body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EntitlementError")
	require.Contains(t, err.Error(), "subscription required")
}

func TestParseOpenCodeZenGoQuotaResponseMissingUsage(t *testing.T) {
	_, err := parseOpenCodeZenGoQuotaResponse([]byte(`{"foo": 1}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing usage")
}

func TestComputeQuotaFromUsageOpenCode(t *testing.T) {
	parsed := parsedUpstreamQuotaUsage{
		opencode: &parsedOpenCodeZenGoQuota{FiveHourPercent: 12.5},
	}
	limit, used, remaining, mode := computeQuotaFromUsage(parsed, 0)
	require.Equal(t, UpstreamQuotaSyncModeOpenCode, mode)
	require.Equal(t, float64(0), limit)
	require.Equal(t, float64(0), used)
	require.Equal(t, float64(0), remaining)
}

// ── persistSnapshot opencode 分支：写 opencode_* 快照键 ──

func TestPersistSnapshotOpenCodeWritesExtraKeys(t *testing.T) {
	snapshot := &UpstreamQuotaSyncSnapshot{
		Status: UpstreamQuotaSyncStatusOK,
		Mode:   UpstreamQuotaSyncModeOpenCode,
		OpenCode: &UpstreamQuotaSyncOpenCodeQuota{
			FiveHourPercent: 12.5,
			FiveHourResetAt: "2026-08-19T20:34:26Z",
			WeeklyPercent:   4.2,
			WeeklyResetAt:   "2026-08-24T00:00:00Z",
			MonthlyPercent:  1.1,
			MonthlyResetAt:  "2026-09-19T15:26:40Z",
		},
	}
	// 键名断言（与火山分支同款约定）
	require.Equal(t, "opencode_5h_used_percent", cnExtraKey(PlatformOpenCode, cnExtraSuffix5hUsed))
	require.Equal(t, "opencode_weekly_used_percent", cnExtraKey(PlatformOpenCode, cnExtraSuffixWeeklyUsed))
	// 快照 JSON 序列化包含 opencode 字段
	b, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Contains(t, string(b), `"opencode":`)
	require.Contains(t, string(b), `"five_hour_percent":12.5`)
	require.Contains(t, string(b), `"monthly_percent":1.1`)
}
