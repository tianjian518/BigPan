package main

import "testing"

func TestNormalizeCategory(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},                    // 不分类
		{"  ", "", true},                  // 纯空白 = 不分类
		{"国产剧", "国产剧", true},           // 一级（老形态不变）
		{"电视剧/国产剧", "电视剧/国产剧", true}, // 两级
		{"电视剧／欧美剧", "电视剧/欧美剧", true}, // 中文斜杠也认
		{"电视剧\\港剧", "电视剧/港剧", true},   // 反斜杠也认
		{" 电视剧 / 国产剧 ", "电视剧/国产剧", true},
		{"电视剧/", "电视剧", true},   // 尾斜杠 = 只用一级
		{"/国产剧", "国产剧", true},     // 首斜杠 = 只用一级
		{"电视剧//国产剧", "电视剧/国产剧", true},
		{"电视剧/国产剧/", "电视剧/国产剧", true}, // 尾斜杠忽略
		{"电视剧/国产剧/美剧", "", false},          // 三级拒绝
		{"电?视剧/国产剧", "电_视剧/国产剧", true}, // 非法字符按老规矩净化
		{"电视剧/国产:剧", "电视剧/国产_剧", true},
	}
	for _, c := range cases {
		got, err := normalizeCategory(c.in)
		if c.ok && err != nil {
			t.Errorf("normalizeCategory(%q) 不该报错，却返回 %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("normalizeCategory(%q) 该报错，却返回 %q", c.in, got)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeCategory(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestSplitJoinCategory(t *testing.T) {
	cases := []struct{ in, p, c string }{
		{"国产剧", "国产剧", ""},
		{"电视剧/国产剧", "电视剧", "国产剧"},
		{"电视剧 / 欧美剧 ", "电视剧", "欧美剧"},
		{"", "", ""},
	}
	for _, c := range cases {
		p, ch := splitCategory(c.in)
		if p != c.p || ch != c.c {
			t.Errorf("splitCategory(%q) = (%q,%q)，想要 (%q,%q)", c.in, p, ch, c.p, c.c)
		}
		if got := joinCategory(p, ch); got != joinCategory(c.p, c.c) {
			t.Errorf("joinCategory 往返不一致：%q vs %q", got, joinCategory(c.p, c.c))
		}
	}
	// join 的特殊情形：一级空 = 不分类，不管二级
	if got := joinCategory("", "国产剧"); got != "" {
		t.Errorf("joinCategory(\"\",\"国产剧\") = %q，想要空串", got)
	}
}
