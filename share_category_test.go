package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestItemDirWithCategory 换分类只动分类段：网盘顶层、剧名、分享内子目录原样保留。
func TestItemDirWithCategory(t *testing.T) {
	cases := []struct {
		name string
		it   Item
		cat  string
		want string
	}{
		{"换分类", Item{Provider: "baidu", Category: "国产剧", Show: "甲", Dir: "百度/国产剧/甲"}, "欧美剧", "百度/欧美剧/甲"},
		{"清空分类", Item{Provider: "baidu", Category: "国产剧", Show: "甲", Dir: "百度/国产剧/甲"}, "", "百度/甲"},
		{"保留分享内子目录", Item{Provider: "baidu", Category: "国产剧", Show: "甲", Dir: "百度/国产剧/甲/Season 1"}, "欧美剧", "百度/欧美剧/甲/Season 1"},
		{"原来没分类", Item{Provider: "baidu", Category: "", Show: "甲", Dir: "百度/甲"}, "华语电影", "百度/华语电影/甲"},
		{"老数据无 Category 字段（分类段在目录里）", Item{Provider: "baidu", Category: "", Show: "甲", Dir: "百度/国产剧/甲"}, "欧美剧", "百度/欧美剧/甲"},
		{"剧名段找不到时保底不丢目录", Item{Provider: "baidu", Category: "", Show: "", Dir: "百度/国产剧/甲"}, "欧美剧", "百度/欧美剧/国产剧/甲"},
		{"英文网盘名", Item{Provider: "quark", Category: "动画", Show: "乙", Dir: "夸克/动画/乙"}, "电影", "夸克/电影/乙"},
	}
	for _, c := range cases {
		if got := itemDirWithCategory(&c.it, c.cat); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestStoreUpdate 原地改条目：字段改上、索引刷新、落盘、找不到返回 false。
func TestStoreUpdate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"x","title":"第01集","provider":"baidu","dir":"百度/国产剧/甲","share_id":"s1","file_id":"f1","ext":".mp4"}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	old := s.FindByID("x")
	if old == nil {
		t.Fatal("初始条目没有")
	}
	if !s.Update("x", func(it *Item) { it.Category = "欧美剧"; it.Dir = "百度/欧美剧/甲" }) {
		t.Fatal("Update 返回 false")
	}
	it := s.FindByID("x")
	if it.Category != "欧美剧" || it.Dir != "百度/欧美剧/甲" {
		t.Fatalf("字段没改上: %+v", it)
	}
	if it.ID != "x" || !it.AddedAt.Equal(old.AddedAt) {
		t.Fatal("ID 或入库时间被改动了")
	}
	// 路径索引要跟着刷新（走 fpIdx 的 FindByName）
	if s.FindByName("百度/欧美剧/甲/第01集.mp4") == nil {
		t.Error("新路径查不到（索引没刷新）")
	}
	if s.FindByName("百度/国产剧/甲/第01集.mp4") != nil {
		t.Error("旧路径还能查到（索引没刷新）")
	}
	// 落盘：重开 Store 值还在
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "欧美剧") {
		t.Error("修改未落盘")
	}
	if s.Update("nope", func(it *Item) {}) {
		t.Error("不存在的 ID 应返回 false")
	}
}

// TestApiShareCategory 端到端：改分类 → 条目/登记/STRM 三处都换，别的剧不动。
func TestApiShareCategory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a1","title":"第01集","provider":"baidu","dir":"百度/国产剧/甲","category":"国产剧","show":"甲","share_id":"s1","file_id":"f1","ext":".mp4","size":1},
{"id":"a2","title":"第02集","provider":"baidu","dir":"百度/国产剧/甲","category":"国产剧","show":"甲","share_id":"s1","file_id":"f2","ext":".mp4","size":1},
{"id":"b1","title":"第01集","provider":"baidu","dir":"百度/国产剧/乙","category":"国产剧","show":"乙","share_id":"s2","file_id":"g1","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	// 隔离全局状态
	oldRoot, oldCfg, oldCfgPath, oldShares := strmRoot, strmCfg, strmConfigPath, globalShares
	t.Cleanup(func() {
		strmRoot, strmCfg, strmConfigPath, globalShares = oldRoot, oldCfg, oldCfgPath, oldShares
	})
	strmRoot = filepath.Join(dir, "strm")
	strmConfigPath = filepath.Join(dir, "strm_config.json")
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: true}
	gs, err := NewShareStore(filepath.Join(dir, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	globalShares = gs
	gs.Put(ShareMeta{Provider: "baidu", ShareID: "s1", Passcode: "abcd", Category: "国产剧", Show: "甲", AutoRefresh: true})

	// 先按旧分类建一次 STRM
	if n, err := buildSTRM(s, "http://192.168.1.10:8099", false); err != nil || n != 3 {
		t.Fatalf("首次构建 n=%d err=%v", n, err)
	}
	oldPath := filepath.Join(strmRoot, "百度/国产剧/甲/第01集.mp4.strm")
	newPath := filepath.Join(strmRoot, "百度/欧美剧/甲/第01集.mp4.strm")
	other := filepath.Join(strmRoot, "百度/国产剧/乙/第01集.mp4.strm")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("旧路径应存在: %v", err)
	}

	// 调接口改分类
	req := httptest.NewRequest("POST", "/api/shares/category",
		strings.NewReader(`{"provider":"baidu","share_id":"s1","category":"欧美剧"}`))
	rec := httptest.NewRecorder()
	apiShareCategory(s, "http://192.168.1.10:8099")(rec, req)
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("接口失败: %s", rec.Body.String())
	}
	if resp["moved"].(float64) != 2 {
		t.Fatalf("moved=%v 期望 2", resp["moved"])
	}

	// 条目：Category/Dir 都换了，ID 不变
	it := s.FindByID("a1")
	if it == nil || it.Category != "欧美剧" || it.Dir != "百度/欧美剧/甲" {
		t.Fatalf("条目未更新: %+v", it)
	}
	// 追更登记同步换（以后追更出的新剧集才归新分类）
	m := globalShares.Get("baidu", "s1")
	if m == nil || m.Category != "欧美剧" || m.Show != "甲" || m.Passcode != "abcd" || !m.AutoRefresh {
		t.Fatalf("ShareMeta 更新不对: %+v", m)
	}
	// STRM：旧路径删掉、新路径落地、另一部剧不动（异步，轮询等它完成）
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(newPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Error("旧路径 .strm 应被删除")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("新路径 .strm 应生成: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("另一部的 .strm 不该被动")
	}
	if b, _ := os.ReadFile(newPath); true {
		// .strm 里的路径是 URL 转义的，先解码再比
		u, err := url.PathUnescape(string(b))
		if err != nil || !strings.Contains(u, "/direct/百度/欧美剧/甲/第01集.mp4") {
			t.Errorf("新 .strm 内容不对: %s", b)
		}
	}

	// 不存在的分享：报错不炸
	req2 := httptest.NewRequest("POST", "/api/shares/category",
		strings.NewReader(`{"provider":"baidu","share_id":"nope","category":"x"}`))
	rec2 := httptest.NewRecorder()
	apiShareCategory(s, "http://192.168.1.10:8099")(rec2, req2)
	var resp2 map[string]interface{}
	json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["ok"] != false {
		t.Errorf("不存在的分享应报错，实际 %s", rec2.Body.String())
	}
}
