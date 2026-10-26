package main

// 补齐逻辑：库里已有部分集时，只补缺的那些。

import (
	"testing"
)

func TestBuildShareItemsFill(t *testing.T) {
	info := ShareInfo{
		Provider: "doubao1", ShareID: "node:123",
		Files: []ShareFile{
			{ID: "a", Name: "E01.mkv", Path: "欧美剧/E01.mkv", Size: 100},
			{ID: "b", Name: "E02.mkv", Path: "欧美剧/E02.mkv", Size: 200},
			{ID: "c", Name: "E03.mkv", Path: "欧美剧/E03.mkv", Size: 300},
		},
	}
	root := shareRootOf(info.Files)
	if root != "欧美剧" {
		t.Fatalf("shareRoot=%q，应为 欧美剧", root)
	}

	// 全新建：3 条
	all := buildShareItems(info, "", "欧美剧", nil, nil, root)
	if len(all) != 3 {
		t.Fatalf("全新导入应 3 条，得到 %d", len(all))
	}

	// 库里已有 a：只补 b、c
	have := map[string]bool{"a": true}
	fill := buildShareItems(info, "", "欧美剧", nil, have, root)
	if len(fill) != 2 {
		t.Fatalf("补齐应 2 条，得到 %d", len(fill))
	}
	for _, it := range fill {
		if it.FileID == "a" {
			t.Fatal("已存在的 a 不该重复补")
		}
		if it.Show != "欧美剧" {
			t.Fatalf("Show=%q，应沿用 欧美剧", it.Show)
		}
	}

	// 全都有：0 条（调用方会标记 skipped）
	haveAll := map[string]bool{"a": true, "b": true, "c": true}
	if got := buildShareItems(info, "", "欧美剧", nil, haveAll, root); len(got) != 0 {
		t.Fatalf("都齐了应 0 条，得到 %d", len(got))
	}

	// 勾选白名单：只选了 c，且 a 已有 → 只补 c
	keep := map[string]bool{"c": true}
	got := buildShareItems(info, "", "欧美剧", keep, have, root)
	if len(got) != 1 || got[0].FileID != "c" {
		t.Fatalf("勾选过滤失效：%+v", got)
	}
}
