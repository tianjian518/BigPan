package main

import "testing"

// 目录层级三种布局：别人的媒体库习惯不同，不能写死。
func TestStrmItemDirLayouts(t *testing.T) {
	it := &Item{Dir: "百度/国产剧/雷霆令"}
	cases := []struct{ layout, want string }{
		{"full", "百度/国产剧/雷霆令"},
		{"no_provider", "国产剧/雷霆令"},
		{"show_only", "雷霆令"},
		{"", "百度/国产剧/雷霆令"}, // 空值等同 full
	}
	old := strmCfg.Layout
	defer func() { strmCfg.Layout = old }()
	for _, c := range cases {
		strmCfg.Layout = c.layout
		if got := strmItemDir(it); got != c.want {
			t.Errorf("layout=%q got %q want %q", c.layout, got, c.want)
		}
	}
}

// 配置里的目录 / 对外地址优先于启动参数；留空时回退，保证老部署行为不变。
func TestStrmConfigOverrides(t *testing.T) {
	old := strmCfg
	defer func() { strmCfg = old }()

	// 回退
	strmCfg.Dir, strmCfg.BaseURL = "", ""
	if got := strmRootDir(); got != strmRoot {
		t.Errorf("空配置应回退启动参数 %q，实际 %q", strmRoot, got)
	}
	if got := strmPublicBase("http://192.168.1.10:8099/"); got != "http://192.168.1.10:8099" {
		t.Errorf("空配置应回退 self 并去尾斜杠，实际 %q", got)
	}

	// 覆盖
	strmCfg.Dir = "/vol1/1000/media"
	strmCfg.BaseURL = "https://pan.example.com/"
	if got := strmRootDir(); got != "/vol1/1000/media" {
		t.Errorf("应使用配置目录，实际 %q", got)
	}
	if got := strmPublicBase("http://192.168.1.10:8099"); got != "https://pan.example.com" {
		t.Errorf("应使用配置地址并去尾斜杠，实际 %q", got)
	}
}
