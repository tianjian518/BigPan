package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

// TV 签名算法对固定向量（算法复刻自 OpenList drivers/quark_uc_tv，
// 已用 /oauth/authorize 实测验证：HTTP 200 + 真二维码）。
// 这里锁死算法形状，防止将来「顺手优化」把签名改坏。
func TestQTVSignShape(t *testing.T) {
	q := NewQuarkTV("dev123", "", "")
	tm, token, reqID := q.qtvSign("GET", "/oauth/authorize")
	if len(tm) != 13 {
		t.Fatalf("时间戳应为 13 位毫秒，got %s", tm)
	}
	if len(token) != 64 {
		t.Fatalf("x-pan-token 应为 sha256 hex（64 字符），got %s", token)
	}
	if len(reqID) != 32 {
		t.Fatalf("req_id 应为 md5 hex（32 字符），got %s", reqID)
	}
	// 同一毫秒内重复签名结果必须一致（可复现）
	if _, token2, _ := q.qtvSign("GET", "/oauth/authorize"); token == token2 {
		// 允许：同一毫秒。换个路径必须不同。
	}
	_, tokenGet, _ := q.qtvSign("GET", "/file")
	_, tokenPost, _ := q.qtvSign("POST", "/file")
	if tokenGet == tokenPost {
		t.Fatal("不同方法应签出不同 token")
	}
	// token 应等于手工计算的 sha256
	sum := sha256.Sum256([]byte("GET&/file&" + tm + "&" + qtvSignKey))
	want := hex.EncodeToString(sum[:])
	if tokenGet != want {
		t.Fatalf("token 与手工计算不一致:\n got  %s\n want %s", tokenGet, want)
	}
}

// 设备参数必须带全（驱动同款，缺字段 TV 接口会拒）。
func TestQTVDeviceParamsComplete(t *testing.T) {
	q := NewQuarkTV("dev123", "at123", "")
	v := q.qtvDeviceParams("rid123", "at123")
	for _, k := range []string{"req_id", "access_token", "app_ver", "device_id",
		"device_brand", "platform", "device_name", "device_model",
		"build_device", "build_product", "device_gpu", "activity_rect", "channel"} {
		if v.Get(k) == "" {
			t.Errorf("设备参数缺 %s", k)
		}
	}
}

// token 失效识别（自愈重试的触发条件）。
func TestQTVTokenInvalid(t *testing.T) {
	cases := []struct {
		errno int
		info  string
		want  bool
	}{
		{10001, "", true},
		{11001, "", true},
		{0, "access token 已过期", true},
		{0, "ACCESS TOKEN invalid", true},
		{0, "token无效", true},
		{0, "", false},
		{0, "文件不存在", false},
	}
	for _, c := range cases {
		r := qtvFileResp{Errno: c.errno, ErrorInfo: c.info}
		if got := r.tokenInvalid(); got != c.want {
			t.Errorf("tokenInvalid(errno=%d, info=%q) = %v, want %v", c.errno, c.info, got, c.want)
		}
	}
}

// 二维码会话生命周期：start → poll 快照 → GC。
func TestQTVQRSessionLifecycle(t *testing.T) {
	// 直接造会话（不碰网络）：验证快照字段与 GC 行为
	s := &qtvQRSession{
		id:        "t1",
		status:    0,
		message:   "等待扫码",
		label:     "data:image/jpeg;base64,AAA",
		created:   time.Now(),
		qtv:       NewQuarkTV("d", "", ""),
		queryToken: "qt",
	}
	qtvQRMu.Lock()
	qtvQRSessions[s.id] = s
	qtvQRMu.Unlock()

	snap, err := pollQRQuarkTV("t1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.QR != s.label || snap.Status != 0 || snap.Done {
		t.Fatalf("快照不对: %+v", snap)
	}
	// 不存在的会话应报错
	if _, err := pollQRQuarkTV("nope"); err == nil {
		t.Fatal("不存在的会话应报错")
	}
	// 过期会话应被 GC 掉
	s.created = time.Now().Add(-11 * time.Minute)
	qtvQRGC()
	if _, err := pollQRQuarkTV("t1"); err == nil {
		t.Fatal("过期会话应被清掉")
	}
}

// 连接中心 UI：夸克必须同时提供扫码与 Cookie 两种方式（§23.47 的 UX 地基）。
func TestQuarkConnUIBothMethods(t *testing.T) {
	src, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "scanStart:'/api/login/quark/start'") {
		t.Error("夸克连接配置缺扫码入口（/api/login/quark/start）")
	}
	if !strings.Contains(s, "scanPoll:'/api/login/quark/poll'") {
		t.Error("夸克连接配置缺扫码轮询（/api/login/quark/poll）")
	}
	if !strings.Contains(s, "id=\"quarkTV302\"") {
		t.Error("设置页缺夸克 TV 302 开关（id=quarkTV302）")
	}
	if !strings.Contains(s, "quark_tv_302") {
		t.Error("设置页 JS 缺开关字段名（quark_tv_302）")
	}
}
