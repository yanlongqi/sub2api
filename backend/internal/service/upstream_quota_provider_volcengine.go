package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// 火山方舟（Volcengine Ark）Coding Plan / Agent Plan 配额同步 provider。
//
// 与智谱/Kimi（数据面 Bearer 接口）不同，火山用量接口是控制面 OpenAPI：
// 统一网关 open.volcengineapi.com（不是数据面推理域名 ark.cn-beijing.volces.com），
// 形如 POST https://open.volcengineapi.com/?Action=GetAFPUsage&Version=2024-01-01&Region=cn-beijing，
// 强制火山引擎签名 V4（AK/SK）——推理 Bearer Key 会被网关以 400 InvalidAuthorization 拒绝。
// 因此 AK/SK 与推理 api_key 是两套凭据，存储于账号 credentials 的
// volcengine_access_key_id / volcengine_secret_access_key（对齐 cc-switch 设计）。
//
// 探测顺序：先 GetAFPUsage（Agent Plan，回绝对额度 Quota/Used），
// 未订阅再 GetCodingPlanUsage（Coding Plan，回百分比）。
const (
	volcengineOpenAPIHost      = "open.volcengineapi.com"
	volcengineAPIVersion       = "2024-01-01"
	volcengineDefaultRegion    = "cn-beijing"
	volcengineService          = "ark"
	volcengineContentType      = "application/json; charset=utf-8"
	volcengineSignedHeaders    = "host;x-date;x-content-sha256;content-type"
	volcengineActionAFPUsage   = "GetAFPUsage"
	volcengineActionCodingPlan = "GetCodingPlanUsage"

	// 账号 credentials 中的火山 AK/SK 键（与推理 api_key 分离）。
	VolcengineAccessKeyIDCredentialKey     = "volcengine_access_key_id"
	VolcengineSecretAccessKeyCredentialKey = "volcengine_secret_access_key"
)

// volcengineQuotaProvider 查询火山方舟 Coding Plan / Agent Plan 配额。
// baseURL 仅用于域名判断（ark.*.volces.com）与 Region 提取；
// 配额端点固定为控制面网关 open.volcengineapi.com。
type volcengineQuotaProvider struct{}

func (volcengineQuotaProvider) Name() string {
	return "volcengine"
}

func (volcengineQuotaProvider) Matches(baseURL string) bool {
	return volcengineRegionFromBaseURL(baseURL) != ""
}

func (volcengineQuotaProvider) UsageURL(baseURL string) string {
	_ = baseURL // 端点固定控制面网关；query 由签名流程构造（见 volcengineBuildSignedRequest）
	return "https://" + volcengineOpenAPIHost + "/"
}

func (volcengineQuotaProvider) AuthorizationHeader(apiKey string) string {
	// 火山走 AK/SK 签名，不用 Bearer；此方法对火山无意义（见 ApplySignedAuth）。
	return ""
}

func (volcengineQuotaProvider) ParseResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	return parseVolcengineQuotaResponse(body)
}

// volcengineRegionFromBaseURL 从数据面 base_url 提取控制面 Region
// （ark.cn-beijing.volces.com → cn-beijing）；未命中 volces.com 域名返回空串。
func volcengineRegionFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "volces.com" && !strings.HasSuffix(host, ".volces.com") {
		return ""
	}
	for _, part := range strings.Split(host, ".") {
		if strings.HasPrefix(part, "cn-") || strings.HasPrefix(part, "ap-") {
			return part
		}
	}
	return volcengineDefaultRegion
}

// =================== 火山引擎签名 V4（AK/SK）===================
//
// 算法是 AWS SigV4 的火山变体（对照官方 volc-openapi-demos/signature/java/Sign.java）。
// 三处致命差异，照搬标准 SigV4（如 bedrock_signer）会签名失败：
//  1. canonical headers 与 SignedHeaders 用固定顺序
//     host;x-date;x-content-sha256;content-type（不按字母序）；
//  2. algorithm 串 HMAC-SHA256（无 AWS4 前缀）、credential scope 结尾 request
//     （非 aws4_request）；
//  3. 签名密钥 kDate=HMAC(SK, date)（SK 不加 AWS4 前缀）。
//
// canonical query 仍按 key 字母序（与标准 SigV4 一致）；service=ark、POST、空 body。

func volcHMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func volcSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// volcURIEncode 按 RFC3986 unreserved 字符集编码（A-Z a-z 0-9 - _ . ~ 之外全部 %XX）。
func volcURIEncode(input string) string {
	var out strings.Builder
	out.Grow(len(input))
	for i := 0; i < len(input); i++ {
		c := input[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

// volcengineCanonicalQuery 构造按 key 字母序排序、逐段 URL 编码的 canonical query string。
// 同一份字符串既用于签名也用于实际请求 URL，保证两者完全一致。
func volcengineCanonicalQuery(action, region string) string {
	pairs := [][2]string{
		{"Action", action},
		{"Region", region},
		{"Version", volcengineAPIVersion},
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, volcURIEncode(p[0])+"="+volcURIEncode(p[1]))
	}
	return strings.Join(parts, "&")
}

// volcengineSign 生成火山引擎签名 V4 的鉴权头三元组（Authorization, X-Date, X-Content-Sha256），
// 三者都要塞进请求头；canonicalQuery 必须与实际请求 URL 的 query 完全一致。
// now 作参数传入便于写确定性单测。
func volcengineSign(accessKeyID, secretAccessKey, region, canonicalQuery string, body []byte, now time.Time) (authorization, xDate, xContentSHA256 string) {
	xDate = now.UTC().Format("20060102T150405Z")
	shortDate := now.UTC().Format("20060102")
	xContentSHA256 = volcSHA256Hex(body)

	// 固定顺序 canonical headers（火山特有，不排序）。
	canonicalHeaders := "host:" + volcengineOpenAPIHost + "\n" +
		"x-date:" + xDate + "\n" +
		"x-content-sha256:" + xContentSHA256 + "\n" +
		"content-type:" + volcengineContentType + "\n"
	canonicalRequest := "POST\n/\n" + canonicalQuery + "\n" + canonicalHeaders + "\n" +
		volcengineSignedHeaders + "\n" + xContentSHA256

	credentialScope := shortDate + "/" + region + "/" + volcengineService + "/request"
	stringToSign := "HMAC-SHA256\n" + xDate + "\n" + credentialScope + "\n" + volcSHA256Hex([]byte(canonicalRequest))

	// 签名密钥派生：kDate=HMAC(SK, date)（SK 不加 AWS4 前缀），终止串 request。
	kDate := volcHMACSHA256([]byte(secretAccessKey), []byte(shortDate))
	kRegion := volcHMACSHA256(kDate, []byte(region))
	kService := volcHMACSHA256(kRegion, []byte(volcengineService))
	kSigning := volcHMACSHA256(kService, []byte("request"))
	signature := hex.EncodeToString(volcHMACSHA256(kSigning, []byte(stringToSign)))

	authorization = fmt.Sprintf(
		"HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, credentialScope, volcengineSignedHeaders, signature,
	)
	return authorization, xDate, xContentSHA256
}

// volcengineBuildSignedRequest 构造带火山签名 V4 头的 OpenAPI POST 请求。
// body 为空（GetAFPUsage/GetCodingPlanUsage 均无请求体）。
func volcengineBuildSignedRequest(region, accessKeyID, secretAccessKey, action string, now time.Time) (*http.Request, error) {
	canonicalQuery := volcengineCanonicalQuery(action, region)
	targetURL := "https://" + volcengineOpenAPIHost + "/?" + canonicalQuery
	body := []byte("")
	authorization, xDate, xContentSHA256 := volcengineSign(accessKeyID, secretAccessKey, region, canonicalQuery, body, now)

	req, err := http.NewRequest(http.MethodPost, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Date", xDate)
	req.Header.Set("X-Content-Sha256", xContentSHA256)
	req.Header.Set("Content-Type", volcengineContentType)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// =================== 响应解析 ===================

// parseVolcengineQuotaResponse 解析 GetAFPUsage / GetCodingPlanUsage 响应。
// 输出统一为 parsedVolcengineQuota（多窗口绝对额度 + 百分比），由调用方区分模式。
func parseVolcengineQuotaResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	// 火山 OpenAPI 业务错误常以 200 + ResponseMetadata.Error 返回。
	if code, msg, ok := volcengineResponseError(body); ok {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("volcengine quota error %s: %s", code, msg)
	}
	result := gjson.GetBytes(body, "Result")
	if !result.Exists() {
		result = gjson.ParseBytes(body)
	}
	vq := &parsedVolcengineQuota{
		PlanType: strings.TrimSpace(result.Get("PlanType").String()),
	}
	// 1) Agent Plan：AFPFiveHour/AFPWeekly/AFPMonthly 绝对额度窗口。
	for _, tier := range parseAFPUsageTiers(result) {
		switch tier.Window {
		case "5h":
			vq.FiveHourQuota, vq.FiveHourUsed, vq.FiveHourResetAt = tier.Quota, tier.Used, tier.ResetAt
		case "weekly":
			vq.WeeklyQuota, vq.WeeklyUsed, vq.WeeklyResetAt = tier.Quota, tier.Used, tier.ResetAt
		case "monthly":
			vq.MonthlyQuota, vq.MonthlyUsed, vq.MonthlyResetAt = tier.Quota, tier.Used, tier.ResetAt
		}
	}
	if vq.FiveHourQuota > 0 || vq.WeeklyQuota > 0 || vq.MonthlyQuota > 0 {
		return parsedUpstreamQuotaUsage{raw: raw, volcengine: vq}, nil
	}
	// 2) Coding Plan：QuotaUsage 数组（Level/Percent/ResetTime，仅百分比）。
	for _, tier := range parseCodingPlanUsageTiers(result) {
		switch tier.Window {
		case "5h":
			vq.FiveHourPercent, vq.FiveHourResetAt = tier.UsedPercent, tier.ResetAt
		case "weekly":
			vq.WeeklyPercent, vq.WeeklyResetAt = tier.UsedPercent, tier.ResetAt
		case "monthly":
			vq.MonthlyPercent, vq.MonthlyResetAt = tier.UsedPercent, tier.ResetAt
		}
	}
	if vq.FiveHourPercent <= 0 && vq.WeeklyPercent <= 0 && vq.MonthlyPercent <= 0 {
		return parsedUpstreamQuotaUsage{}, fmt.Errorf("volcengine quota response has no parseable windows")
	}
	return parsedUpstreamQuotaUsage{raw: raw, volcengine: vq}, nil
}

// volcengineResponseError 提取火山 OpenAPI 响应里的 ResponseMetadata.Error（或顶层 Error）。
func volcengineResponseError(body []byte) (code, msg string, ok bool) {
	err := gjson.GetBytes(body, "ResponseMetadata.Error")
	if !err.Exists() {
		err = gjson.GetBytes(body, "Error")
	}
	if !err.Exists() {
		return "", "", false
	}
	code = strings.TrimSpace(err.Get("Code").String())
	msg = strings.TrimSpace(err.Get("Message").String())
	if code == "" && msg == "" {
		return "", "", false
	}
	return code, msg, true
}

// volcengineIsAuthErrorCode 判断 OpenAPI 错误码是否属于鉴权类（AK/SK 错误）。
func volcengineIsAuthErrorCode(code string) bool {
	c := strings.ToLower(code)
	return strings.Contains(c, "auth") || strings.Contains(c, "signature") ||
		strings.Contains(c, "accessdenied") || strings.Contains(c, "denied") ||
		strings.Contains(c, "unauthorized") || strings.Contains(c, "forbidden") ||
		strings.Contains(c, "credential") || strings.Contains(c, "token")
}

// volcengineTier 表示一个用量窗口档位（绝对额度或百分比）。
type volcengineTier struct {
	Window      string  // "5h" | "weekly" | "monthly"
	Quota       float64 // Agent Plan 绝对额度（AFP 值）
	Used        float64
	UsedPercent float64 // Coding Plan 百分比
	ResetAt     string  // RFC3339
}

// parseAFPUsageTiers 解析 GetAFPUsage 的 Result 为窗口列表。
// 展示 5h / 周 / 月三个窗口（与控制台一致）；AFPDaily 被官方控制台隐藏
// （其 Quota 常高于周上限，属历史默认值而非强制限额），故跳过。
// Quota<=0 视为该窗口未订阅/未启用，跳过。
func parseAFPUsageTiers(result gjson.Result) []volcengineTier {
	var tiers []volcengineTier
	for _, key := range []string{"AFPFiveHour", "AFPWeekly", "AFPMonthly"} {
		win := result.Get(key)
		if !win.Exists() {
			continue
		}
		quota, valid := cnParseF64(win.Get("Quota").Value())
		if !valid || quota <= 0 {
			continue
		}
		used, _ := cnParseF64(win.Get("Used").Value())
		window := "5h"
		switch key {
		case "AFPWeekly":
			window = "weekly"
		case "AFPMonthly":
			window = "monthly"
		}
		tiers = append(tiers, volcengineTier{
			Window:  window,
			Quota:   quota,
			Used:    used,
			ResetAt: cnNormalizeResetTime(win.Get("ResetTime").Value()),
		})
	}
	return tiers
}

// volcengineCodingWindow 把 GetCodingPlanUsage 的 window 标签归一到窗口名。
func volcengineCodingWindow(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "session", "5h", "fivehour", "five_hour", "rolling_5h":
		return "5h"
	case "weekly", "week", "7d":
		return "weekly"
	case "monthly", "month":
		return "monthly"
	default:
		return ""
	}
}

// parseCodingPlanUsageTiers 解析 GetCodingPlanUsage 的 Result 为窗口列表（防御式）。
// 该接口官方文档未给出逐字段规格：真实字段是 Level（session/weekly/monthly）+ Percent；
// 这里宽松匹配 QuotaUsage/Usages/Details 数组及多种字段名，命中即用、未命中跳过。
func parseCodingPlanUsageTiers(result gjson.Result) []volcengineTier {
	var tiers []volcengineTier
	arr := result.Get("QuotaUsage")
	if !arr.Exists() || !arr.IsArray() {
		arr = result.Get("Usages")
	}
	if !arr.Exists() || !arr.IsArray() {
		arr = result.Get("Details")
	}
	if !arr.Exists() || !arr.IsArray() {
		return tiers
	}
	for _, item := range arr.Array() {
		label := firstNonEmptyGString(item, "Level", "Type", "Period", "Label", "Window")
		window := volcengineCodingWindow(label)
		if window == "" {
			continue
		}
		var percent float64
		for _, field := range []string{"Percent", "UsedPercent", "UsagePercent"} {
			if p, valid := cnParseF64(item.Get(field).Value()); valid {
				percent = p
				break
			}
		}
		resetAt := ""
		rt := item.Get("ResetTime")
		if !rt.Exists() {
			rt = item.Get("ResetTimestamp")
		}
		if rt.Exists() {
			resetAt = cnNormalizeResetTime(rt.Value())
		}
		tiers = append(tiers, volcengineTier{
			Window:      window,
			UsedPercent: percent,
			ResetAt:     resetAt,
		})
	}
	return tiers
}

func firstNonEmptyGString(item gjson.Result, fields ...string) string {
	for _, f := range fields {
		if s := strings.TrimSpace(item.Get(f).String()); s != "" {
			return s
		}
	}
	return ""
}
