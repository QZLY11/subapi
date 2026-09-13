//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackageRemainUsed 覆盖积分包聚合口径（对齐 workbuddy2api cmd/credit）。
// 这是整个积分展示的数值基础，退化分支最容易静默算错。
func TestPackageRemainUsed(t *testing.T) {
	cases := []struct {
		name         string
		pkg          workBuddyResourcePackage
		wantRemain   int64
		wantUsed     int64
		wantSize     int64
	}{
		{
			name:       "周期包：以 Cycle* 为准",
			pkg:        workBuddyResourcePackage{CycleCapacitySize: 500, CycleCapacityRemain: 410, CycleCapacityUsed: 89},
			wantRemain: 410, wantUsed: 90, wantSize: 500,
		},
		{
			name:       "周期包：remain 为负时钳 0",
			pkg:        workBuddyResourcePackage{CycleCapacitySize: 500, CycleCapacityRemain: -20},
			wantRemain: 0, wantUsed: 500, wantSize: 500,
		},
		{
			name:       "周期包：remain 超 size 时钳到 size",
			pkg:        workBuddyResourcePackage{CycleCapacitySize: 500, CycleCapacityRemain: 900},
			wantRemain: 500, wantUsed: 0, wantSize: 500,
		},
		{
			name:       "周期包：CycleUsed 远大于 size-remain 时取后者（不越界）",
			pkg:        workBuddyResourcePackage{CycleCapacitySize: 100, CycleCapacityRemain: 90, CycleCapacityUsed: 50},
			wantRemain: 50, wantUsed: 50, wantSize: 100,
		},
		{
			name:       "无周期包：退回总量口径",
			pkg:        workBuddyResourcePackage{CapacitySize: 1000, CapacityRemain: 400, CapacityUsed: 600},
			wantRemain: 400, wantUsed: 600, wantSize: 1000,
		},
		{
			name:       "无周期包且 used 缺省：由 size-remain 推导",
			pkg:        workBuddyResourcePackage{CapacitySize: 1000, CapacityRemain: 400},
			wantRemain: 400, wantUsed: 600, wantSize: 1000,
		},
		{
			name:       "全零包：全 0 不 panic",
			pkg:        workBuddyResourcePackage{},
			wantRemain: 0, wantUsed: 0, wantSize: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remain, used, size := packageRemainUsed(tc.pkg)
			require.Equal(t, tc.wantRemain, remain, "remain")
			require.Equal(t, tc.wantUsed, used, "used")
			require.Equal(t, tc.wantSize, size, "size")
		})
	}
}

// TestFetchCredits_AggregatesPackages 端到端校验积分聚合：
// 多包求和 + TotalDosage 作为 size 下限修正。
func TestFetchCredits_AggregatesPackages(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		require.Equal(t, "Bearer tok-123", r.Header.Get("Authorization"))
		require.Equal(t, "uid-abc", r.Header.Get("X-User-Id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalDosage":1500,"Accounts":[
			{"PackageName":"A","CycleCapacitySize":500,"CycleCapacityRemain":410,"CycleCapacityUsed":89},
			{"PackageName":"B","CapacitySize":1000,"CapacityRemain":900,"CapacityUsed":100}
		]}}}}`))
	}))
	defer srv.Close()

	c := NewWorkBuddyClient()
	c.BillingBase = srv.URL
	credits, err := c.FetchCredits(context.Background(), &WorkBuddyAuth{
		AccessToken: "tok-123",
		UID:         "uid-abc",
	})
	require.NoError(t, err)

	require.Equal(t, "/v2/billing/meter/get-user-resource", gotPath)
	require.Equal(t, wbCreditsProductCode, gotBody["ProductCode"])

	// A: remain 410 / used 90 / size 500（CycleUsed 89 < 90 取推导值）
	// B: remain 900 / used 100 / size 1000
	require.Equal(t, int64(1310), credits.Remain)
	require.Equal(t, int64(190), credits.Used)
	require.Equal(t, int64(1500), credits.Size)
	require.Equal(t, int64(1500), credits.TotalDosage)
	require.Len(t, credits.Packages, 2)
}

// TestFetchCredits_TotalDosageRaisesSize 校验 TotalDosage 作为 size 下限：
// 包总和小于 TotalDosage 时以后者为准，并据此修正 used。
func TestFetchCredits_TotalDosageRaisesSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalDosage":5000,"Accounts":[
			{"PackageName":"A","CapacitySize":1000,"CapacityRemain":800,"CapacityUsed":200}
		]}}}}`))
	}))
	defer srv.Close()

	c := NewWorkBuddyClient()
	c.BillingBase = srv.URL
	credits, err := c.FetchCredits(context.Background(), &WorkBuddyAuth{AccessToken: "t"})
	require.NoError(t, err)
	require.Equal(t, int64(800), credits.Remain)
	require.Equal(t, int64(5000), credits.Size, "size 应被 TotalDosage 抬高")
	require.Equal(t, int64(4200), credits.Used, "used 应修正为 size-remain")
}

