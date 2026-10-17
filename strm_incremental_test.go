package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildSTRMIncremental 锁住增量同步的核心承诺：
//
//	① 没变化时一个文件都不重写（mtime 原样保留）——飞牛影视不会重刮削；
//	② 只加新条目时只落新文件，老文件 mtime 不变；
//	③ 条目改名/挪目录后旧 .strm 被清掉、新路径落地；
//	④ 对外地址（self）变化导致内容变化时才重写。
func TestBuildSTRMIncremental(t *testing.T) {
	s, root := setupSTRMTest(t)
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: true}
	self := "http://192.168.1.10:8099"

	ep1 := filepath.Join(root, "百度/国产剧/甲/第01集.mp4.strm")

	// ① 首次：写入唯一的条目
	if n, err := buildSTRM(s, self); err != nil || n != 1 {
		t.Fatalf("首次构建 n=%d err=%v，期望 1/nil", n, err)
	}
	st1, err := os.Stat(ep1)
	if err != nil {
		t.Fatalf("首次构建未生成 %s: %v", ep1, err)
	}

	// ② 原样再跑：零写入，mtime 不许变
	time.Sleep(50 * time.Millisecond) // 让 mtime 有机会显出差异
	if n, err := buildSTRM(s, self); err != nil || n != 0 {
		t.Fatalf("无变化重跑 n=%d err=%v，期望 0/nil（不应重写任何文件）", n, err)
	}
	st2, err := os.Stat(ep1)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatalf("无变化时文件被重写了：mtime %v → %v", st1.ModTime(), st2.ModTime())
	}

	// ③ 新增一部剧：只写新的 1 个，老文件 mtime 不变
	s.Add(Item{Title: "第01集", Dir: "百度/国产剧/乙", Provider: "baidu", ShareID: "s2", FileID: "f2", Ext: ".mp4", Size: 2})
	time.Sleep(50 * time.Millisecond)
	if n, err := buildSTRM(s, self); err != nil || n != 1 {
		t.Fatalf("加一部剧 n=%d err=%v，期望 1/nil（只写新文件）", n, err)
	}
	ep2 := filepath.Join(root, "百度/国产剧/乙/第01集.mp4.strm")
	if _, err := os.Stat(ep2); err != nil {
		t.Fatalf("新剧集未生成: %v", err)
	}
	st3, _ := os.Stat(ep1)
	if !st3.ModTime().Equal(st1.ModTime()) {
		t.Fatalf("加新剧时老文件被重写了：mtime %v → %v", st1.ModTime(), st3.ModTime())
	}

	// ④ 删旧条目 + 加新路径（等价于改名/挪目录）：旧 .strm 被清掉、新路径落地
	items := s.List()
	for _, it := range items {
		if it.FileID == "f2" {
			if !s.Delete(it.ID) {
				t.Fatal("删除条目失败")
			}
		}
	}
	s.Add(Item{Title: "第01集", Dir: "百度/国产剧/丙", Provider: "baidu", ShareID: "s2", FileID: "f3", Ext: ".mp4", Size: 3})
	if n, err := buildSTRM(s, self); err != nil || n != 1 {
		t.Fatalf("改名 n=%d err=%v，期望 1/nil", n, err)
	}
	if _, err := os.Stat(ep2); err == nil {
		t.Fatal("改名后旧 .strm 应被删除")
	}
	ep3 := filepath.Join(root, "百度/国产剧/丙/第01集.mp4.strm")
	if _, err := os.Stat(ep3); err != nil {
		t.Fatalf("改名后新路径未生成: %v", err)
	}

	// ⑤ 对外地址变化：内容变了才重写
	if n, err := buildSTRM(s, "http://pan.tianjianfn.de5.net"); err != nil || n != 2 {
		t.Fatalf("换 self n=%d err=%v，期望 2/nil（全部内容变化应重写）", n, err)
	}
}
