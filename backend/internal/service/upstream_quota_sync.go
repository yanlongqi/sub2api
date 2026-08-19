package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// UpstreamQuotaSync 受管 extra keys（与 upstream_billing_probe 同套约定）。
const (
	// UpstreamQuotaSyncEnabledExtraKey 是账号级二级开关。
	// 仅当 QuotaLimitCard 总开关（quota_limit>0）开启后才有意义。
	UpstreamQuotaSyncEnabledExtraKey = "upstream_quota_sync_enabled"
	// UpstreamQuotaSyncExtraKey 存储最近一次同步快照（状态/数据/时间）。
	UpstreamQuotaSyncExtraKey = "upstream_quota_sync"
)

const (
	upstreamQuotaSyncCycleInterval  = time.Minute
	upstreamQuotaSyncRequestTimeout = 15 * time.Second
	upstreamQuotaSyncMaxBodyBytes   = 64 * 1024
	upstreamQuotaSyncMaxPerCycle    = 20
	upstreamQuotaSyncConcurrency    = 4
	upstreamQuotaSyncMaxDelay       = 24 * time.Hour
	upstreamQuotaSyncMinInterval    = 5
	upstreamQuotaSyncLeaderLockKey  = "upstream:quota:sync:leader"
	upstreamQuotaSyncLeaderLockTTL  = 2 * time.Minute
)

var (
	ErrUpstreamQuotaSyncUnavailable = infraerrors.ServiceUnavailable(
		"UPSTREAM_QUOTA_SYNC_UNAVAILABLE", "upstream quota sync is unavailable",
	)
	ErrUpstreamQuotaSyncAccountInvalid = infraerrors.BadRequest(
		"UPSTREAM_QUOTA_SYNC_ACCOUNT_INVALID", "account is not an apikey account eligible for upstream quota sync",
	)
)

const (
	UpstreamQuotaSyncStatusOK          = "ok"
	UpstreamQuotaSyncStatusUnsupported = "unsupported"
	UpstreamQuotaSyncStatusFailed      = "failed"

	UpstreamQuotaSyncModeSubscription = "subscription"
	UpstreamQuotaSyncModeBalance      = "balance"
	UpstreamQuotaSyncModeQuotaLimited = "quota_limited"
	UpstreamQuotaSyncModeZhipu        = "zhipu"
	UpstreamQuotaSyncModeVolcengine   = "volcengine"
	UpstreamQuotaSyncModeOpenCode     = "opencode"
)