// TestFetchCredits_NoToken 无凭据时不得发请求。
func TestFetchCredits_NoToken(t *testing.T) {
	c := NewWorkBuddyClient()
	_, err := c.FetchCredits(context.Background(), &WorkBuddyAuth{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "accessToken")
}

// TestDailyCheckin_AlreadyMarkers 校验「今日已签到」的识别边界：
// 只认带分类的 *WorkBuddyError，网络层错误不得被当作已签到。
func TestDailyCheckin_AlreadyMarkers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "中文已签到", err: &WorkBuddyError{Kind: WBErrClient, Status: 400, Msg: "今日已签到"}, want: true},
		{name: "英文 already", err: &WorkBuddyError{Kind: WBErrClient, Status: 400, Msg: "already checked in"}, want: true},
		{name: "code=400", err: &WorkBuddyError{Kind: WBErrClient, Status: 400, Msg: "code=400 msg=bad"}, want: true},
		{name: "其他业务错误", err: &WorkBuddyError{Kind: WBErrClient, Status: 400, Msg: "invalid params"}, want: false},
		{name: "网络层错误不得视为已签", err: fmt.Errorf("connection reset"), want: false},
		{name: "nil 安全", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsWorkBuddyAlreadyCheckin(tc.err))
		})
	}
}

// TestDailyCheckin_PostsExpectedPath 校验签到打到正确的 billing 端点。
func TestDailyCheckin_PostsExpectedPath(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	c := NewWorkBuddyClient()
	c.BillingBase = srv.URL
	require.NoError(t, c.DailyCheckin(context.Background(), &WorkBuddyAuth{AccessToken: "t"}))
	require.Equal(t, "/v2/billing/meter/daily-checkin", gotPath)
	require.Equal(t, http.MethodPost, gotMethod)
}

// TestReportChatActivity_SendsUID 活跃上报必须带 userId（缺失上游静默丢弃）。
func TestReportChatActivity_SendsUID(t *testing.T) {
	var payload []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/report", r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	c := NewWorkBuddyClient()
	c.BillingBase = srv.URL
	require.NoError(t, c.ReportChatActivity(context.Background(), &WorkBuddyAuth{AccessToken: "t", UID: "uid-xyz"}))
	require.Len(t, payload, 1)
	require.Equal(t, "uid-xyz", payload[0]["userId"], "userId 必须是账号 uid")
	require.Equal(t, "chat_request_send", payload[0]["eventCode"])
}

// TestWorkBuddyCheckinSummaryText 汇总计数正确。
func TestWorkBuddyCheckinSummaryText(t *testing.T) {
	results := []*WorkBuddyCheckinResult{
		{Status: "ok"}, {Status: "ok"}, {Status: "already"},
		{Status: "fail"}, {Status: "skipped"}, nil,
	}
	got := WorkBuddyCheckinSummaryText(results)
	require.Contains(t, got, "total=6")
	require.Contains(t, got, "ok=2")
	require.Contains(t, got, "already=1")
	require.Contains(t, got, "fail=1")
	require.Contains(t, got, "skipped=1")
}
