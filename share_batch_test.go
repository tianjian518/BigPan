package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchCache 解析缓存读写与过期。
func TestBatchCache(t *testing.T) {
	info := ShareInfo{
		Provider: "baidu", ShareID: "999", Title: "测试剧",
		Files: []ShareFile{{ID: "1", Name: "第01集.mp4"}},
	}
	batchParseCache.put(info)
	got, ok := batchParseCache.get("BAIDU", "999") // 大小写不敏感
	if !ok || got.Title != "测试剧" || len(got.Files) != 1 {
		t.Fatalf("缓存命中失败: ok=%v got=%+v", ok, got)
	}
	if _, ok := batchParseCache.get("baidu", "none"); ok {
		t.Error("不存在的 key 不该命中")
	}
}

// TestShareExists 已导入判断（按 provider + 分享标识）。
func TestShareExists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.AddBatch([]Item{
		{ID: "a", Title: "第01集", Dir: "百度/测试", Show: "测试", Provider: "baidu", ShareID: "123", FileID: "f1", Ext: ".mp4"},
	})
	if !shareExists(s, "baidu", "123") {
		t.Error("应判定为已导入")
	}
	if shareExists(s, "baidu", "456") {
		t.Error("不同分享不该误判")
	}
	if shareExists(s, "quark", "123") {
		t.Error("不同网盘不该误判")
	}
}

// TestRenderIndexHasBatchImport 确认导入页是「单条 + 批量合一」的那一套。
func TestRenderIndexHasBatchImport(t *testing.T) {
	html := renderIndex(nil, nil, "http://127.0.0.1:8080")
	want := []string{
		"batchText",     // 一个粘贴框，一条多条都走它
		"batchParse()",  // 解析
		"bApplyAll()",   // 统一分类：应用到全部
		"bCat1Opts",     // 一级分类下拉（可选已有 / 新建）
		"bCat2Opts",     // 二级分类下拉
		"bTitle(",       // 剧名逐条可改
		"bOpen(",        // 展开文件、取消勾选
		"bDirBar(",      // 只导某个子目录：目录筛选条
		"bDirFilter(",   // 点目录 = 只勾该目录
		"只导某个目录",
		"粘贴分享链接",
	}
	for _, w := range want {
		if !strings.Contains(html, w) {
			t.Errorf("页面缺少 %q", w)
		}
	}
	// 旧的「单条导入」独立卡片必须已经没了，否则又是两套逻辑
	for _, dead := range []string{`id="shareLink"`, `id="parseOut"`, "function doParse(", "batchApplyAll("} {
		if strings.Contains(html, dead) {
			t.Errorf("页面还残留旧单条导入的痕迹：%q", dead)
		}
	}
}

// TestNormalizeBatchLink 处理「片名 + 链接（访问码）」这种真实粘贴格式。
func TestNormalizeBatchLink(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"《成龙》经典70部优质收藏级作品.1080p.x265.10bit.2Audio国粤双语.内封中字.[423.1GB]\t\thttps://cloud.189.cn/t/zuEfyiUfiEZ3（访问码：xx11）",
			"https://cloud.189.cn/t/zuEfyiUfiEZ3（访问码：xx11）",
		},
		{"1. https://pan.quark.cn/s/abc123", "https://pan.quark.cn/s/abc123"}, // 行首编号一并裁掉
		{"https://pan.baidu.com/s/abc?pwd=1234", "https://pan.baidu.com/s/abc?pwd=1234"},
		{"  https://115.com/s/xyz?password=abcd  ", "https://115.com/s/xyz?password=abcd"},
		{"没有链接的一行", "没有链接的一行"},
	}
	for _, c := range cases {
		got := normalizeBatchLink(c.in)
		if got != c.want {
			t.Errorf("normalizeBatchLink(%q)\n got=%q\nwant=%q", c.in, got, c.want)
		}
	}
}

// TestRenderIndexHasShareBrowse 豆包盘页要有「别人的分享」浏览入口。
func TestRenderIndexHasShareBrowse(t *testing.T) {
	html := renderIndex(nil, nil, "http://127.0.0.1:8080")
	for _, w := range []string{"dbShareLink", "dbOpenShare(", "dbImportShare(", "别人的分享"} {
		if !strings.Contains(html, w) {
			t.Errorf("豆包盘页缺少 %q", w)
		}
	}
}

// TestPickSubdirLandsInOwnFolder 只导分享里的「电视剧」目录时，
// 落盘必须是 豆包1/电视剧/剧名/…（分享容器「芦苇豆包」和目录名折叠掉），
// 不能出现 豆包1/未分类/芦苇豆包/电视剧/… 这种总文件夹。
func TestPickSubdirLandsInOwnFolder(t *testing.T) {
	files := []ShareFile{
		{ID: "1", Name: "国色芳华-01.mp4", Path: "芦苇豆包/电视剧/最新国产电视剧/国色芳华/国色芳华-01.mp4"},
		{ID: "2", Name: "国色芳华-02.mp4", Path: "芦苇豆包/电视剧/最新国产电视剧/国色芳华/国色芳华-02.mp4"},
	}
	info := ShareInfo{Provider: "doubao1", ShareID: "38abf1bc52e891bd", Title: "芦苇豆包", Files: files}
	root := shareRootOf(info.Files) // 芦苇豆包
	got := buildShareItems(info, "", "电视剧", nil, nil, root)
	if len(got) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(got))
	}
	want := "豆包1/电视剧/最新国产电视剧/国色芳华"
	for _, it := range got {
		if it.Dir != want {
			t.Errorf("落盘目录 %q，期望 %q", it.Dir, want)
		}
	}
}
