package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func resetStats() {
	statsMu.Lock()
	statsData = map[string]map[string]*playStat{}
	statsFile = ""
	statsMu.Unlock()
}

func TestStatPlayCounts(t *testing.T) {
	resetStats()
	statPlay("uc", true, nil)
	statPlay("uc", true, nil)
	statPlay("uc", false, errors.New("41020 token校验异常"))
	snap := playStatsSnapshot()
	if len(snap) != 1 {
		t.Fatalf("快照条目=%d，期望 1", len(snap))
	}
	row := snap[0]
	if row["ok"].(int) != 2 || row["fail"].(int) != 1 {
		t.Fatalf("计数错误: %+v", row)
	}
	if row["last_error"].(string) == "" {
		t.Fatal("失败时应记录最近错误")
	}
}

func TestStatPersistence(t *testing.T) {
	resetStats()
	dir := t.TempDir()
	initPlayStats(filepath.Join(dir, "play_stats.json"))
	statPlay("uc", true, nil)
	savePlayStats()
	if _, err := os.Stat(filepath.Join(dir, "play_stats.json")); err != nil {
		t.Fatalf("统计文件未落盘: %v", err)
	}
	// 重新加载
	statsMu.Lock()
	statsData = map[string]map[string]*playStat{}
	statsMu.Unlock()
	initPlayStats(filepath.Join(dir, "play_stats.json"))
	snap := playStatsSnapshot()
	if len(snap) != 1 || snap[0]["ok"].(int) != 1 {
		t.Fatalf("重载后数据不对: %+v", snap)
	}
	resetStats()
}

func TestRememberProbeItemOnlyUC(t *testing.T) {
	probeMu.Lock()
	probeItem = nil
	probeMu.Unlock()
	rememberProbeItem(&Item{ID: "1", Provider: "quark"})
	probeMu.Lock()
	if probeItem != nil {
		probeMu.Unlock()
		t.Fatal("非 UC 条目不应进入探针取样")
	}
	probeMu.Unlock()
	rememberProbeItem(&Item{ID: "2", Provider: "uc"})
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeItem == nil || probeItem.ID != "2" {
		t.Fatal("UC 条目应被记住")
	}
	probeItem = nil
}
