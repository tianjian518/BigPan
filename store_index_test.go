package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T, items ...Item) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > 0 {
		s.AddBatch(items)
	}
	return s
}

// TestUpdateBatchMovesShare 整季改分类：一次批量Update，匹配的全改、不匹配的不动。
func TestUpdateBatchMovesShare(t *testing.T) {
	var items []Item
	for i := 0; i < 20; i++ {
		items = append(items, Item{
			ID: fmt.Sprintf("a%02d", i), Title: fmt.Sprintf("A第%02d集", i),
			Dir: "夸克/电视剧/老分类/甲剧 (2020)", Category: "电视剧/老分类",
			Provider: "quark", ShareID: "shareA", Show: "甲剧 (2020)",
		})
	}
	for i := 0; i < 5; i++ {
		items = append(items, Item{
			ID: fmt.Sprintf("b%02d", i), Title: fmt.Sprintf("B第%02d集", i),
			Dir: "夸克/电视剧/老分类/乙剧 (2021)", Category: "电视剧/老分类",
			Provider: "quark", ShareID: "shareB", Show: "乙剧 (2021)",
		})
	}
	s := newTestStore(t, items...)

	moved := s.UpdateBatch(func(x *Item) bool {
		return x.ShareID == "shareA"
	}, func(x *Item) {
		x.Category = "电视剧/新分类"
		x.Dir = "夸克/电视剧/新分类/甲剧 (2020)"
	})
	if moved != 20 {
		t.Fatalf("moved=%d, want 20", moved)
	}
	for _, it := range s.List() {
		switch it.ShareID {
		case "shareA":
			if it.Category != "电视剧/新分类" || it.Dir != "夸克/电视剧/新分类/甲剧 (2020)" {
				t.Errorf("shareA 条目没搬过去: %+v", it)
			}
		case "shareB":
			if it.Category != "电视剧/老分类" {
				t.Errorf("shareB 条目被误搬: %+v", it)
			}
		}
	}
	// 索引跟着更新：新路径能直接查到（证明 dirty 重建后一致）
	// 注意 FileName() 空扩展名会补 .mp4
	if s.FindByPath("夸克/电视剧/新分类/甲剧 (2020)/A第00集.mp4") == nil {
		t.Error("新路径查不到条目，索引没跟上")
	}
	// 空匹配返回 0，不置脏不落盘
	if n := s.UpdateBatch(func(x *Item) bool { return false }, func(x *Item) {}); n != 0 {
		t.Errorf("空匹配返回 %d, want 0", n)
	}
}

// TestDeleteByIDWorksAfterDirty 删条目走后缀索引定位；删完置脏，再删下一条
// （索引已过期）也必须正常——回归 idIdx 用法与 dirty 重建的配合。
func TestDeleteByIDWorksAfterDirty(t *testing.T) {
	s := newTestStore(t,
		Item{ID: "x1", Title: "一", Dir: "d1", Provider: "quark"},
		Item{ID: "x2", Title: "二", Dir: "d1", Provider: "quark"},
		Item{ID: "x3", Title: "三", Dir: "d1", Provider: "quark"},
	)
	if !s.Delete("x1") {
		t.Fatal("删除 x1 失败")
	}
	// 此时索引已置脏；不读一次直接再删（走 rebuild-if-dirty 分支）
	if !s.Delete("x2") {
		t.Fatal("索引过期状态下删除 x2 失败")
	}
	if s.Delete("nope") {
		t.Error("删不存在的 ID 返回了 true")
	}
	rest := s.List()
	if len(rest) != 1 || rest[0].ID != "x3" {
		t.Fatalf("剩余条目不对: %+v", rest)
	}
}

// TestAddBatchDupPathOverwrite 同路径覆盖：批次内两条同 FullPath，只留一条
// （后者覆盖前者，ID 保留前者）——share.go 攒批入库依赖这个语义。
func TestAddBatchDupPathOverwrite(t *testing.T) {
	s := newTestStore(t)
	n := s.AddBatch([]Item{
		{ID: "p1", Title: "同名单集", Dir: "d", FileID: "f1", Size: 100, Provider: "quark"},
		{ID: "p2", Title: "同名单集", Dir: "d", FileID: "f2", Size: 200, Provider: "quark"},
	})
	if n != 2 {
		t.Fatalf("AddBatch 返回 %d, want 2", n)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("同路径应只剩 1 条，实际 %d 条", len(list))
	}
	if list[0].ID != "p1" {
		t.Errorf("覆盖后 ID 应保留前者 p1，实际 %s", list[0].ID)
	}
	if list[0].FileID != "f2" || list[0].Size != 200 {
		t.Errorf("覆盖后应是后者的值: %+v", list[0])
	}
}

// BenchmarkStoreWriteReadCycle 「写一条→读一次」的周期代价（读会按需重建索引）。
// 2026-10-21 在 x86 沙箱实测 rebuildIndex：333 条 0.25ms / 2 千 2ms /
// 1 万 6.4ms / 5 万 33ms；N1（ARM A53）约慢 3~4 倍。
// 这个基准是 P3-①「写后全量重建索引」的回归哨兵：数值随库增大而劣化属预期，
// 但如果单次周期耗时突然数量级上涨，说明有地方又在循环内逐条写入了。
func BenchmarkStoreWriteReadCycle(b *testing.B) {
	for _, n := range []int{333, 2000, 10000} {
		items := make([]Item, n)
		for i := range items {
			items[i] = Item{
				ID: fmt.Sprintf("id-%d", i), Title: "x",
				Dir: fmt.Sprintf("夸克/电视剧/测试剧 (2020)/Season %d", i%10),
				Category: "电视剧/测试", Provider: "quark",
			}
		}
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			s := &Store{items: items, dirty: true}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.AddBatch(items[i%n : i%n+1])
				s.List()
			}
		})
	}
}
