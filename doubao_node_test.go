package main

import (
	"path/filepath"
	"testing"
)

// 从自己盘导入多个目录，必须在分享列表里各占一条，不能互相覆盖。
// 之前 ShareID 为空 → 键退化成 "doubao1|" → 后导入的覆盖先导入的，
// 列表里永远只剩一条、名字还是最后那个。
func TestDoubaoNodeEntriesAreDistinct(t *testing.T) {
	st, err := NewStore(filepath.Join(t.TempDir(), "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(show, node string, n int) []Item {
		var out []Item
		for i := 0; i < n; i++ {
			out = append(out, Item{
				Title:    "第" + string(rune('0'+i)) + "集",
				Dir:      "doubao1/电视剧/" + show,
				Show:     show,
				Category: "电视剧",
				Provider: "doubao1",
				ShareID:  doubaoNodeIDPrefix + node,
				FileID:   node + "-f" + string(rune('0'+i)),
				Ext:      ".mp4",
			})
		}
		return out
	}
	var all []Item
	all = append(all, mk("动漫剧集", "111", 3)...)
	all = append(all, mk("科幻片", "222", 2)...)
	if added := st.AddBatch(all); added != 5 {
		t.Fatalf("应入库 5 条，实际 %d", added)
	}

	list := st.Shares()
	if len(list) != 2 {
		t.Fatalf("两个 node 目录应各占一条，实际 %d 条：%+v", len(list), list)
	}
	names := map[string]bool{}
	for _, s := range list {
		names[s.ShowName] = true
		if s.ShareURL != "" {
			t.Fatalf("自己盘条目不该有分享链接，实际 %q", s.ShareURL)
		}
		if s.Files == 0 {
			t.Fatalf("%s 的文件数为 0", s.ShowName)
		}
	}
	if !names["动漫剧集"] || !names["科幻片"] {
		t.Fatalf("剧名不对：%v", names)
	}
}

// 巡检必须跳过 node: 条目，否则几千个条目轮着扫会直接风控。
func TestHealthSkipsDoubaoNodeEntries(t *testing.T) {
	st, err := NewStore(filepath.Join(t.TempDir(), "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.AddBatch([]Item{
		{Title: "e1", Dir: "doubao1/电视剧/A", Show: "A", Category: "电视剧",
			Provider: "doubao1", ShareID: "node:111", FileID: "f1", Ext: ".mp4"},
		{Title: "e2", Dir: "doubao1/电视剧/B", Show: "B", Category: "电视剧",
			Provider: "doubao1", ShareID: "node:222", FileID: "f2", Ext: ".mp4"},
		{Title: "e3", Dir: "quark/电视剧/C", Show: "C", Category: "电视剧",
			Provider: "quark", ShareID: "abcdef", FileID: "f3", Ext: ".mp4"},
	})
	old := globalShares
	defer func() { globalShares = old }()
	gs, err := NewShareStore(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	globalShares = gs

	metas := healthTargets(st)
	if len(metas) != 1 {
		t.Fatalf("巡检应只剩 1 个分享（quark），实际 %d 个：%+v", len(metas), metas)
	}
	if metas[0].Provider != "quark" {
		t.Fatalf("留下的应是 quark，实际 %s / %s", metas[0].Provider, metas[0].ShareID)
	}
}
