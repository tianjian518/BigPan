package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 默认配置下，任何请求都不能走代取（守住的底线：不改配置 = 行为与改动前一致）。
func TestWantBrowserRelayDefaultOff(t *testing.T) {
	old := strmCfg.BaiduBrowserRelay
	defer func() { strmCfg.BaiduBrowserRelay = old }()
	strmCfg.BaiduBrowserRelay = false

	r := httptest.NewRequest(http.MethodGet, "/direct/a.mkv", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 Chrome/120.0.0.0 Safari/537.36")
	if wantBrowserRelay(&Item{Provider: "baidu"}, r) {
		t.Fatal("开关关闭时不应走代取")
	}
}

// 只对百度生效：139 / 夸克即使在浏览器里也不能被代取（避免白烧流量）。
func TestWantBrowserRelayOnlyBaidu(t *testing.T) {
	old := strmCfg.BaiduBrowserRelay
	defer func() { strmCfg.BaiduBrowserRelay = old }()
	strmCfg.BaiduBrowserRelay = true

	r := httptest.NewRequest(http.MethodGet, "/direct/a.mkv", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 Chrome/120.0.0.0 Safari/537.36")

	if !wantBrowserRelay(&Item{Provider: "baidu"}, r) {
		t.Fatal("百度 + 浏览器 + 开关开启：应走代取")
	}
	for _, p := range []string{"139", "quark", "115"} {
		if wantBrowserRelay(&Item{Provider: p}, r) {
			t.Fatalf("%s 不应走百度代取", p)
		}
	}
}

// 媒体服务器与命令行工具必须始终走 302，不被代取。
func TestWantBrowserRelaySkipsMediaServers(t *testing.T) {
	old := strmCfg.BaiduBrowserRelay
	defer func() { strmCfg.BaiduBrowserRelay = old }()
	strmCfg.BaiduBrowserRelay = true

	uas := []string{
		"Kodi/20.0 (Linux; Android 11) Android/11.0.0 Sys_CPU/arm64-v8a",
		"Emby/4.7.14.0 (Linux; Android 11) Android/11 ExoPlayerLib/2.18.1",
		"Infuse/7.6", "VLC/3.0.20 LibVLC/3.0.20",
		"okhttp/4.9.0", "Dalvik/2.1.0 (Linux; U; Android 11)",
		"curl/7.68.0", "Go-http-client/2.0", "python-requests/2.31",
		"Lavf/59.27.100", "libmpv", "",
	}
	for _, ua := range uas {
		r := httptest.NewRequest(http.MethodGet, "/direct/a.mkv", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if wantBrowserRelay(&Item{Provider: "baidu"}, r) {
			t.Fatalf("媒体服务器/工具不应代取: %q", ua)
		}
	}
}

// 真实浏览器 UA 要能识别。
func TestIsBrowserUA(t *testing.T) {
	yes := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
	}
	for _, ua := range yes {
		if !isBrowserUA(ua) {
			t.Fatalf("应识别为浏览器: %q", ua)
		}
	}
	no := []string{
		"", "netdisk;P2SP;2.2.91.136;android-android;",
		"Kodi/20.0", "okhttp/4.9.0",
	}
	for _, ua := range no {
		if isBrowserUA(ua) {
			t.Fatalf("不应识别为浏览器: %q", ua)
		}
	}
}

// 代取要透传 Range，并按扩展名纠正 Content-Type。
func TestProxyBaiduRelayPassThrough(t *testing.T) {
	var gotUA, gotRange, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotRange = r.Header.Get("Range")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	r := httptest.NewRequest(http.MethodGet, "/direct/movie.mkv", nil)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	proxyBaiduRelay(w, r, srv.URL+"/movie.mkv", baiduNetUA, "")

	if gotUA != baiduNetUA {
		t.Fatalf("代取必须用网盘 UA，实际: %q", gotUA)
	}
	if gotRange != "bytes=0-3" {
		t.Fatalf("Range 未透传: %q", gotRange)
	}
	if gotReferer != "https://pan.baidu.com/" {
		t.Fatalf("Referer 未设置: %q", gotReferer)
	}
	if w.Code != http.StatusPartialContent {
		t.Fatalf("状态码应为 206，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/x-matroska" {
		t.Fatalf("Content-Type 应按扩展名纠正，实际 %q", ct)
	}
	if w.Body.String() != "data" {
		t.Fatalf("响应体未转发: %q", w.Body.String())
	}
}

// 上游报错时不能把上游状态码当成功透传。
func TestProxyBaiduRelayUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	r := httptest.NewRequest(http.MethodGet, "/direct/movie.mkv", nil)
	w := httptest.NewRecorder()
	proxyBaiduRelay(w, r, srv.URL+"/movie.mkv", baiduNetUA, "")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("上游 403 时应回 502，实际 %d", w.Code)
	}
}
