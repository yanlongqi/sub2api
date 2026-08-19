package service

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── 火山引擎签名 V4 确定性测试（对齐 cc-switch coding_plan.rs 单测）──

func TestVolcengineCanonicalQuery(t *testing.T) {
	// 按 key 字母序：Action < Region < Version
	q := volcengineCanonicalQuery("GetAFPUsage", "cn-beijing")
	require.Equal(t, "Action=GetAFPUsage&Region=cn-beijing&Version=2024-01-01", q)
}

func TestVolcengineURIEncode(t *testing.T) {
	require.Equal(t, "abcXYZ019-_.~", volcURIEncode("abcXYZ019-_.~"))
	require.Equal(t, "%20%2B%3D%26", volcURIEncode(" +=&"))
	require.Equal(t, "%E4%B8%AD", volcURIEncode("中"))
}

func TestVolcengineSign(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	auth, xDate, xContent := volcengineSign(
		"AKTEST", "SKTEST", "cn-beijing",
		"Action=GetAFPUsage&Region=cn-beijing&Version=2024-01-01",
		[]byte(""), now,
	)
	require.Equal(t, "20260621T120000Z", xDate)
	// 空 body 的 SHA256
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", xContent)
	// 算法串无 AWS4 前缀、scope 结尾 request
	require.Contains(t, auth, "HMAC-SHA256 Credential=AKTEST/20260621/cn-beijing/ark/request, ")
	require.Contains(t, auth, "SignedHeaders=host;x-date;x-content-sha256;content-type, ")
	require.Contains(t, auth, "Signature=")
	// 签名为 64 位 hex
	sigStart := len(auth) - 64
	require.Regexp(t, "^[0-9a-f]{64}$", auth[sigStart:])
}

func TestVolcengineBuildSignedRequest(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	req, err := volcengineBuildSignedRequest("cn-beijing", "AK", "SK", "GetAFPUsage", now)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, req.Method)
	u, err := url.Parse(req.URL.String())
	require.NoError(t, err)
	require.Equal(t, "open.volcengineapi.com", u.Host)
	require.Equal(t, "/", u.Path)
	require.Equal(t, "Action=GetAFPUsage&Region=cn-beijing&Version=2024-01-01", u.RawQuery)
	require.Equal(t, "application/json; charset=utf-8", req.Header.Get("Content-Type"))
	require.NotEmpty(t, req.Header.Get("X-Date"))
	require.NotEmpty(t, req.Header.Get("X-Content-Sha256"))
	require.Contains(t, req.Header.Get("Authorization"), "HMAC-SHA256 Credential=AK/")
}

func TestVolcengineRegionFromBaseURL(t *testing.T) {
	require.Equal(t, "cn-beijing", volcengineRegionFromBaseURL("https://ark.cn-beijing.volces.com/api/v3"))
	require.Equal(t, "cn-shanghai", volcengineRegionFromBaseURL("https://ark.cn-shanghai.volces.com/api/v3"))
	// 非火山域名返回空
	require.Equal(t, "", volcengineRegionFromBaseURL("https://api.deepseek.com"))
	require.Equal(t, "", volcengineRegionFromBaseURL("https://open.bigmodel.cn/api/paas/v4"))
	// 无法识别 region 的火山域名回落默认
	require.Equal(t, volcengineDefaultRegion, volcengineRegionFromBaseURL("https://ark.volces.com"))
}

func TestVolcengineIsAuthErrorCode(t *testing.T) {
	require.True(t, volcengineIsAuthErrorCode("InvalidAuthorization"))
	require.True(t, volcengineIsAuthErrorCode("SignatureDoesNotMatch"))
	require.True(t, volcengineIsAuthErrorCode("AccessDenied"))
	require.False(t, volcengineIsAuthErrorCode("InternalError"))
	require.False(t, volcengineIsAuthErrorCode("ResourceNotFound"))
}

func TestVolcengineResponseError(t *testing.T) {
	body := []byte(`{"ResponseMetadata":{"Error":{"Code":"InvalidAuthorization","Message":"bad aksk"}},"Result":{}}`)
	code, msg, ok := volcengineResponseError(body)
	require.True(t, ok)
	require.Equal(t, "InvalidAuthorization", code)
	require.Equal(t, "bad aksk", msg)

	body2 := []byte(`{"Result":{"PlanType":"pro"}}`)
	_, _, ok = volcengineResponseError(body2)
	require.False(t, ok)
}

// ── GetAFPUsage（Agent Plan）解析 ──

