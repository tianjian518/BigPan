package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 设置文件读写 + 开关语义（夸克 TV 302 的地基）。
func TestAppSettingsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")

	// 文件不存在：默认全关，并落盘一份
	loadAppSettings(p)
	if quarkTV302Enabled() {
		t.Fatal("默认应为关闭")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("首次加载应落盘一份默认设置: %v", err)
	}

	// 写一份开着的，重新加载应读进来
	os.WriteFile(p, []byte(`{"quark_tv_302":true}`), 0644)
	loadAppSettings(p)
	if !quarkTV302Enabled() {
		t.Fatal("应读到开启")
	}

	// setQuarkTV302 改内存 + 落盘
	setQuarkTV302(false)
	if quarkTV302Enabled() {
		t.Fatal("setQuarkTV302(false) 后应关闭")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "{\n  \"quark_tv_302\": false\n}" {
		t.Fatalf("落盘内容不对: %s", b)
	}
}

// settingsPath 应与条目库同目录。
func TestSettingsPath(t *testing.T) {
	got := settingsPath("/data/bigpan-data/items.json")
	if got != "/data/bigpan-data/settings.json" {
		t.Fatalf("got %s", got)
	}
}
