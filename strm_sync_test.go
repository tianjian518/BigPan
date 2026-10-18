package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupSTRMTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a","title":"第01集","provider":"baidu","dir":"百度/国产剧/甲","share_id":"s1","file_id":"f1","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	// 配置文件与状态文件都指到临时目录，避免测试写 /data/ 下的真实文件
	oldRoot, oldCfg, oldCfgPath := strmRoot, strmCfg, strmConfigPath
	t.Cleanup(func() { strmRoot = oldRoot; strmCfg = oldCfg; strmConfigPath = oldCfgPath })
	strmRoot = filepath.Join(dir, "strm")
	strmConfigPath = filepath.Join(dir, "strm_config.json")
	return s, strmRoot
}

// 导入/删除后应自动生成 STRM（强同步的异步实现）。
func TestRebuildSTRMAsyncGenerates(t *testing.T) {
	s, root := setupSTRMTest(t)
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: true}

	rebuildSTRMAsync(s, "http://192.168.1.10:8099", "测试")

	want := filepath.Join(root, "百度/国产剧/甲/第01集.mp4.strm")
	for i := 0; i < 40; i++ {
		time.Sleep(100 * time.Millisecond)
		if b, err := os.ReadFile(want); err == nil {
			if !strings.Contains(string(b), "/direct/") || !strings.Contains(string(b), "192.168.1.10:8099") {
				t.Fatalf("内容不对: %s", string(b))
			}
			return
		}
	}
	t.Fatal("超时：STRM 未生成")
}

// 关掉自动重建时不应后台生成（只能手动点）。
func TestRebuildSTRMAsyncDisabled(t *testing.T) {
	s, root := setupSTRMTest(t)
	strmCfg = STRMConfig{Layout: "full", AutoRebuild: false}

	rebuildSTRMAsync(s, "http://192.168.1.10:8099", "测试")
	time.Sleep(700 * time.Millisecond)

	if _, err := os.Stat(filepath.Join(root, "百度/国产剧/甲/第01集.mp4.strm")); err == nil {
		t.Fatal("AutoRebuild=false 仍生成了 STRM")
	}
}
