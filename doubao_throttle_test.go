package main

import (
	"fmt"
	"testing"
)

// TestDbSoftThrottle 限流判定：671000000（豆包软限流码）必须认出来。
//
// 背景（2026-10-27 实测）：扫分享目录时豆包返回
// `{"code":671000000,"msg":"unknown error"}`，之前走的是 switch default，
// 直接当普通错误抛出 —— 不冷却出口、不放大间隔、不换 IP，整趟扫描中止。
// 用户看到的现象就是「几千个只进来几十个」。
func TestDbSoftThrottle(t *testing.T) {
	yes := []error{
		fmt.Errorf("豆包接口错误 671000000：unknown error"),
		fmt.Errorf("豆包接口限流：too many requests"),
		fmt.Errorf("豆包返回空响应（出口被风控）"),
		&dbThrottleErr{wait: 60e9},
	}
	for _, e := range yes {
		if !dbSoftThrottle(e) {
			t.Errorf("应判为限流：%v", e)
		}
	}
	no := []error{
		nil,
		fmt.Errorf("豆包登录已失效，请重新粘贴 Cookie"),
		fmt.Errorf("该分享是豆包新格式（飞书体系），暂不支持"),
		fmt.Errorf("豆包参数不合法"),
	}
	for _, e := range no {
		if dbSoftThrottle(e) {
			t.Errorf("不该判为限流：%v", e)
		}
	}
}

// TestDbGapGrowsSlowly 撞限流后间隔成倍放大，但不超过 10 秒。
func TestDbGapGrowsSlowly(t *testing.T) {
	oldGap, oldCur := doubaoGap, dbGapCur
	defer func() { doubaoGap, dbGapCur = oldGap, oldCur }()
	doubaoGap, dbGapCur = 2e9, 0

	for i := 0; i < 20; i++ {
		dbGapOnFail()
		if got := dbGapNow(); got > 10e9 {
			t.Fatalf("间隔涨过头了：%v", got)
		}
	}
	if got := dbGapNow(); got != 10e9 {
		t.Errorf("连续限流应涨到上限 10s，实际 %v", got)
	}
}
