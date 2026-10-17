package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

// 导入 N 条：逐条 Add（每次全量落盘）vs 批量 AddBatch。
// 只跑：go test -bench 'AddBatch|AddSingle' -benchtime=1x
func benchItems(n int) []Item {
	out := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Item{
			Title: fmt.Sprintf("第%03d集", i), Dir: "百度/国产剧/测试剧",
			Provider: "baidu", ShareID: "s1", FileID: fmt.Sprintf("f%d", i),
			Ext: ".mp4", Size: 1 << 30,
		})
	}
	return out
}

func BenchmarkAddSingle300(b *testing.B) {
	list := benchItems(300)
	for i := 0; i < b.N; i++ {
		s, _ := NewStore(filepath.Join(b.TempDir(), "items.json"))
		for _, it := range list {
			s.Add(it)
		}
	}
}

func BenchmarkAddBatch300(b *testing.B) {
	list := benchItems(300)
	for i := 0; i < b.N; i++ {
		s, _ := NewStore(filepath.Join(b.TempDir(), "items.json"))
		s.AddBatch(list)
	}
}