// UpstreamQuotaSyncSnapshot 持久化在 accounts.extra。
type UpstreamQuotaSyncSnapshot struct {
	Status        string         `json:"status"`
	Mode          string         `json:"mode,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
	Limit         float64        `json:"limit,omitempty"`
	Used          float64        `json:"used,omitempty"`
	Remaining     float64        `json:"remaining,omitempty"`
	// 订阅模式：上游 sub2api /v1/usage 返回的各维度原始数据，用于前端按日/周/月分别展示。
	Subscription *UpstreamQuotaSyncSubscription `json:"subscription,omitempty"`
	// 余额模式下表示账户余额（remaining）。
	Balance         *float64    `json:"balance,omitempty"`
	// 余额模式的货币单位（上游 unit 字段，如 CNY/USD；空时前端默认 CNY）。
	Currency        string      `json:"currency,omitempty"`
	// 智谱模式：Coding Plan 滚动窗口已用百分比（5h + weekly 双窗口）。
	Zhipu           *UpstreamQuotaSyncZhipuQuota `json:"zhipu,omitempty"`
	// 火山方舟模式：Agent Plan（绝对额度）/ Coding Plan（百分比）多窗口快照。
	Volcengine      *UpstreamQuotaSyncVolcengineQuota `json:"volcengine,omitempty"`
	// OpenCode Zen Go 订阅模式：rolling/weekly/monthly 三窗口已用百分比。
	OpenCode        *UpstreamQuotaSyncOpenCodeQuota `json:"opencode,omitempty"`
	ReceivedAt      *time.Time  `json:"received_at,omitempty"`
	LastAttemptAt   time.Time   `json:"last_attempt_at"`
	NextSyncAt      time.Time   `json:"next_sync_at"`
	FailureCount    int         `json:"failure_count,omitempty"`
	HTTPStatus      int         `json:"http_status,omitempty"`
	LastError       string      `json:"last_error,omitempty"`
}

// UpstreamQuotaSyncZhipuQuota 是智谱 Coding Plan 配额的多窗口快照（已用百分比）。
// 5h 为 5 小时滚动窗口，weekly 为每周窗口（新套餐），period 为订阅周期额度
// （TIME_LIMIT，如 MCP 工具额度）；reset_at 为下次重置时间（RFC3339）。
type UpstreamQuotaSyncZhipuQuota struct {
	Level           string  `json:"level,omitempty"`
	FiveHourPercent float64 `json:"five_hour_percent,omitempty"`
	FiveHourResetAt string  `json:"five_hour_reset_at,omitempty"`
	WeeklyPercent   float64 `json:"weekly_percent,omitempty"`
	WeeklyResetAt   string  `json:"weekly_reset_at,omitempty"`
	PeriodPercent   float64 `json:"period_percent,omitempty"`
	PeriodResetAt   string  `json:"period_reset_at,omitempty"`
}

// UpstreamQuotaSyncVolcengineQuota 是火山方舟配额的多窗口快照。
// Agent Plan（GetAFPUsage）：各窗口回绝对额度 Quota/Used（AFP 值）+ 已用百分比；
// Coding Plan（GetCodingPlanUsage）：各窗口仅回已用百分比（Quota/Used 为 0）。
// reset_at 为下次重置时间（RFC3339）。
// UpstreamQuotaSyncOpenCodeQuota 是 OpenCode Zen Go 订阅配额的多窗口快照
// （GET /zen/go/v1/usage，三窗口仅回已用百分比；reset_at 为 RFC3339）。
// percent 字段不用 omitempty：新订阅全 0 时也要序列化（0 是有效值），
// 否则前端 v-if 判空会漏渲染 7d/30d 进度条。
type UpstreamQuotaSyncOpenCodeQuota struct {
	FiveHourPercent float64 `json:"five_hour_percent"`
	FiveHourResetAt string  `json:"five_hour_reset_at,omitempty"`
	WeeklyPercent   float64 `json:"weekly_percent"`
	WeeklyResetAt   string  `json:"weekly_reset_at,omitempty"`
	MonthlyPercent  float64 `json:"monthly_percent"`
	MonthlyResetAt  string  `json:"monthly_reset_at,omitempty"`
}

type UpstreamQuotaSyncVolcengineQuota struct {
	PlanType        string  `json:"plan_type,omitempty"`
	FiveHourQuota   float64 `json:"five_hour_quota,omitempty"`
	FiveHourUsed    float64 `json:"five_hour_used,omitempty"`
	FiveHourPercent float64 `json:"five_hour_percent,omitempty"`
	FiveHourResetAt string  `json:"five_hour_reset_at,omitempty"`
	WeeklyQuota     float64 `json:"weekly_quota,omitempty"`
	WeeklyUsed      float64 `json:"weekly_used,omitempty"`
	WeeklyPercent   float64 `json:"weekly_percent,omitempty"`
	WeeklyResetAt   string  `json:"weekly_reset_at,omitempty"`
	MonthlyQuota    float64 `json:"monthly_quota,omitempty"`
	MonthlyUsed     float64 `json:"monthly_used,omitempty"`
	MonthlyPercent  float64 `json:"monthly_percent,omitempty"`
	MonthlyResetAt  string  `json:"monthly_reset_at,omitempty"`
}

// UpstreamQuotaSyncSubscription 是上游订阅模式各维度限额/用量/窗口的归一化快照。
// 字段命名与既有 apikey 配额 extra 键（quota_*_limit/used/start/reset_at）一一对应，
// 便于前端复用既有 UsageProgressBar + QuotaBadge 显示逻辑。
type UpstreamQuotaSyncSubscription struct {
	Daily   *UpstreamQuotaSyncWindow `json:"daily,omitempty"`
	Weekly  *UpstreamQuotaSyncWindow `json:"weekly,omitempty"`
	Monthly *UpstreamQuotaSyncWindow `json:"monthly,omitempty"`
	// 订阅到期时间（RFC3339），上游 subscription.expires_at 透传。
	ExpiresAt string `json:"expires_at,omitempty"`
}

// UpstreamQuotaSyncWindow 单个维度（日/周/月）的限额窗口。
type UpstreamQuotaSyncWindow struct {
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	// WindowStart 为该窗口的起始时间（RFC3339）。上游 weekly_window_start 透传；
	// 日/月窗口上游未单独提供时，由同步时刻兜底，用于前端计算 reset_at。
	WindowStart string `json:"window_start,omitempty"`
	// ResetsAt 为该窗口下次重置时间（RFC3339）。日=+24h，周=上游 weekly_window_start+7d，
	// 月=+30d。前端优先使用此字段，回退到 WindowStart+周期。
	ResetsAt string `json:"resets_at,omitempty"`
}

// UpstreamQuotaSyncService 周期性调用上游 sub2api 的 /v1/usage，
// 把订阅/余额数据归一化成 quota_limit/quota_used 写回账号 extra。
type UpstreamQuotaSyncService struct {
	accountRepo               AccountRepository
	accountTestService        *AccountTestService
	cfg                       intervalSource
	securityAllowInsecureHTTP func() bool

	parentCtx      context.Context
	parentCancel   context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	started        bool
	stopped        bool
	cycleMu        sync.Mutex
	syncGroup      singleflight.Group
	syncSlots      chan struct{}
	now            func() time.Time
	lockCache      LeaderLockCache
	instanceID     string
	quotaProviders *upstreamQuotaProviderRegistry
}

// intervalSource 由 wire 注入，复用 token_refresh.check_interval_minutes。
type intervalSource interface {
	TokenRefreshCheckIntervalMinutes() int
}

// tokenRefreshIntervalAdapter 把 *config.Config 适配成 intervalSource。
type tokenRefreshIntervalAdapter struct {
	minutes int
}

func (a tokenRefreshIntervalAdapter) TokenRefreshCheckIntervalMinutes() int {
	return a.minutes
}

// ConfigTokenRefreshInterval 是 wire 注入所需的最小接口。
type ConfigTokenRefreshInterval interface {
	// TokenRefreshCheckIntervalMinutes 返回 token_refresh.check_interval_minutes。
	// *config.Config 通过其 TokenRefresh 字段天然满足此接口的 duck-typed 调用方约定。
}

// NewUpstreamQuotaSyncIntervalSource 从 *config.Config 读取 token_refresh.check_interval_minutes。
func NewUpstreamQuotaSyncIntervalSource(checkIntervalMinutes int) intervalSource {
	return tokenRefreshIntervalAdapter{minutes: checkIntervalMinutes}
}

// NewUpstreamQuotaSyncService 构造服务。
func NewUpstreamQuotaSyncService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	interval intervalSource,
) *UpstreamQuotaSyncService {
	ctx, cancel := context.WithCancel(context.Background())
	return &UpstreamQuotaSyncService{
		accountRepo:        accountRepo,
		accountTestService: accountTestService,
		cfg:                interval,
		parentCtx:          ctx,
		parentCancel:       cancel,
		syncSlots:          make(chan struct{}, upstreamQuotaSyncConcurrency),
		now:                time.Now,
		instanceID:         uuid.NewString(),
		quotaProviders:     newUpstreamQuotaProviderRegistry(zhipuQuotaProvider{}, volcengineQuotaProvider{}, openCodeZenGoQuotaProvider{}, deepSeekQuotaProvider{}, defaultSub2APIQuotaProvider{}),
	}
}

// SetLeaderLock 注入分布式锁依赖（与 upstream_billing_probe 同模式）。
func (s *UpstreamQuotaSyncService) SetLeaderLock(lockCache LeaderLockCache, db interface{}) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
}

// ProvideUpstreamQuotaSyncService 启动周期 runner。
func ProvideUpstreamQuotaSyncService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	interval intervalSource,
	lockCache LeaderLockCache,
) *UpstreamQuotaSyncService {
	svc := NewUpstreamQuotaSyncService(accountRepo, accountTestService, interval)
	svc.SetLeaderLock(lockCache, nil)
	svc.Start()
	return svc
}

func (s *UpstreamQuotaSyncService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *UpstreamQuotaSyncService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *UpstreamQuotaSyncService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(upstreamQuotaSyncCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil {
				logger.LegacyPrintf("service.upstream_quota_sync", "run_due_failed: err=%v", err)
			}
		}
	}
}

func (s *UpstreamQuotaSyncService) intervalMinutes() int {
	if s.cfg != nil {
		if v := s.cfg.TokenRefreshCheckIntervalMinutes(); v > 0 {
			if v < upstreamQuotaSyncMinInterval {
				return upstreamQuotaSyncMinInterval
			}
			return v
		}
	}
	return upstreamQuotaSyncMinInterval
}

func (s *UpstreamQuotaSyncService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// RunDue 执行一个有界批次的到期账号同步。
func (s *UpstreamQuotaSyncService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	runRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, upstreamQuotaSyncLeaderLockKey)
	if lockErr != nil {
		return fmt.Errorf("acquire upstream quota sync leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer runRelease()

	now := s.currentTime()
	accounts, err := s.accountRepo.FindByExtraField(ctx, UpstreamQuotaSyncEnabledExtraKey, true)
	if err != nil {
		return fmt.Errorf("list upstream quota sync candidates: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !isUpstreamQuotaSyncAccount(&account) || !account.IsActive() || !upstreamQuotaSyncEnabled(&account) {
			continue
		}
		snapshot := decodeUpstreamQuotaSyncSnapshot(account.Extra)
		if snapshot != nil && !snapshot.NextSyncAt.IsZero() && now.Before(snapshot.NextSyncAt) {
			continue
		}
		due = append(due, account)
	}
	if len(due) > upstreamQuotaSyncMaxPerCycle {
		due = due[:upstreamQuotaSyncMaxPerCycle]
	}
	interval := s.intervalMinutes()
	for i := range due {
		account := due[i]
		if _, err := s.syncAccountWithMode(ctx, account.ID, interval, true); err != nil {
			logger.LegacyPrintf("service.upstream_quota_sync", "sync_failed account=%d err=%v", account.ID, err)
		}
	}
	return nil
}

// SyncAccount 手动触发单账号同步（忽略二级开关与节流，供管理员按钮调用）。
func (s *UpstreamQuotaSyncService) SyncAccount(ctx context.Context, accountID int64) (*UpstreamQuotaSyncSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamQuotaSyncUnavailable
	}
	return s.syncAccountWithMode(ctx, accountID, s.intervalMinutes(), false)
}

func (s *UpstreamQuotaSyncService) syncAccountWithMode(ctx context.Context, accountID int64, intervalMinutes int, requireEnabled bool) (*UpstreamQuotaSyncSnapshot, error) {
	key := fmt.Sprintf("%d", accountID)
	value, err, _ := s.syncGroup.Do(key, func() (any, error) {
		select {
		case s.syncSlots <- struct{}{}:
			defer func() { <-s.syncSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !isUpstreamQuotaSyncAccount(account) {
			return nil, ErrUpstreamQuotaSyncAccountInvalid
		}
		if requireEnabled {
			if !account.IsActive() || !upstreamQuotaSyncEnabled(account) {
				return nil, nil
			}
		}
		return s.syncLoadedAccount(ctx, account, intervalMinutes)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	return value.(*UpstreamQuotaSyncSnapshot), nil
}

func (s *UpstreamQuotaSyncService) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(lockCtx, key, s.instanceID, upstreamQuotaSyncLeaderLockTTL)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
		}, true, nil
	}
	return func() {}, true, nil
}

func (s *UpstreamQuotaSyncService) syncLoadedAccount(ctx context.Context, account *Account, intervalMinutes int) (*UpstreamQuotaSyncSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "transport_unavailable")
	}
	apiKey := upstreamQuotaSyncAPIKey(account)
	if apiKey == "" {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "missing_api_key")
	}
	baseURL := upstreamQuotaSyncBaseURL(account)
	if baseURL == "" {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "missing_base_url")
	}
	normalizedBaseURL, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "invalid_base_url")
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
			return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "proxy_unavailable")
		}
		proxyURL = account.Proxy.URL()
	}
	provider := s.quotaProviders.Resolve(normalizedBaseURL)
	// 火山方舟：控制面 OpenAPI 走 AK/SK 签名 POST，与通用 GET + Bearer 流程不同。
	if volcProvider, ok := provider.(volcengineQuotaProvider); ok {
		return s.syncVolcengineAccount(ctx, account, intervalMinutes, now, normalizedBaseURL, proxyURL, volcProvider)
	}
	syncURL := provider.UsageURL(normalizedBaseURL)
	syncCtx, cancel := context.WithTimeout(ctx, upstreamQuotaSyncRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(syncCtx, http.MethodGet, syncURL, bytes.NewReader(nil))
	if err != nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "request_build_failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", provider.AuthorizationHeader(apiKey))
	account.ApplyHeaderOverrides(req.Header)
	var tlsProfile *tlsfingerprint.Profile
	if s.accountTestService.tlsFPProfileService != nil {
		tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
	}
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "request_failed")
	}
	if resp == nil || resp.Body == nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "empty_response")
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamQuotaSyncMaxBodyBytes+1))
	if readErr != nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_read_failed")
	}
	if len(body) > upstreamQuotaSyncMaxBodyBytes {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_too_large")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "unsupported")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "http_error")
	}
	parsed, parseErr := provider.ParseResponse(body)
	if parseErr != nil {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "invalid_response")
	}
	limit, used, remaining, mode := computeQuotaFromUsage(parsed, account.GetQuotaLimit())
	snapshot := &UpstreamQuotaSyncSnapshot{
		Status:        UpstreamQuotaSyncStatusOK,
		Mode:          mode,
		Data:          parsed.raw,
		Limit:         limit,
		Used:          used,
		Remaining:     remaining,
		ReceivedAt:    probeTimePtr(now),
		LastAttemptAt: now,
		NextSyncAt:    now.Add(nextSyncDelay(intervalMinutes, 0)),
		HTTPStatus:    resp.StatusCode,
	}
	// 订阅模式：填充各维度窗口，前端按日/周/月独立展示 + reset_at。
	if mode == UpstreamQuotaSyncModeSubscription && parsed.subscription != nil {
		windows := buildSubscriptionWindows(parsed.subscription, now)
		sub := &UpstreamQuotaSyncSubscription{}
		// buildSubscriptionWindows 保证顺序：daily, weekly, monthly
		idx := 0
		if idx < len(windows) && parsed.subscription.DailyLimitUSD != nil && *parsed.subscription.DailyLimitUSD > 0 {
			sub.Daily = windows[idx]
			idx++
		}
		if idx < len(windows) && parsed.subscription.WeeklyLimitUSD != nil && *parsed.subscription.WeeklyLimitUSD > 0 {
			sub.Weekly = windows[idx]
			idx++
		}
		if idx < len(windows) && parsed.subscription.MonthlyLimitUSD != nil && *parsed.subscription.MonthlyLimitUSD > 0 {
			sub.Monthly = windows[idx]
			idx++
		}
		if parsed.subscription.ExpiresAt != nil {
			sub.ExpiresAt = *parsed.subscription.ExpiresAt
		}
		snapshot.Subscription = sub
	}
	// 余额模式：填充 balance 字段与货币单位，前端按余额显示。
	if mode == UpstreamQuotaSyncModeBalance && parsed.balance != nil {
		bal := *parsed.balance
		snapshot.Balance = &bal
		snapshot.Currency = normalizeUpstreamQuotaCurrency(parsed.currency)
	}
	// 智谱模式：填充 5h + weekly + 订阅周期多窗口快照。
	if mode == UpstreamQuotaSyncModeZhipu && parsed.zhipu != nil {
		snapshot.Zhipu = &UpstreamQuotaSyncZhipuQuota{
			Level:           parsed.zhipu.Level,
			FiveHourPercent: parsed.zhipu.FiveHourPercent,
			FiveHourResetAt: parsed.zhipu.FiveHourResetAt,
			WeeklyPercent:   parsed.zhipu.WeeklyPercent,
			WeeklyResetAt:   parsed.zhipu.WeeklyResetAt,
			PeriodPercent:   parsed.zhipu.PeriodPercent,
			PeriodResetAt:   parsed.zhipu.PeriodResetAt,
		}
	}
	// OpenCode Zen Go 模式：填充 rolling/weekly/monthly 三窗口快照。
	if mode == UpstreamQuotaSyncModeOpenCode && parsed.opencode != nil {
		snapshot.OpenCode = &UpstreamQuotaSyncOpenCodeQuota{
			FiveHourPercent: parsed.opencode.FiveHourPercent,
			FiveHourResetAt: parsed.opencode.FiveHourResetAt,
			WeeklyPercent:   parsed.opencode.WeeklyPercent,
			WeeklyResetAt:   parsed.opencode.WeeklyResetAt,
			MonthlyPercent:  parsed.opencode.MonthlyPercent,
			MonthlyResetAt:  parsed.opencode.MonthlyResetAt,
		}
	}
	if err := s.persistSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// syncVolcengineAccount 同步火山方舟账号配额：控制面 OpenAPI（AK/SK 签名 POST）。
// 探测顺序：先 GetAFPUsage（Agent Plan，绝对额度），未订阅再 GetCodingPlanUsage
// （Coding Plan，百分比）。两个 plan 共用同一份 AK/SK，鉴权类错误直接停。
func (s *UpstreamQuotaSyncService) syncVolcengineAccount(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	normalizedBaseURL string,
	proxyURL string,
	_ volcengineQuotaProvider,
) (*UpstreamQuotaSyncSnapshot, error) {
	accessKeyID := strings.TrimSpace(account.GetCredential(VolcengineAccessKeyIDCredentialKey))
	secretAccessKey := strings.TrimSpace(account.GetCredential(VolcengineSecretAccessKeyCredentialKey))
	if accessKeyID == "" || secretAccessKey == "" {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "missing_volcengine_credentials")
	}
	region := volcengineRegionFromBaseURL(normalizedBaseURL)
	if region == "" {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, 0, "invalid_base_url")
	}
	var (
		parsed    parsedUpstreamQuotaUsage
		httpStats int
		hit       bool
	)
	for _, action := range []string{volcengineActionAFPUsage, volcengineActionCodingPlan} {
		body, statusCode, err := s.callVolcengineOpenAPI(ctx, account, region, accessKeyID, secretAccessKey, action, proxyURL)
		if err != nil {
			return s.persistSyncFailure(ctx, account, intervalMinutes, now, statusCode, err.Error())
		}
		httpStats = statusCode
		candidate, parseErr := parseVolcengineQuotaResponse(body)
		if parseErr != nil {
			// 鉴权类错误（AK/SK 无效）直接停，不再试另一个 plan。
			if code, _, ok := volcengineResponseError(body); ok && volcengineIsAuthErrorCode(code) {
				return s.persistSyncFailure(ctx, account, intervalMinutes, now, statusCode, "volcengine_auth_failed")
			}
			continue
		}
		parsed = candidate
		hit = true
		break
	}
	if !hit {
		return s.persistSyncFailure(ctx, account, intervalMinutes, now, httpStats, "invalid_response")
	}
	limit, used, remaining, mode := computeQuotaFromUsage(parsed, account.GetQuotaLimit())
	snapshot := &UpstreamQuotaSyncSnapshot{
		Status:     UpstreamQuotaSyncStatusOK,
		Mode:       mode,
		Data:       parsed.raw,
		Limit:      limit,
		Used:       used,
		Remaining:  remaining,
		ReceivedAt: probeTimePtr(now),
		LastAttemptAt: now,
		NextSyncAt:  now.Add(nextSyncDelay(intervalMinutes, 0)),
		HTTPStatus:  httpStats,
	}
	if mode == UpstreamQuotaSyncModeVolcengine && parsed.volcengine != nil {
		snapshot.Volcengine = &UpstreamQuotaSyncVolcengineQuota{
			PlanType:        parsed.volcengine.PlanType,
			FiveHourQuota:   parsed.volcengine.FiveHourQuota,
			FiveHourUsed:    parsed.volcengine.FiveHourUsed,
			FiveHourPercent: parsed.volcengine.FiveHourPercent,
			FiveHourResetAt: parsed.volcengine.FiveHourResetAt,
			WeeklyQuota:     parsed.volcengine.WeeklyQuota,
			WeeklyUsed:      parsed.volcengine.WeeklyUsed,
			WeeklyPercent:   parsed.volcengine.WeeklyPercent,
			WeeklyResetAt:   parsed.volcengine.WeeklyResetAt,
			MonthlyQuota:    parsed.volcengine.MonthlyQuota,
			MonthlyUsed:     parsed.volcengine.MonthlyUsed,
			MonthlyPercent:  parsed.volcengine.MonthlyPercent,
			MonthlyResetAt:  parsed.volcengine.MonthlyResetAt,
		}
	}
	if err := s.persistSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// callVolcengineOpenAPI 发起一次火山控制面 OpenAPI 签名调用，返回响应体与状态码。
// 传输失败返回 err（reason 直接进 persistSyncFailure）。
func (s *UpstreamQuotaSyncService) callVolcengineOpenAPI(
	ctx context.Context,
	account *Account,
	region, accessKeyID, secretAccessKey, action, proxyURL string,
) ([]byte, int, error) {
	syncCtx, cancel := context.WithTimeout(ctx, upstreamQuotaSyncRequestTimeout)
	defer cancel()
	req, err := volcengineBuildSignedRequest(region, accessKeyID, secretAccessKey, action, s.currentTime())
	if err != nil {
		return nil, 0, fmt.Errorf("request_build_failed")
	}
	req = req.WithContext(syncCtx)
	account.ApplyHeaderOverrides(req.Header)
	var tlsProfile *tlsfingerprint.Profile
	if s.accountTestService.tlsFPProfileService != nil {
		tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
	}
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return nil, 0, fmt.Errorf("request_failed")
	}
	if resp == nil || resp.Body == nil {
		return nil, 0, fmt.Errorf("empty_response")
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamQuotaSyncMaxBodyBytes+1))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("response_read_failed")
	}
	if len(body) > upstreamQuotaSyncMaxBodyBytes {
		return nil, resp.StatusCode, fmt.Errorf("response_too_large")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, resp.StatusCode, fmt.Errorf("unsupported")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error")
	}
	return body, resp.StatusCode, nil
}

func (s *UpstreamQuotaSyncService) persistSyncFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
) (*UpstreamQuotaSyncSnapshot, error) {
	previous := decodeUpstreamQuotaSyncSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	status := UpstreamQuotaSyncStatusFailed
	if reason == "unsupported" {
		status = UpstreamQuotaSyncStatusUnsupported
	}
	snapshot := &UpstreamQuotaSyncSnapshot{
		Status:        status,
		Mode:          previousMode(previous),
		LastAttemptAt: now,
		NextSyncAt:    now.Add(nextSyncDelay(intervalMinutes, 0)),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		snapshot.Data = previous.Data
		snapshot.Limit = previous.Limit
		snapshot.Used = previous.Used
		snapshot.Remaining = previous.Remaining
		snapshot.ReceivedAt = previous.ReceivedAt
	}
	if err := s.persistSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func previousMode(previous *UpstreamQuotaSyncSnapshot) string {
	if previous == nil {
		return ""
	}
	return previous.Mode
}

// normalizeUpstreamQuotaCurrency 归一化余额货币单位：上游未返回时默认 CNY。
func normalizeUpstreamQuotaCurrency(currency string) string {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" {
		return "CNY"
	}
	return c
}

// persistSnapshot 把快照写入 extra[upstream_quota_sync]，并把归一化的 quota_limit/quota_used
// 一并合并写入（让 UpdateExtra 单次事务触发调度快照刷新）。
func (s *UpstreamQuotaSyncService) persistSnapshot(ctx context.Context, account *Account, snapshot *UpstreamQuotaSyncSnapshot) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamQuotaSyncUnavailable
	}
	updates := map[string]any{
		UpstreamQuotaSyncExtraKey: snapshot,
	}
	// 仅在成功状态下覆盖配额字段；失败时保留既有用量，避免显示抖动。
	if snapshot.Status == UpstreamQuotaSyncStatusOK {
		// 订阅模式：清空顶层 quota_limit/quota_used（订阅无"总配额"概念），
		// 各维度独立写入 quota_daily_*/quota_weekly_*/quota_monthly_*。
		// 余额/quota_limited 模式：写顶层 quota_limit/quota_used。
		if snapshot.Mode == UpstreamQuotaSyncModeSubscription {
			updates["quota_limit"] = nil
			updates["quota_used"] = nil
			// 先无条件清空三个维度的全部字段，避免上游移除某维度后本地残留陈旧数据。
			// persistSubscriptionWindow 仅在上游仍返回该维度时才覆盖写入有效值。
			clearSubscriptionWindow(updates, "daily")
			clearSubscriptionWindow(updates, "weekly")
			clearSubscriptionWindow(updates, "monthly")
			if snapshot.Subscription != nil {
				persistSubscriptionWindow(updates, "daily", snapshot.Subscription.Daily)
				persistSubscriptionWindow(updates, "weekly", snapshot.Subscription.Weekly)
				persistSubscriptionWindow(updates, "monthly", snapshot.Subscription.Monthly)
			}
		} else if snapshot.Mode == UpstreamQuotaSyncModeZhipu {
			// 智谱百分比配额：不写顶层 quota_limit/quota_used（无绝对额度），
			// 写 zhipu_5h/weekly 快照键供调度阈值评估（cnProviderThresholdCandidates）复用。
			updates["quota_limit"] = nil
			updates["quota_used"] = nil
			clearSubscriptionWindow(updates, "daily")
			clearSubscriptionWindow(updates, "weekly")
			clearSubscriptionWindow(updates, "monthly")
			if snapshot.Zhipu != nil {
				updates[cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed)] = snapshot.Zhipu.FiveHourPercent
				updates[cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyUsed)] = snapshot.Zhipu.WeeklyPercent
				if snapshot.Zhipu.FiveHourResetAt != "" {
					updates[cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset)] = snapshot.Zhipu.FiveHourResetAt
				}
				if snapshot.Zhipu.WeeklyResetAt != "" {
					updates[cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset)] = snapshot.Zhipu.WeeklyResetAt
				}
			}
		} else if snapshot.Mode == UpstreamQuotaSyncModeVolcengine {
			// 火山方舟多窗口配额：不写顶层 quota_limit/quota_used（无单一总额概念），
			// 写 volcengine_5h/weekly 快照键供调度阈值评估与前端展示。
			updates["quota_limit"] = nil
			updates["quota_used"] = nil
			clearSubscriptionWindow(updates, "daily")
			clearSubscriptionWindow(updates, "weekly")
			clearSubscriptionWindow(updates, "monthly")
			if snapshot.Volcengine != nil {
				updates[cnExtraKey(PlatformVolcengine, cnExtraSuffix5hUsed)] = snapshot.Volcengine.FiveHourPercent
				updates[cnExtraKey(PlatformVolcengine, cnExtraSuffixWeeklyUsed)] = snapshot.Volcengine.WeeklyPercent
				if snapshot.Volcengine.FiveHourResetAt != "" {
					updates[cnExtraKey(PlatformVolcengine, cnExtraSuffix5hReset)] = snapshot.Volcengine.FiveHourResetAt
				}
				if snapshot.Volcengine.WeeklyResetAt != "" {
					updates[cnExtraKey(PlatformVolcengine, cnExtraSuffixWeeklyReset)] = snapshot.Volcengine.WeeklyResetAt
				}
			}
		} else if snapshot.Mode == UpstreamQuotaSyncModeOpenCode {
			// OpenCode Zen Go 订阅配额：不写顶层 quota_limit/quota_used（无绝对额度），
			// 写 opencode_5h/weekly 快照键供调度阈值评估与前端展示。
			updates["quota_limit"] = nil
			updates["quota_used"] = nil
			clearSubscriptionWindow(updates, "daily")
			clearSubscriptionWindow(updates, "weekly")
			clearSubscriptionWindow(updates, "monthly")
			if snapshot.OpenCode != nil {
				updates[cnExtraKey(PlatformOpenCode, cnExtraSuffix5hUsed)] = snapshot.OpenCode.FiveHourPercent
				updates[cnExtraKey(PlatformOpenCode, cnExtraSuffixWeeklyUsed)] = snapshot.OpenCode.WeeklyPercent
				if snapshot.OpenCode.FiveHourResetAt != "" {
					updates[cnExtraKey(PlatformOpenCode, cnExtraSuffix5hReset)] = snapshot.OpenCode.FiveHourResetAt
				}
				if snapshot.OpenCode.WeeklyResetAt != "" {
					updates[cnExtraKey(PlatformOpenCode, cnExtraSuffixWeeklyReset)] = snapshot.OpenCode.WeeklyResetAt
				}
			}
		} else {
			if snapshot.Limit > 0 {
				updates["quota_limit"] = snapshot.Limit
			} else {
				updates["quota_limit"] = nil
			}
			updates["quota_used"] = snapshot.Used
			// 非订阅模式：清空可能残留的订阅维度（含 used/start/reset_at），避免显示陈旧数据
			clearSubscriptionWindow(updates, "daily")
			clearSubscriptionWindow(updates, "weekly")
			clearSubscriptionWindow(updates, "monthly")
		}
	}
	return s.accountRepo.UpdateExtra(ctx, account.ID, updates)
}

// clearSubscriptionWindow 把单个维度的 limit/used/start/reset_at 全部置 nil，
// 供 persistSnapshot 在写入前清空残留字段。dimension 取值 daily/weekly/monthly。
func clearSubscriptionWindow(updates map[string]any, dimension string) {
	prefix := "quota_" + dimension + "_"
	updates[prefix+"limit"] = nil
	updates[prefix+"used"] = nil
	updates[prefix+"start"] = nil
	updates[prefix+"reset_at"] = nil
}

// persistSubscriptionWindow 把单个订阅窗口映射到既有 extra 键。
// 维度 dimension 取值 daily/weekly/monthly。各维度独立持久化到 quota_{dim}_*；
// 月维度不再复用顶层 quota_*，由前端 AccountCapacityCell M 徽章与 AccountUsageCell 30d 进度条展示。
func persistSubscriptionWindow(updates map[string]any, dimension string, w *UpstreamQuotaSyncWindow) {
	if w == nil || w.Limit <= 0 {
		return
	}
	switch dimension {
	case "daily":
		updates["quota_daily_limit"] = w.Limit
		updates["quota_daily_used"] = w.Used
		if w.WindowStart != "" {
			updates["quota_daily_start"] = w.WindowStart
		}
		if w.ResetsAt != "" {
			updates["quota_daily_reset_at"] = w.ResetsAt
		}
	case "weekly":
		updates["quota_weekly_limit"] = w.Limit
		updates["quota_weekly_used"] = w.Used
		if w.WindowStart != "" {
			updates["quota_weekly_start"] = w.WindowStart
		}
		if w.ResetsAt != "" {
			updates["quota_weekly_reset_at"] = w.ResetsAt
		}
	case "monthly":
		// 月维度独立持久化到 quota_monthly_*，前端 AccountCapacityCell 新增 M 徽章展示。
		updates["quota_monthly_limit"] = w.Limit
		updates["quota_monthly_used"] = w.Used
		if w.WindowStart != "" {
			updates["quota_monthly_start"] = w.WindowStart
		}
		if w.ResetsAt != "" {
			updates["quota_monthly_reset_at"] = w.ResetsAt
		}
	}
}

// =================== 解析与归一化 ===================

type upstreamQuotaUsageRaw struct {
	Mode         string  `json:"mode"`
	IsValid      bool    `json:"isValid"`
	Remaining    float64 `json:"remaining"`
	Unit         string  `json:"unit"`
	Quota        *struct {
		Limit     float64 `json:"limit"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
	} `json:"quota"`
	Subscription *struct {
		DailyUsageUSD     float64  `json:"daily_usage_usd"`
		WeeklyUsageUSD    float64  `json:"weekly_usage_usd"`
		MonthlyUsageUSD   float64  `json:"monthly_usage_usd"`
		DailyLimitUSD     *float64 `json:"daily_limit_usd"`
		WeeklyLimitUSD    *float64 `json:"weekly_limit_usd"`
		MonthlyLimitUSD   *float64 `json:"monthly_limit_usd"`
		WeeklyWindowStart *string  `json:"weekly_window_start"`
		ExpiresAt         *string  `json:"expires_at"`
	} `json:"subscription"`
	Balance *float64 `json:"balance"`
	raw     map[string]any
}

