package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStoreBulkDeleteDoesNotBlockReads 回归测试：
// 2026-10-21 盒子上 WebDAV「连不上」，根因是条目库每次增删都持写锁全量写盘
// （eMMC 2.7MB/s × 858KB ≈ 0.3s/次），批量操作时 PROPFIND 全排队。
// 修复后写盘异步合并，读请求在批量删除 500 条期间不应出现明显停顿。
func TestStoreBulkDeleteDoesNotBlockReads(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}

	const n = 1500
	batch := make([]Item, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%04d", i)
		ids = append(ids, id)
		batch = append(batch, Item{
			ID: id, Title: fmt.Sprintf("第%04d集", i),
			Dir:      fmt.Sprintf("天翼/测试剧/Season %d", i%5),
			Show:     "测试剧",
			Provider: "tianyi", ShareID: "123", ShareNumID: "123",
			FileID: fmt.Sprintf("f%d", i), Ext: ".mkv", Size: int64(i),
		})
	}
	s.AddBatch(batch)

	var maxLat int64 // 纳秒
	var reads int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				t0 := time.Now()
				s.Children("天翼/测试剧")
				if d := time.Since(t0).Nanoseconds(); d > atomic.LoadInt64(&maxLat) {
					atomic.StoreInt64(&maxLat, d)
				}
				atomic.AddInt64(&reads, 1)
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()

	// 批量删除 500 条（模拟清理一季剧集）
	for i := 0; i < 500; i++ {
		s.Delete(ids[i])
	}
	close(stop)

	maxMs := time.Duration(atomic.LoadInt64(&maxLat)).Milliseconds()
	t.Logf("删除 500 条期间读到 %d 次，最大读延迟 %dms", atomic.LoadInt64(&reads), maxMs)
	if maxMs > 120 {
		t.Fatalf("读延迟 %dms 过高：写盘又回到锁里了", maxMs)
	}
	if got := len(s.List()); got != n-500 {
		t.Fatalf("删除后条目数 %d，期望 %d", got, n-500)
	}

	// 异步写盘：Flush 后磁盘上必须是删完的版本
	s.Flush()
	s2, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.List()); got != n-500 {
		t.Fatalf("落盘后重开条目数 %d，期望 %d", got, n-500)
	}
}

// TestStoreRealDataOnBox 用真实条目库副本压测（只在盒子本地跑，验证 eMMC 写速下的读延迟）。
// 用法：STORE_REAL=/data/items.json ./storetest -test.run TestStoreRealDataOnBox -test.v
func TestStoreRealDataOnBox(t *testing.T) {
	src := os.Getenv("STORE_REAL")
	if src == "" {
		t.Skip("未设置 STORE_REAL")
	}
	// 放在同一块盘上（/data 所在卷），避免跨盘影响写速判断
	dstDir := filepath.Dir(src) + "/_bench"
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dstDir)
	p := filepath.Join(dstDir, "items.json")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	items := s.List()
	t.Logf("真实库 %d 条，文件 %.1fKB", len(items), float64(len(b))/1024)

	var maxLat, sumLat int64
	var reads int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				t0 := time.Now()
				s.Children("天翼")
				d := time.Since(t0).Nanoseconds()
				atomic.AddInt64(&sumLat, d)
				if d > atomic.LoadInt64(&maxLat) {
					atomic.StoreInt64(&maxLat, d)
				}
				atomic.AddInt64(&reads, 1)
				time.Sleep(300 * time.Microsecond)
			}
		}
	}()

	del := len(items) / 4
	if del > 400 {
		del = 400
	}
	ids := make([]string, 0, del)
	for i := 0; i < del; i++ {
		ids = append(ids, items[i].ID)
	}
	t0 := time.Now()
	s.DeleteBatch(ids)
	delCost := time.Since(t0)
	close(stop)
	<-done

	n := atomic.LoadInt64(&reads)
	avg := time.Duration(0)
	if n > 0 {
		avg = time.Duration(atomic.LoadInt64(&sumLat) / n)
	}
	t.Logf("删除 %d 条耗时 %v；期间读 %d 次，平均 %v，最大 %v",
		del, delCost, n, avg, time.Duration(atomic.LoadInt64(&maxLat)))
	s.Flush()
}
