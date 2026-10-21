package main

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

// ---------------------------------------------------------------------------
// 后台任务防崩
//
// Go 的 recover 只在本 goroutine 内有效，所以每个 go func 都得自己兜底。
// 本项目以前是裸 go func：巡检、追更、STRM 重建、订阅同步这些长期循环，
// 一旦内部 panic（网盘返回没预料到的结构、空指针、map 并发写……），
// 整个循环静默退出 —— 服务进程还在、端口还通，但功能已经死了，
// 不看日志根本发现不了，只能等下次重启。
//
// 两类封装：
//   goSafe     一次性任务：崩溃只记日志，不拖垮进程（HTTP handler 不用套，
//              net/http 自带 recover，每个请求 panic 只断连接）。
//   goSafeLoop 循环任务：本轮崩溃 → 记日志 → 等 backoff → 自动重启，
//              除非进程退出。
// ---------------------------------------------------------------------------

// goSafe 起一个「panic 打不死」的后台 goroutine。
// name 仅用于日志定位（如 "strm重建"）。
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[!!] 后台任务「%s」崩溃: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// goSafeLoop 起一个「崩溃后自动重启」的循环任务。
// backoff 是崩溃后的重启间隔（也是正常两轮之间的间隔由 fn 内部控制，
// 这里只管崩溃退出的重试节奏）。
//
// 注意 fn 里如果自带 time.Sleep(首轮延迟)，重启后该延迟会重新生效 ——
// 这是有意为之：崩溃后立刻重跑大概率再崩，缓一手更稳。
func goSafeLoop(name string, backoff time.Duration, fn func()) {
	if backoff <= 0 {
		backoff = 30 * time.Second
	}
	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[!!] 后台任务「%s」崩溃（%v 后自动重启）: %v\n%s",
							name, backoff, r, debug.Stack())
					}
				}()
				fn()
			}()
			time.Sleep(backoff)
		}
	}()
}

// recoverToErr 把 panic 转成 error 的 defer 辅助：用于「必须给上游一个交代」的
// 场景（如分片下载：goroutine 崩溃也必须往结果 channel 里塞个错误，
// 否则读取方永远等不到这个分片，整个下载卡死）。
// 用法：defer recoverToErr(&err)。
func recoverToErr(err *error) {
	if r := recover(); r != nil {
		if *err == nil {
			*err = fmt.Errorf("任务内部崩溃: %v", r)
		}
		log.Printf("[!!] 任务内部崩溃: %v\n%s", r, debug.Stack())
	}
}