type parsedUpstreamQuotaUsage struct {
	raw       map[string]any
	mode      string
	currency  string
	quota     *struct {
		Limit     float64
		Used      float64
		Remaining float64
	}
	subscription *struct {
		DailyUsageUSD     float64
		WeeklyUsageUSD    float64
		MonthlyUsageUSD   float64
		DailyLimitUSD     *float64
		WeeklyLimitUSD    *float64
		MonthlyLimitUSD   *float64
		WeeklyWindowStart *string
		ExpiresAt         *string
	}
	balance    *float64
	zhipu      *parsedZhipuQuota
	volcengine *parsedVolcengineQuota
	opencode   *parsedOpenCodeZenGoQuota
}

// parsedVolcengineQuota 是火山方舟配额响应的解析结果（多窗口绝对额度/百分比）。
type parsedVolcengineQuota struct {
	PlanType        string
	FiveHourQuota   float64
	FiveHourUsed    float64
	FiveHourPercent float64
	FiveHourResetAt string
	WeeklyQuota     float64
	WeeklyUsed      float64
	WeeklyPercent   float64
	WeeklyResetAt   string
	MonthlyQuota    float64
	MonthlyUsed     float64
	MonthlyPercent  float64
	MonthlyResetAt  string
}

// parsedOpenCodeZenGoQuota 是 OpenCode Zen Go 配额响应的解析结果（三窗口已用百分比）。
type parsedOpenCodeZenGoQuota struct {
	FiveHourPercent float64
	FiveHourResetAt string
	WeeklyPercent   float64
	WeeklyResetAt   string
	MonthlyPercent  float64
	MonthlyResetAt  string
}

