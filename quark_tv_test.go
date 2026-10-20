package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 造一个带夸克/百度分享条目的 store（照 strm_sync_test 的模式）。
func setupQuarkTVTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"q1","title":"第01集","provider":"quark","dir":"夸克/国产剧/甲","share_id":"s1","passcode":"","file_id":"f1","ext":".mp4","size":1},
{"id":"b1","title":"第01集","provider":"baidu","dir":"百度/国产剧/乙","share_id":"s2","file_id":"f2","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	// 设置与账号都指到临时目录，避免动真实文件
	oldPath := appSettingsPath
	loadAppSettings(filepath.Join(dir, "settings.json"))
	t.Cleanup(func() { appSettingsPath = oldPath })
	// 全局账号库注入（provider 取凭证用），测试里是空库 = 都没登录
	oldAcc := globalAccounts
	globalAccounts, _ = NewAccountStore(filepath.Join(dir, "accounts.json"))
	t.Cleanup(func() { globalAccounts = oldAcc })
	return s
}

// 开关关着：夸克写 /play/；开着：夸克也写 /direct/（存量 strm 双向兼容的地基）。
func TestItemPlayURLQuarkToggle(t *testing.T) {
	s := setupQuarkTVTest(t)
	q := s.FindByID("q1")
	b := s.FindByID("b1")
	if q == nil || b == nil {
		t.Fatal("条目没造出来")
	}

	setQuarkTV302(false)
	if u := itemPlayURL(q, "http://1.2.3.4:8099"); !strings.Contains(u, "/play/") {
		t.Fatalf("开关关着夸克应写 /play/，got %s", u)
	}
	if u := itemPlayURL(b, "http://1.2.3.4:8099"); !strings.Contains(u, "/direct/") {
		t.Fatalf("百度应写 /direct/，got %s", u)
	}

	setQuarkTV302(true)
	u := itemPlayURL(q, "http://1.2.3.4:8099")
	if !strings.Contains(u, "/direct/") || !strings.Contains(u, "k=") {
		t.Fatalf("开关开着夸克应写 /direct/ 且带路径令牌，got %s", u)
	}
}

// Wants302 应跟随开关（WebDAV 侧零改动自动跟随的地基）。
func TestQuarkWants302FollowsToggle(t *testing.T) {
	setupQuarkTVTest(t)
	var p quarkProvider
	setQuarkTV302(false)
	if p.Wants302() {
		t.Fatal("开关关着不应 302")
	}
	setQuarkTV302(true)
	if !p.Wants302() {
		t.Fatal("开关开着应 302")
	}
}

// /play 处理器分叉：开关开着 + 夸克 + 没扫码 → 报错引导去扫码（不回落中转）。
// 开关关着 → 走老路，报错引导去导 Cookie。两条错误信息不同，正好验证分叉。
func TestHandlePlayQuarkBranch(t *testing.T) {
	s := setupQuarkTVTest(t)
	as, _ := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	h := handlePlay(s, as)

	// 开关开着：应走进 quarkTVPlayTarget，因没有 TV 账号报「扫码」
	setQuarkTV302(true)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/play?id=q1", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "扫码") {
		t.Fatalf("开关开着应引导扫码，got %d %s", rec.Code, rec.Body.String())
	}

	// 开关关着：老路，报「导入夸克凭证」
	setQuarkTV302(false)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/play?id=q1", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "夸克凭证") {
		t.Fatalf("开关关着应引导导 Cookie，got %d %s", rec.Code, rec.Body.String())
	}
}

