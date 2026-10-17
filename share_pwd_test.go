package main

import "testing"

// TestMergeSharePwd 覆盖「提取码单独填」的三种形态：
//  ① 裸链 + 单独提取码 → 拼上 ?pwd=
//  ② 链接已带 ?pwd= → 不重复拼
//  ③ 链接已带其他 query（如 ?from=x）→ 用 & 分隔拼 pwd
//  ④ 提取码为空 → 原样返回
func TestMergeSharePwd(t *testing.T) {
	cases := []struct{ link, pwd, want string }{
		{"https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv", "1016",
			"https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=1016"},
		{"https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=1016", "1016",
			"https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=1016"},
		{"https://pan.quark.cn/s/abc123?from=share", "wxyz",
			"https://pan.quark.cn/s/abc123?from=share&pwd=wxyz"},
		{"https://115.com/s/abc123xyz", "", "https://115.com/s/abc123xyz"},
		{"https://pan.baidu.com/s/1xxx", "  ", "https://pan.baidu.com/s/1xxx"},
	}
	for _, c := range cases {
		if got := mergeSharePwd(c.link, c.pwd); got != c.want {
			t.Errorf("mergeSharePwd(%q, %q) = %q, want %q", c.link, c.pwd, got, c.want)
		}
	}
}

// TestParseShareLinkBaiduShort 百度 /s/1xxx?pwd= 短链的链接解析。
func TestParseShareLinkBaiduShort(t *testing.T) {
	for _, c := range []struct{ link, wantSur, wantPwd string }{
		{"https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=1016", "1AbCdEfGhIjKlMnOpQrStUv", "1016"},
		{"https://pan.baidu.com/s/1XyZ0123456789abcdefghij?pwd=1012", "1XyZ0123456789abcdefghij", "1012"},
	} {
		sur, pwd, err := baiduParseShareLink(c.link)
		if err != nil {
			t.Fatalf("baiduParseShareLink(%q) 报错: %v", c.link, err)
		}
		if sur != c.wantSur || pwd != c.wantPwd {
			t.Errorf("baiduParseShareLink(%q) = (%q, %q), want (%q, %q)", c.link, sur, pwd, c.wantSur, c.wantPwd)
		}
	}
}
