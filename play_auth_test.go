package main

import (
	"net/url"
	"testing"
)

// TestPlayTokenRoundTrip /play 路径令牌的回环一致性：
// directGuard 重定向时按「/play/ + 原始文件名」签令牌，handlePlay 收到后按
// r.URL.Path 验令牌 —— 中间隔着 url.PathEscape/Parse 一轮转义，
// 这里钉住两者必须对得上（含空格、中文、#? 等会被转义的字符）。
func TestPlayTokenRoundTrip(t *testing.T) {
	names := []string{
		"S01E01.mkv",
		"玉簟秋 第 1 集.mkv", // 空格 + 中文
		"巅峰之夜 (2020).mkv",  // 括号
		"a#b?c.mkv",           // # 和 ?：转义敏感字符
	}
	for _, name := range names {
		redirect := "/play/" + url.PathEscape(name) + "?id=x&k=" + strmPathToken("/play/"+name)
		u, err := url.Parse(redirect)
		if err != nil {
			t.Fatalf("parse %q: %v", redirect, err)
		}
		if u.Path != "/play/"+name {
			t.Errorf("Path 回环不一致: got %q want %q", u.Path, "/play/"+name)
		}
		if u.Query().Get("k") != strmPathToken(u.Path) {
			t.Errorf("令牌与路径不匹配: name=%q path=%q", name, u.Path)
		}
	}
}

// TestPlayTokenRejectsWrongPath 令牌只对签发时的路径有效：换个路径必须拒。
func TestPlayTokenRejectsWrongPath(t *testing.T) {
	tok := strmPathToken("/play/甲剧/S01E01.mkv")
	if tok == strmPathToken("/play/乙剧/S01E01.mkv") {
		t.Error("不同路径签出了相同令牌")
	}
	if tok == "" {
		t.Error("令牌为空")
	}
}