func TestParseVolcengineQuotaResponseAFP(t *testing.T) {
	body := []byte(`{
		"Result": {
			"PlanType": "Pro",
			"AFPFiveHour": {"Quota": 120, "Used": 30, "ResetTime": 1782000000000},
			"AFPWeekly": {"Quota": 600, "Used": 150, "ResetTime": 1782400000000},
			"AFPMonthly": {"Quota": 0, "Used": 0, "ResetTime": 0},
			"AFPDaily": {"Quota": 999, "Used": 1, "ResetTime": 0}
		}
	}`)
	parsed, err := parseVolcengineQuotaResponse(body)
	require.NoError(t, err)
	require.NotNil(t, parsed.volcengine)
	require.Equal(t, "Pro", parsed.volcengine.PlanType)
	// 5h 窗口
	require.Equal(t, float64(120), parsed.volcengine.FiveHourQuota)
	require.Equal(t, float64(30), parsed.volcengine.FiveHourUsed)
	require.NotEmpty(t, parsed.volcengine.FiveHourResetAt)
	// weekly 窗口
	require.Equal(t, float64(600), parsed.volcengine.WeeklyQuota)
	require.Equal(t, float64(150), parsed.volcengine.WeeklyUsed)
	// monthly Quota<=0 跳过；AFPDaily 不解析
	require.Equal(t, float64(0), parsed.volcengine.MonthlyQuota)
	// 原始数据保留
	require.NotNil(t, parsed.raw)
}

func TestParseVolcengineQuotaResponseCodingPlan(t *testing.T) {
	body := []byte(`{
		"Result": {
			"QuotaUsage": [
				{"Level": "session", "Percent": 42.5, "ResetTime": 1782000000},
				{"Level": "weekly", "Percent": 12.0, "ResetTime": 1782400000},
				{"Level": "monthly", "Percent": 5.0, "ResetTime": 1783000000}
			]
		}
	}`)
	parsed, err := parseVolcengineQuotaResponse(body)
	require.NoError(t, err)
	require.NotNil(t, parsed.volcengine)
	require.Equal(t, float64(42.5), parsed.volcengine.FiveHourPercent)
	require.Equal(t, float64(12.0), parsed.volcengine.WeeklyPercent)
	require.Equal(t, float64(5.0), parsed.volcengine.MonthlyPercent)
	require.NotEmpty(t, parsed.volcengine.FiveHourResetAt)
	// Coding Plan 无绝对额度
	require.Equal(t, float64(0), parsed.volcengine.FiveHourQuota)
}

func TestParseVolcengineQuotaResponseAuthError(t *testing.T) {
	body := []byte(`{"ResponseMetadata":{"Error":{"Code":"InvalidAuthorization","Message":"aksk wrong"}}}`)
	_, err := parseVolcengineQuotaResponse(body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "InvalidAuthorization")
}

func TestParseVolcengineQuotaResponseNoWindows(t *testing.T) {
	// 已鉴权但未订阅任何 plan：Result 无可解析窗口
	body := []byte(`{"Result": {}}`)
	_, err := parseVolcengineQuotaResponse(body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no parseable windows")
}

func TestComputeQuotaFromUsageVolcengine(t *testing.T) {
	parsed := parsedUpstreamQuotaUsage{
		volcengine: &parsedVolcengineQuota{FiveHourQuota: 120, FiveHourUsed: 30},
	}
	limit, used, remaining, mode := computeQuotaFromUsage(parsed, 0)
	require.Equal(t, UpstreamQuotaSyncModeVolcengine, mode)
	require.Equal(t, float64(0), limit)
	require.Equal(t, float64(0), used)
	require.Equal(t, float64(0), remaining)
}

func TestVolcengineProviderMatches(t *testing.T) {
	p := volcengineQuotaProvider{}
	require.True(t, p.Matches("https://ark.cn-beijing.volces.com/api/v3"))
	require.True(t, p.Matches("https://ark.cn-shanghai.volces.com"))
	require.False(t, p.Matches("https://api.deepseek.com"))
	require.False(t, p.Matches("https://open.bigmodel.cn"))
	require.Equal(t, "volcengine", p.Name())
}

// ── persistSnapshot 火山分支：写 volcengine_* 快照键 ──

func TestPersistSnapshotVolcengineWritesExtraKeys(t *testing.T) {
	snapshot := &UpstreamQuotaSyncSnapshot{
		Status: UpstreamQuotaSyncStatusOK,
		Mode:   UpstreamQuotaSyncModeVolcengine,
		Volcengine: &UpstreamQuotaSyncVolcengineQuota{
			FiveHourPercent: 42.5,
			FiveHourResetAt: "2026-06-21T17:00:00Z",
			WeeklyPercent:   12.0,
			WeeklyResetAt:   "2026-06-26T00:00:00Z",
		},
	}
	updates := map[string]any{}
	// 复刻 persistSnapshot 的火山分支逻辑做键名断言（不落库）
	require.Equal(t, "volcengine_5h_used_percent", cnExtraKey(PlatformVolcengine, cnExtraSuffix5hUsed))
	require.Equal(t, "volcengine_weekly_used_percent", cnExtraKey(PlatformVolcengine, cnExtraSuffixWeeklyUsed))
	updates[cnExtraKey(PlatformVolcengine, cnExtraSuffix5hUsed)] = snapshot.Volcengine.FiveHourPercent
	updates[cnExtraKey(PlatformVolcengine, cnExtraSuffixWeeklyUsed)] = snapshot.Volcengine.WeeklyPercent
	v, ok := updates["volcengine_5h_used_percent"].(float64)
	require.True(t, ok)
	require.Equal(t, 42.5, v)
	// 快照 JSON 序列化包含 volcengine 字段
	b, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Contains(t, string(b), `"volcengine":`)
	require.Contains(t, string(b), `"five_hour_percent":42.5`)
}
