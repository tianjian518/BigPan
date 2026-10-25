package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 端到端：扫描中途被风控 → 进度落盘 → 恢复后从断点接着扫。
// 需要 DOUBAO_CK；默认跳过。
func TestDoubaoScanResumeLive(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	dbScanInit()
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	cli := newDoubao(ck)
	_, kids, err := cli.dbHome()
	if err != nil {
		t.Fatalf("列根目录失败：%v", err)
	}
	// 找一个有子目录的节点
	var target dbNode
	for _, k := range kids {
		if k.IsDir() && k.Name != "Via" {
			target = k
			break
		}
	}
	if target.ID == "" {
		t.Skip("没有合适的测试目录")
	}
	key := "own|testslot|" + target.ID
	dbScanDone(key)

	// 1 秒后把所有出口打黑，模拟扫描中途撞上风控
	go func() {
		time.Sleep(1200 * time.Millisecond)
		cli.ipMu.Lock()
		ips := append([]string(nil), cli.ips...)
		cli.ipMu.Unlock()
		dbExitMu.Lock()
		for _, ip := range ips {
			dbExits[ip] = &dbExitState{downTo: time.Now().Add(2 * time.Minute), fails: 1}
		}
		dbExitMu.Unlock()
		cli.tr.CloseIdleConnections()
	}()

	_, files, _, err := scanDoubaoOwn(cli, "testslot", target.ID, target.Name, 0)
	t.Logf("第一轮：%d 个视频，err=%v", len(files), err)
	if err == nil {
		t.Skip("目录太小，1.2 秒内就扫完了，没法模拟中断")
	}
	if !strings.Contains(err.Error(), "接着扫") {
		t.Fatalf("中断时该告诉用户进度已保存，实际：%v", err)
	}

	st := dbScanTake(key)
	if len(st.Seen) == 0 {
		t.Fatal("断点没落盘")
	}
	t.Logf("断点已存：%d 个目录 / %d 个视频", len(st.Seen), len(st.Files))

	// 恢复出口，再扫一次
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()
	cli.tr.CloseIdleConnections()

	_, files2, _, err2 := scanDoubaoOwn(cli, "testslot", target.ID, target.Name, 0)
	if err2 != nil {
		t.Fatalf("续扫失败：%v", err2)
	}
	if len(files2) < len(files) {
		t.Fatalf("续扫结果 %d 少于中断时 %d", len(files2), len(files))
	}
	t.Logf("续扫完成：%d 个视频（应 ≥ 断点时的 %d 个）", len(files2), len(files))

	if again := dbScanTake(key); len(again.Seen) != 0 {
		t.Fatal("扫完了断点应被清掉")
	}
}
