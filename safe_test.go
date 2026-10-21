package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGoSafeRecoversPanic：goSafe 里的 panic 不能杀进程。
func TestGoSafeRecoversPanic(t *testing.T) {
	done := make(chan struct{})
	goSafe("test-once", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goSafe 内的函数没有执行完")
	}
	// 能跑到这里说明进程没被带崩
}

// TestGoSafeLoopRestarts：循环任务 panic 后必须自动重启，不能静默死亡。
func TestGoSafeLoopRestarts(t *testing.T) {
	var runs int32
	var once sync.Once
	release := make(chan struct{})
	goSafeLoop("test-loop", 50*time.Millisecond, func() {
		n := atomic.AddInt32(&runs, 1)
		if n == 1 {
			panic("first-run-boom") // 第一轮就崩
		}
		if n >= 3 { // 跑到第 3 轮 = 崩溃后重启成功
			once.Do(func() { close(release) })
		}
	})
	select {
	case <-release:
	case <-time.After(3 * time.Second):
		t.Fatalf("循环任务没有自动重启，只跑了 %d 轮", atomic.LoadInt32(&runs))
	}
}

// TestRecoverToErr：defer recoverToErr(&err) 能把 panic 转成 error。
func TestRecoverToErr(t *testing.T) {
	var err error
	func() {
		defer recoverToErr(&err)
		panic("kaboom")
	}()
	if err == nil {
		t.Fatal("panic 没有被转成 error")
	}
}