// parsedZhipuQuota 是智谱配额响应的解析结果（5h + weekly + 订阅周期已用百分比）。
type parsedZhipuQuota struct {
	Level           string
	FiveHourPercent float64
	FiveHourResetAt string
	WeeklyPercent   float64
	WeeklyResetAt   string
	PeriodPercent   float64
	PeriodResetAt   string
}

func parseUpstreamQuotaUsageResponse(body []byte) (parsedUpstreamQuotaUsage, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	var typed upstreamQuotaUsageRaw
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&typed); err != nil {
		return parsedUpstreamQuotaUsage{}, err
	}
	typed.raw = raw
	out := parsedUpstreamQuotaUsage{raw: raw, mode: typed.Mode, currency: typed.Unit}
	if typed.Quota != nil {
		out.quota = &struct {
			Limit     float64
			Used      float64
			Remaining float64
		}{Limit: typed.Quota.Limit, Used: typed.Quota.Used, Remaining: typed.Quota.Remaining}
	}
	if typed.Subscription != nil {
		out.subscription = &struct {
			DailyUsageUSD     float64
			WeeklyUsageUSD    float64
			MonthlyUsageUSD   float64
			DailyLimitUSD     *float64
			WeeklyLimitUSD    *float64
			MonthlyLimitUSD   *float64
			WeeklyWindowStart *string
			ExpiresAt         *string
		}{
			DailyUsageUSD:     typed.Subscription.DailyUsageUSD,
			WeeklyUsageUSD:    typed.Subscription.WeeklyUsageUSD,
			MonthlyUsageUSD:   typed.Subscription.MonthlyUsageUSD,
			DailyLimitUSD:     typed.Subscription.DailyLimitUSD,
			WeeklyLimitUSD:    typed.Subscription.WeeklyLimitUSD,
			MonthlyLimitUSD:   typed.Subscription.MonthlyLimitUSD,
			WeeklyWindowStart: typed.Subscription.WeeklyWindowStart,
			ExpiresAt:         typed.Subscription.ExpiresAt,
		}
	}
	out.balance = typed.Balance
	return out, nil
}

