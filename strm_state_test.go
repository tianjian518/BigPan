package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildSTRMBaseChangeNeedsManualForce 锁住「地址变动 → 全量重建人工触发」：
//
//	① 地址变过 + 自动路径：存量 .strm 一个都不重写（mtime 不变），stale 标志保持；
//	② stale 期间新增条目：只写新文件（用新地址），存量依旧不动；
//	③ 人工强制（设置页按钮）：全部重写 + 状态刷新 + stale 清除；
//	④ 之后再跑自动同步：零写入（幂等）。
func TestBuildSTRMBaseChangeNeedsManualForce(t *testing.T) {
	s, root := setupSTRMTest(t)
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: true}
	self := "http://192.168.1.10:8099"
	newSelf := "https://pan.tianjianfn.de5.net"

	ep1 := filepath.Join(root, "百度/国产剧/甲/第01集.mp4.strm")
	if n, err := buildSTRM(s, self, false); err != nil || n != 1 {
		t.Fatalf("首次构建 n=%d err=%v，期望 1/nil", n, err)
	}
	st1, _ := os.Stat(ep1)
	if st := loadSTRMState(); st.BuiltBase != self {
		t.Fatalf("首次同步后应记录 built_base=%q，实际 %+v", self, st)
	}

	// ① 地址变过 + 自动路径：存量原样保留，状态不刷新
	time.Sleep(50 * time.Millisecond)
	if n, err := buildSTRM(s, newSelf, false); err != nil || n != 0 {
		t.Fatalf("地址变化后自动同步 n=%d err=%v，期望 0/nil（存量应等手动重建）", n, err)
	}
	st2, _ := os.Stat(ep1)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("地址变化后存量 .strm 被自动重写了")
	}
	if st := loadSTRMState(); st.BuiltBase != self {
		t.Fatal("地址变化后 built_base 被提前更新，stale 标志会失效")
	}
	if stale, built, cur := strmStatus(newSelf); !stale || built != self || cur != newSelf {
		t.Fatalf("stale 状态不对: stale=%v built=%q cur=%q", stale, built, cur)
	}

	// ② stale 期间新增条目：只写新文件（新地址），存量 mtime 不变
	s.Add(Item{Title: "第02集", Dir: "百度/国产剧/甲", Provider: "baidu", ShareID: "s1", FileID: "f9", Ext: ".mp4", Size: 2})
	if n, err := buildSTRM(s, newSelf, false); err != nil || n != 1 {
		t.Fatalf("stale 期间新增条目 n=%d err=%v，期望 1/nil", n, err)
	}
	ep2 := filepath.Join(root, "百度/国产剧/甲/第02集.mp4.strm")
	b, err := os.ReadFile(ep2)
	if err != nil {
		t.Fatalf("新条目 .strm 未生成: %v", err)
	}
	if !strings.Contains(string(b), newSelf) {
		t.Fatalf("新文件应使用新地址: %s", b)
	}
	st3, _ := os.Stat(ep1)
	if !st3.ModTime().Equal(st1.ModTime()) {
		t.Fatal("stale 期间存量文件又被重写了")
	}

	// ③ 人工强制：存量重写 + 状态刷新 + stale 清除
	if n, err := buildSTRM(s, newSelf, true); err != nil || n != 1 {
		t.Fatalf("强制重建 n=%d err=%v，期望 1/nil（仅 ep1 内容过期；ep2 已是新地址）", n, err)
	}
	if b, _ := os.ReadFile(ep1); !strings.Contains(string(b), newSelf) {
		t.Fatalf("强制重建后存量文件应换成新地址: %s", b)
	}
	if st := loadSTRMState(); st.BuiltBase != newSelf {
		t.Fatal("强制重建后 built_base 未更新")
	}
	if stale, _, _ := strmStatus(newSelf); stale {
		t.Fatal("强制重建后仍报 stale")
	}

	// ④ 幂等：再跑自动同步零写入
	if n, err := buildSTRM(s, newSelf, false); err != nil || n != 0 {
		t.Fatalf("重建后自动同步 n=%d err=%v，期望 0/nil", n, err)
	}
}

