package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseTianyiLink(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCode string
		wantPwd  string
		wantErr  bool
	}{
		{"普通链接", "https://cloud.189.cn/web/share?code=abc123DEF", "abc123DEF", "", false},
		{"链接+访问码文本", "https://cloud.189.cn/web/share?code=abc123 提取码:1a2b", "abc123", "1a2b", false},
		{"mergeSharePwd 拼的 pwd 参数", "https://cloud.189.cn/web/share?code=abc123&pwd=1a2b", "abc123", "1a2b", false},
		{"子域名也认", "https://hk.cloud.189.cn/web/share?code=xyz789", "xyz789", "", false},
		{"前后有空白", "  https://cloud.189.cn/web/share?code=abc123  ", "abc123", "", false},
		{"链接后带 & 参数", "https://cloud.189.cn/web/share?code=abc123&from=share", "abc123", "", false},
		{"空输入", "", "", "", true},
		{"不是天翼链接", "https://115.com/s/abc123", "", "", true},
		{"夸克链接不误判", "https://pan.quark.cn/s/abc123def456", "", "", true},
		// 用户实际粘贴的形态：/t/ 短链 + 全角括号访问码
		{"短链+全角括号访问码", "https://cloud.189.cn/t/zuEfyiUfiEZ3（访问码：xx11）", "zuEfyiUfiEZ3", "xx11", false},
		{"短链+半角括号", "https://cloud.189.cn/t/2eEnuyjm2auu(访问码：ucq0)", "2eEnuyjm2auu", "ucq0", false},
		{"短链裸链接", "https://cloud.189.cn/t/BjUZJzUNNfIv", "BjUZJzUNNfIv", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, pwd, err := parseTianyiLink(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际 code=%q pwd=%q", code, pwd)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错: %v", err)
			}
			if code != c.wantCode || pwd != c.wantPwd {
				t.Fatalf("got code=%q pwd=%q, want code=%q pwd=%q", code, pwd, c.wantCode, c.wantPwd)
			}
		})
	}
}

func TestTianyiProviderMatchLink(t *testing.T) {
	p := providerByName("tianyi")
	if p == nil {
		t.Fatal("tianyi provider 未注册")
	}
	if p.Name() != "tianyi" {
		t.Fatalf("Name() = %q", p.Name())
	}
	if !p.MatchLink("https://cloud.189.cn/web/share?code=abc123") {
		t.Error("应匹配天翼分享链接")
	}
	if p.MatchLink("https://115.com/s/abc123") {
		t.Error("不应匹配 115 链接")
	}
	// 天翼直链不绑会话，必须 302
	if !p.Wants302() {
		t.Error("天翼 Wants302 应为 true")
	}
}

func TestTianyiExpiresAt(t *testing.T) {
	now := time.Now().UnixMilli()
	// 相对秒数（7 天）
	got := tianyiExpiresAt(7 * 24 * 3600)
	if got < now || got > now+8*24*3600*1000 {
		t.Fatalf("相对秒数换算错: %d (now=%d)", got, now)
	}
	// 绝对毫秒时间戳（>1e10，原样用）
	abs := now + 3600*1000
	if got2 := tianyiExpiresAt(abs); got2 != abs {
		t.Fatalf("毫秒时间戳应原样返回: got %d want %d", got2, abs)
	}
	// 拿不到按 7 天兜底
	if got3 := tianyiExpiresAt(0); got3 < now || got3 > now+8*24*3600*1000 {
		t.Fatalf("0 值兜底错: %d", got3)
	}
}

func TestTianyiCodeFromURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"https://cloud.189.cn/web/share?code=abc123", "abc123"},
		{"abc123def", "abc123def"}, // 裸 code（6 位以上）
		{"short", ""},              // 太短不当 code
		{"https://example.com/x", ""},
	}
	for _, c := range cases {
		if got := tianyiCodeFromURL(c.in); got != c.want {
			t.Errorf("tianyiCodeFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	if !isAllDigits("12345678901234") {
		t.Error("纯数字应 true")
	}
	if isAllDigits("abc123") {
		t.Error("含字母应 false")
	}
	if isAllDigits("") {
		t.Error("空串应 false")
	}
}

func TestTianyiShareReNotMatchOthers(t *testing.T) {
	// 天翼正则不能把其他网盘链接误判成自己的（findProvider 按注册顺序逐个试）
	for _, link := range []string{
		"https://pan.quark.cn/s/abc",
		"https://115.com/s/abc?password=1234",
		"https://pan.baidu.com/s/abc",
		"https://yun.139.com/s/abc",
	} {
		if tianyiShareRe.MatchString(link) {
			t.Errorf("误匹配: %s", link)
		}
	}
}

func TestTianyiQRURL(t *testing.T) {
	u := tianyiQRURL("https://open.e.189.cn/api/account/qrClinentLogin.do?paras=new_uuid%3Dabc%7C123")
	if !strings.HasPrefix(u, "https://cloud.189.cn/api/portal/loginUrl.action?") {
		t.Errorf("QR URL 形态不对: %s", u)
	}
	if !strings.Contains(u, "uuid=https://open.e.189.cn") {
		t.Errorf("QR URL 缺 uuid: %s", u)
	}
}
