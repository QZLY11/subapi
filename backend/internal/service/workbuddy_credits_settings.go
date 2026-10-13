package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// WorkBuddyCreditsSettings 控制 WorkBuddy 积分快照的查询行为，管理端 UI 可改。
//
// 语义关键点：RefreshIntervalMinutes 是【同一个账号】两次查询之间的间隔，
// 与账号总数无关。N 个账号的到期时刻由账号 ID 哈希铺开在周期内，互不重叠。
type WorkBuddyCreditsSettings struct {
	// BackgroundRefreshEnabled 是否开启后台自动查询。
	// 关闭后快照不再自动更新，只在管理端点「查询全部积分」时打上游。
	BackgroundRefreshEnabled bool `json:"background_refresh_enabled"`

	// RefreshIntervalMinutes 单账号刷新周期（分钟）：同一账号每多久查一次。
	RefreshIntervalMinutes int `json:"refresh_interval_minutes"`

	// MaxPerTick 单轮扫描处理上限。防止账号规模把上游压垮：
	// 账号数超过 MaxPerTick × (周期/分钟) 之后，实际周期自动拉长。
	MaxPerTick int `json:"max_per_tick"`
}

const (
	workBuddyCreditsMinIntervalMinutes = 1
	workBuddyCreditsMaxIntervalMinutes = 1440 // 24h
	workBuddyCreditsMinMaxPerTick      = 1
	workBuddyCreditsMaxMaxPerTick      = 200
)

func defaultWorkBuddyCreditsSettings() *WorkBuddyCreditsSettings {
	return &WorkBuddyCreditsSettings{
		BackgroundRefreshEnabled: true,
		RefreshIntervalMinutes:   int(wbCreditsRefreshInterval / time.Minute),
		MaxPerTick:               wbCreditsRefreshMaxPerTick,
	}
}

func normalizeWorkBuddyCreditsSettings(s *WorkBuddyCreditsSettings) {
	if s == nil {
		return
	}
	if s.RefreshIntervalMinutes < workBuddyCreditsMinIntervalMinutes {
		s.RefreshIntervalMinutes = workBuddyCreditsMinIntervalMinutes
	}
	if s.RefreshIntervalMinutes > workBuddyCreditsMaxIntervalMinutes {
		s.RefreshIntervalMinutes = workBuddyCreditsMaxIntervalMinutes
	}
	if s.MaxPerTick < workBuddyCreditsMinMaxPerTick {
		s.MaxPerTick = workBuddyCreditsMinMaxPerTick
	}
	if s.MaxPerTick > workBuddyCreditsMaxMaxPerTick {
		s.MaxPerTick = workBuddyCreditsMaxMaxPerTick
	}
}

// GetWorkBuddyCreditsSettings 读取后台刷新设置；设置缺失时返回默认值。
func (s *SettingService) GetWorkBuddyCreditsSettings(ctx context.Context) (*WorkBuddyCreditsSettings, error) {
	defaults := defaultWorkBuddyCreditsSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyWorkBuddyCreditsSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get workbuddy credits settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse workbuddy credits settings: %w", err)
	}
	normalizeWorkBuddyCreditsSettings(&settings)
	return &settings, nil
}

// SetWorkBuddyCreditsSettings 校验并持久化后台刷新设置。
func (s *SettingService) SetWorkBuddyCreditsSettings(ctx context.Context, settings *WorkBuddyCreditsSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if settings == nil {
		return infraerrors.BadRequest("INVALID_WORKBUDDY_CREDITS_SETTINGS", "settings cannot be nil")
	}
	if settings.RefreshIntervalMinutes < workBuddyCreditsMinIntervalMinutes ||
		settings.RefreshIntervalMinutes > workBuddyCreditsMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_WORKBUDDY_CREDITS_INTERVAL",
			fmt.Sprintf("refresh_interval_minutes must be between %d and %d",
				workBuddyCreditsMinIntervalMinutes, workBuddyCreditsMaxIntervalMinutes))
	}
	if settings.MaxPerTick < workBuddyCreditsMinMaxPerTick ||
		settings.MaxPerTick > workBuddyCreditsMaxMaxPerTick {
		return infraerrors.BadRequest(
			"INVALID_WORKBUDDY_CREDITS_MAX_PER_TICK",
			fmt.Sprintf("max_per_tick must be between %d and %d",
				workBuddyCreditsMinMaxPerTick, workBuddyCreditsMaxMaxPerTick))
	}
	normalizeWorkBuddyCreditsSettings(settings)
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal workbuddy credits settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyWorkBuddyCreditsSettings, string(data))
}