// TestDetectSTRMBaseSeedsStaleState 状态文件丢失（老版本升级/误删）时，
// 应从盘上 .strm 反推上次用的地址，地址守卫继续有效。
func TestDetectSTRMBaseSeedsStaleState(t *testing.T) {
	s, root := setupSTRMTest(t)
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: true}
	self := "http://192.168.1.10:8099"
	newSelf := "https://new.example.com"

	if n, err := buildSTRM(s, self, false); err != nil || n != 1 {
		t.Fatalf("首次构建 n=%d err=%v", n, err)
	}
	if err := os.Remove(strmStatePath()); err != nil {
		t.Fatal(err)
	}
	ep1 := filepath.Join(root, "百度/国产剧/甲/第01集.mp4.strm")
	st1, _ := os.Stat(ep1)

	// 地址变过：反推出旧地址 → 按 stale 处理，不自动重写
	time.Sleep(50 * time.Millisecond)
	if n, err := buildSTRM(s, newSelf, false); err != nil || n != 0 {
		t.Fatalf("状态丢失+地址变化 n=%d err=%v，期望 0/nil", n, err)
	}
	st2, _ := os.Stat(ep1)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("存量文件被重写了")
	}
	if stale, built, _ := strmStatus(newSelf); !stale || built != self {
		t.Fatalf("应从 .strm 反推旧地址: stale=%v built=%q", stale, built)
	}

	// 地址没变：反推地址与当前一致 → 不拦截，正常同步并把地址落盘
	if n, err := buildSTRM(s, self, false); err != nil || n != 0 {
		t.Fatalf("状态丢失+地址没变 n=%d err=%v，期望 0/nil（内容一致不应重写）", n, err)
	}
	if st := loadSTRMState(); st.BuiltBase != self {
		t.Fatalf("这次同步后应把反推出的地址落盘，实际 %+v", st)
	}
}

// TestDetectSTRMBase 反推逻辑：/direct/ 与 /play/ 两种形态、空目录、无标记文件。
func TestDetectSTRMBase(t *testing.T) {
	// 空目录
	if got := detectSTRMBase(t.TempDir()); got != "" {
		t.Errorf("空目录应返回空串，实际 %q", got)
	}
	// 空串根目录也不能炸
	if got := detectSTRMBase(""); got != "" {
		t.Errorf("空根目录应返回空串，实际 %q", got)
	}
	// /direct/ 形态（非夸克）
	d1 := t.TempDir()
	sub := filepath.Join(d1, "a", "b")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sub, "x.strm"), []byte("http://192.168.1.10:8099/direct/a/b/x.mp4?k=abc"), 0644)
	os.WriteFile(filepath.Join(d1, "note.txt"), []byte("not a strm"), 0644)
	if got := detectSTRMBase(d1); got != "http://192.168.1.10:8099" {
		t.Errorf("/direct/ 形态反推错: %q", got)
	}
	// /play/ 形态（夸克）
	d2 := t.TempDir()
	os.WriteFile(filepath.Join(d2, "q.strm"), []byte("https://pan.example.com/play/q.mkv?id=1&size=2"), 0644)
	if got := detectSTRMBase(d2); got != "https://pan.example.com" {
		t.Errorf("/play/ 形态反推错: %q", got)
	}
	// 内容没有标记：反推不出，返回空（调用方按不拦截处理）
	d3 := t.TempDir()
	os.WriteFile(filepath.Join(d3, "weird.strm"), []byte("garbage"), 0644)
	if got := detectSTRMBase(d3); got != "" {
		t.Errorf("无标记内容应返回空串，实际 %q", got)
	}
}

// TestSTRMStateRoundtrip 状态文件读写闭环。
func TestSTRMStateRoundtrip(t *testing.T) {
	dir := t.TempDir()
	old := strmConfigPath
	defer func() { strmConfigPath = old }()
	strmConfigPath = filepath.Join(dir, "strm_config.json")

	// 文件不存在：零值，不炸
	if st := loadSTRMState(); st.BuiltBase != "" {
		t.Errorf("无文件时应为零值，实际 %+v", st)
	}
	// 落盘再读回
	want := strmState{BuiltBase: "http://192.168.1.10:8099", BuiltAt: time.Now()}
	saveSTRMState(want)
	got := loadSTRMState()
	if got.BuiltBase != want.BuiltBase || !got.BuiltAt.Equal(want.BuiltAt) {
		t.Errorf("读写不一致: got %+v want %+v", got, want)
	}
	// 路径确实在配置文件同目录
	if p := strmStatePath(); p != filepath.Join(dir, "strm_state.json") {
		t.Errorf("状态文件路径错: %q", p)
	}
}