// computeQuotaFromUsage 按「订阅 / 余额 / quota_limited」三种模式归一化 quota_limit/quota_used。
//
// 订阅模式：取月/周/日中首个已配置的限额作为 quota_limit，对应 usage 作为 quota_used。
// 余额模式：若数据库为空（storedLimit<=0）或本次余额大于既有 quota_limit → quota_limit=balance, quota_used=0；
//
//	若本次余额小于既有 quota_limit → quota_used = quota_limit - balance（即 remaining=balance）。
//
// quota_limited 模式：直接透传 limit/used。
func computeQuotaFromUsage(parsed parsedUpstreamQuotaUsage, storedLimit float64) (limit, used, remaining float64, mode string) {
	if parsed.opencode != nil {
		mode = UpstreamQuotaSyncModeOpenCode
		// OpenCode Zen Go 配额为三窗口百分比（无绝对额度），顶层不聚合 quota_limit/quota_used。
		// 窗口数据由 snapshot.OpenCode 承载；此处 limit/used/remaining 保持 0，
		// persistSnapshot 的 opencode 分支会写 opencode_* 快照键。
		return 0, 0, 0, mode
	}
	if parsed.volcengine != nil {
		mode = UpstreamQuotaSyncModeVolcengine
		// 火山方舟配额为多窗口绝对额度/百分比，顶层不聚合 quota_limit/quota_used。
		// 双窗口数据由 snapshot.Volcengine 承载；此处 limit/used/remaining 保持 0，
		// persistSnapshot 的火山分支会写 volcengine_* 快照键。
		return 0, 0, 0, mode
	}
	if parsed.zhipu != nil {
		mode = UpstreamQuotaSyncModeZhipu
		// 智谱配额为百分比（无绝对额度），顶层不聚合 quota_limit/quota_used。
		// 双窗口数据由 snapshot.Zhipu 承载；此处 limit/used/remaining 保持 0，
		// persistSnapshot 的智谱分支会写 zhipu_* 快照键。
		return 0, 0, 0, mode
	}
	if parsed.subscription != nil && parsed.mode != "quota_limited" {
		mode = UpstreamQuotaSyncModeSubscription
		// 订阅模式：limit/used 留给各维度独立展示，顶层不再聚合（避免与月维度重复）。
		// 顶层 Remaining 取所有已配置维度剩余的最小值，供"剩余额度"行展示。
		windows := buildSubscriptionWindows(parsed.subscription, time.Now())
		minRem := -1.0
		for _, w := range windows {
			if w == nil || w.Limit <= 0 {
				continue
			}
			rem := w.Limit - w.Used
			if rem < 0 {
				rem = 0
			}
			if minRem < 0 || rem < minRem {
				minRem = rem
			}
		}
		remaining = minRem
		if remaining < 0 {
			remaining = 0
		}
		return
	}
	if parsed.balance != nil && parsed.mode != "quota_limited" {
		mode = UpstreamQuotaSyncModeBalance
		balance := *parsed.balance
		switch {
		case storedLimit <= 0 || balance > storedLimit:
			limit = balance
			used = 0
			remaining = balance
		case balance < storedLimit:
			limit = storedLimit
			used = storedLimit - balance
			if used < 0 {
				used = 0
			}
			remaining = balance
		default: // balance == storedLimit
			limit = storedLimit
			used = 0
			remaining = balance
		}
		return
	}
	if parsed.quota != nil {
		mode = UpstreamQuotaSyncModeQuotaLimited
		limit = parsed.quota.Limit
		used = parsed.quota.Used
		remaining = parsed.quota.Remaining
		return
	}
	return 0, 0, 0, ""
}