// /direct 反向兼容：开关关着 + 夸克 → 内部转 /play（中转接手），不回 302 死链。
func TestDirectGuardQuarkRedirectsToPlay(t *testing.T) {
	s := setupQuarkTVTest(t)
	h := directGuard(s)

	it := s.FindByID("q1")
	if it == nil {
		t.Fatal("条目没造出来")
	}
	// 带路径令牌免登录（媒体服务器场景）
	tok := strmPathToken(it.FullPath())
	url := "/direct/" + strings.TrimPrefix(it.FullPath(), "/") + "?k=" + tok

	setQuarkTV302(false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("开关关着夸克 /direct 应转 /play，got %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/play/") || !strings.Contains(loc, "id=q1") {
		t.Fatalf("转跳目标不对: %s", loc)
	}
}

// 设置 API：GET 读、POST 改（改完开关立即生效）。
func TestSettingsAPI(t *testing.T) {
	setupQuarkTVTest(t)
	h := apiSettings()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var got struct {
		OK       bool        `json:"ok"`
		Settings AppSettings `json:"settings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.OK {
		t.Fatal("GET 应 ok")
	}

	body := strings.NewReader(`{"quark_tv_302":true}`)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/settings", body))
	if rec.Code != http.StatusOK || !quarkTV302Enabled() {
		t.Fatalf("POST 后开关应生效，got %d", rec.Code)
	}
	setQuarkTV302(false) // 收尾，别影响其他测试
}

// ---- 换 token 回归（2026-10-20 线上事故）----

// pointCodeAPI 把 token 域名指到测试服务器，返回的清理函数恢复原值。
func pointCodeAPI(t *testing.T, url string) {
	t.Helper()
	old := qtvCodeAPI
	qtvCodeAPI = url
	t.Cleanup(func() { qtvCodeAPI = old })
}

// 事故回归：换 token 请求必须带 User-Agent。
// api.extscreen.com 前的 WAF 对无 UA 请求回 403 HTML 挑战页（Go 的 http.Client
// 默认不发 UA 头），曾导致扫码确认后报「夸克TV登录返回无法识别」。
// OpenList 驱动走 resty 框架自带默认 UA 所以从未暴露。
func TestQtvExchangeTokenSendsUA(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "OK",
			"data": map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 7200},
		})
	}))
	defer srv.Close()
	pointCodeAPI(t, srv.URL)

	q := NewQuarkTV("dev-1", "", "")
	if err := q.qtvExchangeToken(context.Background(), "the-code", false); err != nil {
		t.Fatalf("换 token 应成功，got %v", err)
	}
	if gotUA != qtvUA {
		t.Fatalf("请求必须带 TV UA（WAF 拦无 UA 请求），got %q", gotUA)
	}
	if q.accessTokenOrEmpty() != "at-1" {
		t.Fatal("access_token 没存进客户端")
	}
}

// WAF 挑战页（403 HTML）不能再被一句「无法识别」带过——报错必须带 HTTP 状态和体片段。
func TestQtvExchangeTokenWAFHTMLDiagnosable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html;charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html><body>WAF challenge page</body></html>"))
	}))
	defer srv.Close()
	pointCodeAPI(t, srv.URL)

	q := NewQuarkTV("dev-1", "", "")
	err := q.qtvExchangeToken(context.Background(), "the-code", false)
	if err == nil {
		t.Fatal("403 HTML 应报错")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "WAF challenge") {
		t.Fatalf("报错应带 HTTP 状态与响应片段便于定位，got %v", err)
	}
}

// 无效 code 时顶层 code 仍是 200，真错误在 data.error_info——不能误报「登录失败：OK」。
func TestQtvExchangeTokenInvalidCodeMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "OK",
			"data": map[string]any{"status": -1, "errno": 11004, "error_info": "授权码Code无效"},
		})
	}))
	defer srv.Close()
	pointCodeAPI(t, srv.URL)

	q := NewQuarkTV("dev-1", "", "")
	err := q.qtvExchangeToken(context.Background(), "bad-code", false)
	if err == nil || !strings.Contains(err.Error(), "授权码Code无效") {
		t.Fatalf("无效 code 应报 data.error_info，got %v", err)
	}
}

// TestQtvStableDeviceIDReusesAccount 设备号复用逻辑（「设备数超限」事故的回归）：
//  1. 无账号时生成稳定设备号并持久化（重读不变）；
//  2. 有账号时优先复用账号里的设备号（重扫不占新名额）；
//  3. 账号设备号被清空时回落到持久化设备号（老账号升级路径）。
func TestQtvStableDeviceIDReusesAccount(t *testing.T) {
	dir := t.TempDir()
	as, err := NewAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}

	id1 := qtvStableDeviceID(as)
	if len(id1) != 32 {
		t.Fatalf("设备号应为 32 位十六进制，得到 %q", id1)
	}
	if id2 := qtvStableDeviceID(as); id2 != id1 {
		t.Fatalf("无账号时重读应得到同一设备号：%s != %s", id2, id1)
	}
	// 持久化文件确实写盘了（换个 AccountStore 实例读同一目录也要一样）
	as2, _ := NewAccountStore(filepath.Join(dir, "accounts.json"))
	if id3 := qtvStableDeviceID(as2); id3 != id1 {
		t.Fatalf("重启后应读回同一设备号：%s != %s", id3, id1)
	}

	as.Put(Account{Provider: "quark_tv", Cookie: "rt", DeviceID: "acctdev00000000000000000000000001"})
	if got := qtvStableDeviceID(as); got != "acctdev00000000000000000000000001" {
		t.Fatalf("有账号时应复用账号设备号，得到 %q", got)
	}

	as.Put(Account{Provider: "quark_tv", Cookie: "rt", DeviceID: ""})
	if got := qtvStableDeviceID(as); got != id1 {
		t.Fatalf("账号设备号为空应回落到持久化设备号 %s，得到 %q", id1, got)
	}
}

// pointAPIBase 把 TV 开放接口域名指到 httptest（qtvAPIBase 因此是 var）。
func pointAPIBase(t *testing.T, url string) {
	t.Helper()
	old := qtvAPIBase
	qtvAPIBase = url
	t.Cleanup(func() { qtvAPIBase = old })
}

// TestQtvStreamingURLPicksBestResolution 网页播放走转码流：video_info 可能带
// 多档分辨率，必须取像素总数最大的一档（浏览器放不了低清糊画面）。
func TestQtvStreamingURLPicksBestResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file" {
			t.Errorf("路径应为 /file，got %s", r.URL.Path)
		}
		if r.URL.Query().Get("method") != "streaming" {
			t.Errorf("转码流应带 method=streaming，got %q", r.URL.Query().Get("method"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "errno": 0,
			"data": map[string]any{
				"default_resolution": "normal",
				"video_info": []map[string]any{
					{"resolution": "low", "width": 640, "height": 360, "size": 50000000, "url": "https://cdn.example/low.mp4"},
					{"resolution": "super", "width": 1920, "height": 1080, "size": 254822837, "url": "https://cdn.example/super.mp4"},
					{"resolution": "high", "width": 1280, "height": 720, "size": 120000000, "url": "https://cdn.example/high.mp4"},
				},
			},
		})
	}))
	defer srv.Close()
	pointAPIBase(t, srv.URL)

	q := NewQuarkTV("dev-1", "at-1", "")
	u, err := q.StreamingURL(context.Background(), "fid-x")
	if err != nil {
		t.Fatalf("取转码流应成功，got %v", err)
	}
	if u != "https://cdn.example/super.mp4" {
		t.Fatalf("应取分辨率最高的一档（super 1080p），got %q", u)
	}
}

// TestQtvPickURLWebPrefersStreaming 网页模式取链策略：
//  1. web=true 且转码流可用 → 转码流（浏览器可解的 H.264 MP4）；
//  2. web=true 但转码流没有 → 回落原画（万一片源本身就是 MP4/H.264）；
//  3. web=false（原生播放器）→ 原画，清晰度优先。
func TestQtvPickURLWebPrefersStreaming(t *testing.T) {
	fileH := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("method") == "streaming" {
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200, "errno": 0,
				"data": map[string]any{"video_info": []map[string]any{
					{"resolution": "super", "width": 1920, "height": 1080, "url": "https://cdn.example/stream.mp4"},
				}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": 200, "errno": 0,
			"data": map[string]any{"download_url": "https://cdn.example/origin.mkv"},
		})
	}

	t.Run("网页模式取转码流", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(fileH))
		defer srv.Close()
		pointAPIBase(t, srv.URL)
		u, err := qtvPickURL(context.Background(), NewQuarkTV("dev-1", "at-1", ""), "fid", true)
		if err != nil || u != "https://cdn.example/stream.mp4" {
			t.Fatalf("网页模式应取转码流，got %q err=%v", u, err)
		}
	})

	t.Run("转码流缺失回落原画", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("method") == "streaming" {
				// 夸克对没转码的文件回错误（如「不支持转码」）
				json.NewEncoder(w).Encode(map[string]any{"status": -1, "errno": 32010, "error_info": "文件不支持转码"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200, "errno": 0,
				"data": map[string]any{"download_url": "https://cdn.example/origin.mkv"},
			})
		}))
		defer srv.Close()
		pointAPIBase(t, srv.URL)
		u, err := qtvPickURL(context.Background(), NewQuarkTV("dev-1", "at-1", ""), "fid", true)
		if err != nil || u != "https://cdn.example/origin.mkv" {
			t.Fatalf("转码流失败应回落原画，got %q err=%v", u, err)
		}
	})

	t.Run("原生播放器只取原画", func(t *testing.T) {
		var sawStreaming bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("method") == "streaming" {
				sawStreaming = true
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": 200, "errno": 0,
				"data": map[string]any{"download_url": "https://cdn.example/origin.mkv"},
			})
		}))
		defer srv.Close()
		pointAPIBase(t, srv.URL)
		u, err := qtvPickURL(context.Background(), NewQuarkTV("dev-1", "at-1", ""), "fid", false)
		if err != nil || u != "https://cdn.example/origin.mkv" {
			t.Fatalf("原生播放器应取原画，got %q err=%v", u, err)
		}
		if sawStreaming {
			t.Fatal("原生播放器不应打转码流接口（浪费一次 API 调用）")
		}
	})
}
