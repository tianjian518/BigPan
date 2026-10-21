package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTianyiSubscribe(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://content.21cn.com/h5/subscrip/index.html#/pages/own-home/index?uuid=56673222a02292e570a02e226f5d256f", "56673222a02292e570a02e226f5d256f"},
		{"https://h5.cloud.189.cn/share.html?userId=56673222a02292e570a02e226f5d256f", "56673222a02292e570a02e226f5d256f"},
		{"  56673222a02292e570a02e226f5d256f  ", "56673222a02292e570a02e226f5d256f"},
		{"https://content.21cn.com/h5/subscrip/index.html#/pages/own-home/index?uuid=56673222A02292E570A02E226F5D256F", "56673222a02292e570a02e226f5d256f"},
	}
	for _, c := range cases {
		got, err := parseTianyiSubscribe(c.in)
		if err != nil {
			t.Errorf("parse %q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parse %q = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := parseTianyiSubscribe("https://cloud.189.cn/t/zuEfyiUfiEZ3"); err == nil {
		t.Errorf("分享链接不该被认成订阅号")
	}
	if _, err := parseTianyiSubscribe(""); err == nil {
		t.Errorf("空串应报错")
	}
}

// TestTianyiSubImportLive 实测：从订阅号挑一条小分享收录进临时库。
// 用法：T189_UP=xxx go test -run TestTianyiSubImportLive -v
func TestTianyiSubImportLive(t *testing.T) {
	up := os.Getenv("T189_UP")
	if up == "" {
		t.Skip("未设置 T189_UP，跳过联网测试")
	}
	list, _, err := tianyiUpShares(nil, up, 1, 20)
	if err != nil {
		t.Fatalf("拉取分享失败: %v", err)
	}
	// 找一条目录型、名字带"豆瓣"的合集（文件多但层级浅）
	var target *tianyiSubShare
	for i := range list {
		if list[i].IsFolder {
			target = &list[i]
			break
		}
	}
	if target == nil {
		t.Skip("首页没有目录型分享")
	}

	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatalf("建临时库失败: %v", err)
	}
	entry := tianyiSubEntry{UserID: up, Name: "测试", AutoImport: true}
	n, err := tianyiSubImportOne(store, "", entry, *target)
	if err != nil {
		t.Fatalf("收录失败: %v", err)
	}
	fmt.Printf("\n[收录] %s → %d 条\n", target.Name, n)
	items := store.List()
	if len(items) == 0 {
		t.Fatal("收录后库里没有条目")
	}
	it := items[0]
	fmt.Printf("  示例: %s | share_id=%s share_num_id=%s sub=%s\n", it.FullPath(), it.ShareID, it.ShareNumID, it.SubUserID)
	if it.SubUserID != up || it.Provider != "tianyi" || it.ShareNumID == "" {
		t.Errorf("条目字段不对: %+v", it)
	}
}

// TestRenderIndexHasTianyiSub 确认页面里真的塞进了天翼订阅页（并导出 HTML 供语法检查）。
func TestRenderIndexHasTianyiSub(t *testing.T) {
	html := renderIndex(nil, nil, "http://127.0.0.1:8080")
	for _, want := range []string{"pg-tianyi-sub", "天翼订阅", "loadTianyiSubs()", "tsBrowse("} {
		if !strings.Contains(html, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	if err := os.WriteFile("/tmp/bigpan_index.html", []byte(html), 0644); err != nil {
		t.Fatalf("dump: %v", err)
	}
}
