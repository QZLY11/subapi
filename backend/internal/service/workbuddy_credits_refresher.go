package service

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// WorkBuddyCreditsRefresher 后台错开刷新 WorkBuddy 账号的积分快照。
//
// 背景：WorkBuddy 账号的积分只能靠打上游 meter 接口获得，而该接口按账号逐个
// 查询有明显的累计耗时（62 个账号串行可达 2 分钟）。若把它放在管理页加载路径
// 上，页面必然卡死。
//
// 因此改为：
//   - 页面加载只读 extra 快照（QueryCreditsSnapshot，毫秒级）；
//   - 本服务按管理端设置的周期（默认 10 分钟/账号，见 WorkBuddyCreditsSettings）
//     用账号 ID 的稳定相位错开，每 tick 只处理配额内的到期账号，把上游压力
//     摊平到时间轴上；
//   - 管理员需要最新值时，点「查询全部积分」显式调 QueryAllCredits。
//
// 开关与参数都在管理端 UI 配置（settings 表），改完下一 tick 即生效，无需重启。
type WorkBuddyCreditsRefresher struct {
	creditsSvc *WorkBuddyCreditsService
	settingSvc *SettingService

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewWorkBuddyCreditsRefresher 构造后台刷新服务。
func NewWorkBuddyCreditsRefresher(
	creditsSvc *WorkBuddyCreditsService,
	settingSvc *SettingService,
) *WorkBuddyCreditsRefresher {
	return &WorkBuddyCreditsRefresher{
		creditsSvc: creditsSvc,
		settingSvc: settingSvc,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// ProvideWorkBuddyCreditsRefresher 构造并启动后台刷新服务（wire provider）。
//
// 与 ProvideUpstreamBillingProbeService 一致：启动动作放在 provider 内部，
// 保证只要该服务被装配进依赖图就一定会跑，不依赖调用方记得手动 Start。
func ProvideWorkBuddyCreditsRefresher(
	creditsSvc *WorkBuddyCreditsService,
	settingSvc *SettingService,
) *WorkBuddyCreditsRefresher {
	r := NewWorkBuddyCreditsRefresher(creditsSvc, settingSvc)
	r.Start()
	return r
}

// Start 启动后台循环（幂等）。
//
// 注意：这里不再判断开关 —— 开关与周期由管理端设置控制，每轮 tick 实时读取，
// 因此在 UI 上改完下一轮即生效，不需要重启服务。
func (r *WorkBuddyCreditsRefresher) Start() {
	if r == nil || r.creditsSvc == nil {
		return
	}
	r.startOnce.Do(func() {
		go r.loop()
		logger.LegacyPrintf("service.workbuddy_credits_refresher",
			"[WorkBuddyCredits] 后台刷新服务已启动 (扫描=%s)，开关/周期以管理端设置为准",
			wbCreditsRefreshTick)
	})
}

// wbCreditsBackgroundRefreshEnabled 判断后台积分刷新是否启用。
//
// 设为 0/false/off/no/disabled 即完全关闭后台查询：快照不再自动更新，只有
// 管理员在账号管理页点「刷新积分」时才打上游。账号数极大（上千）或上游风控
// 敏感时用这个开关整段止损，不必改代码。
func wbCreditsBackgroundRefreshEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WORKBUDDY_CREDITS_BACKGROUND_REFRESH"))) {
	case "0", "false", "off", "no", "disabled":
		return false
	}
	return true
}

// Stop 停止后台循环并等待退出。
func (r *WorkBuddyCreditsRefresher) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		select {
		case <-r.doneCh:
		case <-time.After(5 * time.Second):
		}
	})
}

func (r *WorkBuddyCreditsRefresher) loop() {
	defer close(r.doneCh)
	ticker := time.NewTicker(wbCreditsRefreshTick)
	defer ticker.Stop()
	// 启动后先跑一轮，让从未采集过的账号尽快有快照可读。
	r.tick()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

func (r *WorkBuddyCreditsRefresher) tick() {
	// 单轮预算：处理量由 settings.MaxPerTick 封顶（默认 30），4 并发 × 每个
	// 最多 20s 上游超时 → 最坏约 150s，5 分钟预算留足余量，确保一轮不会拖到
	// 下一 tick 之后。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 环境变量可强制关闭，优先于 UI 设置（运维级止损后路）。
	if !wbCreditsBackgroundRefreshEnabled() {
		return
	}

	settings, err := r.currentSettings(ctx)
	if err != nil {
		logger.LegacyPrintf("service.workbuddy_credits_refresher",
			"[WorkBuddyCredits] 读取设置失败: %v", err)
		return
	}
	if settings == nil || !settings.BackgroundRefreshEnabled {
		// 后台自动查询已在管理端关闭：快照只在点「查询全部积分」时更新。
		return
	}

	n, err := r.creditsSvc.RefreshDueAccounts(ctx, settings)
	if err != nil {
		logger.LegacyPrintf("service.workbuddy_credits_refresher",
			"[WorkBuddyCredits] 刷新失败: %v", err)
		return
	}
	if n > 0 {
		logger.LegacyPrintf("service.workbuddy_credits_refresher",
			"[WorkBuddyCredits] 本轮刷新 %d 个账号积分 (周期=%dmin, 上限=%d)",
			n, settings.RefreshIntervalMinutes, settings.MaxPerTick)
	}
}

// currentSettings 读取当前生效设置；未装配 SettingService 时退回默认值。
func (r *WorkBuddyCreditsRefresher) currentSettings(ctx context.Context) (*WorkBuddyCreditsSettings, error) {
	if r.settingSvc == nil {
		return defaultWorkBuddyCreditsSettings(), nil
	}
	return r.settingSvc.GetWorkBuddyCreditsSettings(ctx)
}