// buildSubscriptionWindows 把上游订阅的日/周/月限额/用量归一化成
// UpstreamQuotaSyncWindow，并计算各维度的 reset_at。now 仅用于兜底日/月窗口起点。
func buildSubscriptionWindows(sub *struct {
	DailyUsageUSD     float64
	WeeklyUsageUSD    float64
	MonthlyUsageUSD   float64
	DailyLimitUSD     *float64
	WeeklyLimitUSD    *float64
	MonthlyLimitUSD   *float64
	WeeklyWindowStart *string
	ExpiresAt         *string
}, now time.Time) []*UpstreamQuotaSyncWindow {
	if sub == nil {
		return nil
	}
	nowUTC := now.UTC()
	var out []*UpstreamQuotaSyncWindow
	if sub.DailyLimitUSD != nil && *sub.DailyLimitUSD > 0 {
		limit := *sub.DailyLimitUSD
		used := sub.DailyUsageUSD
		// 上游未单独提供日窗口起点，用同步时刻兜底；reset = start + 24h。
		start := nowUTC
		out = append(out, &UpstreamQuotaSyncWindow{
			Limit:      limit,
			Used:       used,
			Remaining:  maxFloat64(0, limit-used),
			WindowStart: start.Format(time.RFC3339),
			ResetsAt:   start.Add(24 * time.Hour).Format(time.RFC3339),
		})
	}
	if sub.WeeklyLimitUSD != nil && *sub.WeeklyLimitUSD > 0 {
		limit := *sub.WeeklyLimitUSD
		used := sub.WeeklyUsageUSD
		var start time.Time
		if sub.WeeklyWindowStart != nil {
			if parsed, err := time.Parse(time.RFC3339, *sub.WeeklyWindowStart); err == nil {
				start = parsed.UTC()
			}
		}
		if start.IsZero() {
			start = nowUTC
		}
		out = append(out, &UpstreamQuotaSyncWindow{
			Limit:      limit,
			Used:       used,
			Remaining:  maxFloat64(0, limit-used),
			WindowStart: start.Format(time.RFC3339),
			ResetsAt:   start.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		})
	}
	if sub.MonthlyLimitUSD != nil && *sub.MonthlyLimitUSD > 0 {
		limit := *sub.MonthlyLimitUSD
		used := sub.MonthlyUsageUSD
		// 上游未单独提供月窗口起点，用同步时刻兜底；reset = start + 30d（与既有 subscription_service 月窗口一致）。
		start := nowUTC
		out = append(out, &UpstreamQuotaSyncWindow{
			Limit:      limit,
			Used:       used,
			Remaining:  maxFloat64(0, limit-used),
			WindowStart: start.Format(time.RFC3339),
			ResetsAt:   start.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		})
	}
	return out
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// =================== 账号适配 ===================

func isUpstreamQuotaSyncAccount(account *Account) bool {
	return account != nil && account.Type == AccountTypeAPIKey
}

func upstreamQuotaSyncEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamQuotaSyncEnabledExtraKey].(bool)
	return ok && enabled
}

