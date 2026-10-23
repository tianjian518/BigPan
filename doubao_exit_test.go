package main

import (
	"strings"
	"testing"
	"time"
)

// 出口池：失败 → 冷却 → 到期自动放行 → 成功清零。
func TestDbExitPool(t *testing.T) {
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	ip := "1.2.3.4"
	if !dbExitUsable(ip) {
		t.Fatal("没打过任何请求的 IP 应该是可用的")
	}
	d := dbExitFail(ip)
	if d != dbCoolFirst {
		t.Fatalf("首次冷却应为 %v，实际 %v", dbCoolFirst, d)
	}
	if dbExitUsable(ip) {
		t.Fatal("刚冷却完的 IP 不该可用")
	}
	if w := dbExitWait(); w <= 0 || w > dbCoolFirst {
		t.Fatalf("等待时间应落在 (0, %v]，实际 %v", dbCoolFirst, w)
	}

	// 再失败一次，冷却翻倍
	d2 := dbExitFail(ip)
	if d2 != dbCoolFirst*2 {
		t.Fatalf("第二次冷却应翻倍为 %v，实际 %v", dbCoolFirst*2, d2)
	}

	// 连续失败不能超过上限
	for i := 0; i < 20; i++ {
		d = dbExitFail(ip)
	}
	if d > dbCoolMax {
		t.Fatalf("冷却时长不该超过 %v，实际 %v", dbCoolMax, d)
	}

	dbExitOK(ip)
	if !dbExitUsable(ip) {
		t.Fatal("成功一次后应立刻放行")
	}
	if w := dbExitWait(); w != 0 {
		t.Fatalf("池里没有冷却中的 IP 时不该有等待时间，实际 %v", w)
	}
}

// 两条出口：一条冷却，等的是"最快恢复"的那条。
func TestDbExitWaitPicksEarliest(t *testing.T) {
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	now := time.Now()
	dbExits["a"] = &dbExitState{downTo: now.Add(5 * time.Minute), fails: 1}
	dbExits["b"] = &dbExitState{downTo: now.Add(90 * time.Second), fails: 1}
	dbExits["c"] = &dbExitState{downTo: now.Add(-time.Minute), fails: 1} // 已到期
	dbExitMu.Unlock()

	if dbExitUsable("a") || dbExitUsable("b") {
		t.Fatal("a/b 应在冷却中")
	}
	if !dbExitUsable("c") {
		t.Fatal("c 已过冷却期，应放行")
	}
	w := dbExitWait()
	if w <= 0 || w > 91*time.Second || w < 85*time.Second {
		t.Fatalf("应返回最快恢复的约 90 秒，实际 %v", w)
	}
}

// 风控错误必须是「人话」，且带分钟数。
func TestDbThrottleErrText(t *testing.T) {
	e := &dbThrottleErr{wait: 5 * time.Minute}
	if !strings.Contains(e.Error(), "风控") || !strings.Contains(e.Error(), "分钟") {
		t.Fatalf("风控提示应有具体指引，实际：%s", e.Error())
	}
	if !isThrottleErr(e) {
		t.Fatal("isThrottleErr 应识别自己的类型")
	}
	// 不足一分钟也要显示"1 分钟"，不能说"0 分钟"
	short := &dbThrottleErr{wait: 10 * time.Second}
	if !strings.Contains(short.Error(), "1 分钟") {
		t.Fatalf("短冷却应显示 1 分钟，实际：%s", short.Error())
	}
}

// 断点：存 → 取 → 完成删除。
func TestDbScanResume(t *testing.T) {
	dbScanInit()
	key := "test|own|123"
	dbScanDone(key)

	st := dbScanTake(key)
	if len(st.Seen) != 0 || len(st.Queue) != 0 {
		t.Fatal("新 key 应该拿到空状态")
	}
	st.Title = "测试剧"
	st.Seen = []string{"root", "a", "b"}
	st.Queue = []dbScanJob{{ID: "c", Prefix: "x"}}
	st.Files = []ShareFile{{ID: "f1", Name: "1.mp4"}}
	dbScanPut(st)

	got := dbScanTake(key)
	if got.Title != "测试剧" || len(got.Seen) != 3 || len(got.Queue) != 1 || len(got.Files) != 1 {
		t.Fatalf("断点没存住：%+v", got)
	}
	dbScanDone(key)
	if again := dbScanTake(key); len(again.Seen) != 0 {
		t.Fatal("完成后断点应被删掉")
	}
}
