package main

import (
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