// IsUpstreamQuotaSyncEnabled 暴露给外部（如显示组件）。
func (a *Account) IsUpstreamQuotaSyncEnabled() bool {
	return upstreamQuotaSyncEnabled(a)
}

// GetUpstreamQuotaSyncSnapshot 返回最近一次同步快照（可能为 nil）。
func (a *Account) GetUpstreamQuotaSyncSnapshot() *UpstreamQuotaSyncSnapshot {
	return decodeUpstreamQuotaSyncSnapshot(a.Extra)
}

func decodeUpstreamQuotaSyncSnapshot(extra map[string]any) *UpstreamQuotaSyncSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[UpstreamQuotaSyncExtraKey]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot UpstreamQuotaSyncSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	return &snapshot
}

// upstreamQuotaSyncAPIKey 返回账号凭据中的 api_key（sub2api 用户 API Key）。
func upstreamQuotaSyncAPIKey(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.TrimSpace(account.GetCredential("api_key"))
}

// upstreamQuotaSyncBaseURL 返回上游 sub2api 实例的 base URL（按平台复用既有取值器）。
func upstreamQuotaSyncBaseURL(account *Account) string {
	if account == nil {
		return ""
	}
	switch account.Platform {
	case PlatformOpenAI:
		return account.GetOpenAIBaseURL()
	case PlatformGrok:
		return account.GetGrokBaseURL()
	case PlatformGemini, PlatformAntigravity:
		return strings.TrimSpace(account.GetCredential("base_url"))
	default:
		return account.GetBaseURL()
	}
}

// nextSyncDelay 复用 upstream_billing_probe 的同名函数语义。
// 这里重新实现一份以避免跨文件符号耦合。
func nextSyncDelay(intervalMinutes int, retryAfterDuration time.Duration) time.Duration {
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval < time.Duration(upstreamQuotaSyncMinInterval)*time.Minute {
		interval = time.Duration(upstreamQuotaSyncMinInterval) * time.Minute
	}
	if interval > upstreamQuotaSyncMaxDelay {
		interval = upstreamQuotaSyncMaxDelay
	}
	if retryAfterDuration > interval {
		return retryAfterDuration
	}
	if interval > upstreamQuotaSyncMaxDelay {
		return upstreamQuotaSyncMaxDelay
	}
	return interval
}

// silence unused import warnings for math when not all branches use it.
var _ = bytes.NewReader
