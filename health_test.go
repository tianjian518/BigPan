package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 失效判定的边界：宁可漏判（多等一轮），绝不误判（删错整部剧）。
func TestShareGone(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		// 真失效
		{"分享已失效（原分享被取消或删除，errno=-130 分享不存在）", true},
		{"分享已失效（链接不存在，errno=105）", true},
		{"分享可能已失效或提取码不正确", true},
		{"移动云盘: 分享已过期（9530）", true},
		{"分享不存在", true},
		{"链接不存在", true},

		// 临时故障 / 账号问题：一律不清理
		{"连接百度分享失败：dial tcp: i/o timeout", false},
		// 分享还在、只是没扫到视频（139 的 ISO 分享就是这个），不能判死
		{"这个分享里没有找到视频文件", false},
		{"net/http: TLS handshake timeout", false},
		{"移动云盘: 系统繁忙（500）", false},
		{"too many requests", false},
		{"请求过于频繁，已限流", false},
		{"还没有导入移动云盘凭证", false},
		{"夸克登录已失效，请重新登录", false},
		{"502 Bad Gateway", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.err != "" {
			err = &strErr{c.err}
		}
		if got := shareGone(err); got != c.want {
			t.Errorf("shareGone(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

// 清理只动目标分享，绝不波及其他分享的条目。
func TestPurgeShareOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a1","title":"甲1","provider":"baidu","dir":"百度/国产剧/甲","share_id":"s1","file_id":"f1","ext":".mp4"},
{"id":"a2","title":"甲2","provider":"baidu","dir":"百度/国产剧/甲","share_id":"s1","file_id":"f2","ext":".mp4"},
{"id":"b1","title":"乙1","provider":"baidu","dir":"百度/国产剧/乙","share_id":"s2","file_id":"f3","ext":".mp4"},
{"id":"c1","title":"丙1","provider":"quark","dir":"夸克/欧美剧/丙","share_id":"s1","file_id":"f4","ext":".mkv"}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}

	old := globalShares
	gs, _ := NewShareStore(filepath.Join(dir, "shares.json"))
	gs.Put(ShareMeta{Provider: "baidu", ShareID: "s1", Show: "甲"})
	gs.Put(ShareMeta{Provider: "baidu", ShareID: "s2", Show: "乙"})
	globalShares = gs
	defer func() { globalShares = old }()

	n := purgeShare(s, ShareMeta{Provider: "baidu", ShareID: "s1"})
	if n != 2 {
		t.Fatalf("应删 2 条，实际 %d", n)
	}
	left := s.List()
	if len(left) != 2 {
		t.Fatalf("应剩 2 条，实际 %d: %+v", len(left), left)
	}
	// 同 shareID 但不同网盘（夸克 s1）必须保留
	found := map[string]bool{}
	for _, it := range left {
		found[it.ID] = true
	}
	if !found["b1"] || !found["c1"] {
		t.Fatalf("误删了别的分享: %+v", left)
	}
	// 登记只删目标
	if globalShares.Get("baidu", "s1") != nil {
		t.Fatal("目标登记未删除")
	}
	if globalShares.Get("baidu", "s2") == nil {
		t.Fatal("其他登记被误删")
	}
}

// 没有追更登记（老数据）时，手动清理要能从条目里补出剧名，日志才看得懂。
func TestShareMetaForFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a","title":"第1集","provider":"baidu","dir":"百度/国产剧/甲剧","show":"甲剧","category":"国产剧","share_id":"s9","file_id":"f","ext":".mp4"}
]`), 0644)
	s, _ := NewStore(p)

	old := globalShares
	globalShares = nil
	defer func() { globalShares = old }()

	m := shareMetaFor(s, "baidu", "s9")
	if m.Show != "甲剧" || m.Category != "国产剧" {
		t.Fatalf("未从条目补全: %+v", m)
	}
}

// 全量扫描必须覆盖「不在追更列表里」的分享：
// 已完结、已隐藏、以及 items 里有但 shares.json 没登记的孤儿。
func TestHealthTargetsIncludesFinishedAndOrphans(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a","title":"x","provider":"baidu","dir":"百度/剧A","show":"剧A","share_id":"s-active","file_id":"f1","ext":".mp4"},
{"id":"b","title":"y","provider":"baidu","dir":"百度/剧B","show":"剧B","share_id":"s-finished","file_id":"f2","ext":".mp4"},
{"id":"c","title":"z","provider":"quark","dir":"夸克/剧C","show":"剧C","share_id":"s-hidden","file_id":"f3","ext":".mkv"},
{"id":"d","title":"w","provider":"baidu","dir":"百度/剧D","show":"剧D","share_id":"s-orphan","file_id":"f4","ext":".mp4"}
]`), 0644)
	s, _ := NewStore(p)

	old := globalShares
	defer func() { globalShares = old }()
	gs, _ := NewShareStore(filepath.Join(dir, "shares.json"))
	gs.Put(ShareMeta{Provider: "baidu", ShareID: "s-active", Show: "剧A", AutoRefresh: true})
	gs.Put(ShareMeta{Provider: "baidu", ShareID: "s-finished", Show: "剧B", AutoRefresh: false}) // 已完结
	gs.Put(ShareMeta{Provider: "quark", ShareID: "s-hidden", Show: "剧C", Hidden: true})          // 已隐藏
	// s-orphan 故意不登记
	globalShares = gs

	metas := healthTargets(s)
	got := map[string]bool{}
	for _, m := range metas {
		got[m.Provider+"/"+m.ShareID] = true
	}
	for _, want := range []string{"baidu/s-active", "baidu/s-finished", "quark/s-hidden", "baidu/s-orphan"} {
		if !got[want] {
			t.Errorf("漏扫: %s（当前 %v）", want, got)
		}
	}
	if len(metas) != 4 {
		t.Errorf("应扫 4 个分享，实际 %d", len(metas))
	}
	// 孤儿要能从条目补出剧名，清理日志才有意义
	for _, m := range metas {
		if m.ShareID == "s-orphan" && m.Show != "剧D" {
			t.Errorf("孤儿未补剧名: %+v", m)
		}
	}
}
