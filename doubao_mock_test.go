package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 用 mock 服务器复现「扫到一半被风控 → 断点落盘 → 恢复后续扫」。
// 不依赖真实网络，可重复跑。
func TestDoubaoScanResumeMock(t *testing.T) {
	// 造一棵树：root → 5 个分类 → 每个 5 个子目录 → 每个 3 个视频
	children := map[string][]dbNode{}
	root := "root"
	var lv1, lv2 []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("a%d", i)
		lv1 = append(lv1, id)
		children[root] = append(children[root], dbNode{ID: id, Name: id, NodeType: 1})
	}
	for _, a := range lv1 {
		for j := 0; j < 5; j++ {
			id := a + fmt.Sprintf("b%d", j)
			lv2 = append(lv2, id)
			children[a] = append(children[a], dbNode{ID: id, Name: id, NodeType: 1})
		}
	}
	for _, b := range lv2 {
		for k := 0; k < 3; k++ {
			children[b] = append(children[b], dbNode{
				ID: b + fmt.Sprintf("f%d", k), Name: fmt.Sprintf("ep%d.mkv", k), NodeType: 6, Size: 100,
			})
		}
	}

	var budget int64 = 18 // 只放行 18 次请求，之后一律空体（模拟风控）
	var calls int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		if atomic.LoadInt64(&budget) <= 0 {
			w.WriteHeader(200) // 豆包风控的经典姿势：200 + 空体
			return
		}
		atomic.AddInt64(&budget, -1)
		var body struct {
			NodeID string `json:"node_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		list := children[body.NodeID]
		if list == nil {
			list = []dbNode{}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"children": list, "has_more": false},
		})
	}))
	defer srv.Close()

	// 清掉全局状态，避免和其它测试串味
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()
	key := "own|mockslot|" + root
	dbScanDone(key)

	cli := newDoubaoAt("test", srv.URL)

	_, files, err := scanDoubaoOwn(cli, "mockslot", root, "")
	if err == nil {
		t.Fatal("预算耗尽时应当失败，而不是静默返回半截数据")
	}
	if !strings.Contains(err.Error(), "接着扫") {
		t.Fatalf("错误里应提示进度已保存，实际：%v", err)
	}
	if len(files) != 0 {
		t.Fatalf("失败时不该返回任何文件，实际 %d 个", len(files))
	}
	st := dbScanTake(key)
	if len(st.Seen) == 0 {
		t.Fatal("断点没落盘")
	}
	t.Logf("第一轮：扫到 %d 个目录，err=%v", len(st.Seen), err)

	// 放开预算，续扫
	atomic.StoreInt64(&budget, 1000)
	atomic.StoreInt64(&calls, 0)
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	_, files2, err2 := scanDoubaoOwn(cli, "mockslot", root, "")
	if err2 != nil {
		t.Fatalf("续扫失败：%v", err2)
	}
	if len(files2) != 75 {
		t.Fatalf("应该扫到 75 个视频，实际 %d", len(files2))
	}
	// 续扫时不该从 root 重来：31 个目录减去已扫的，请求数应明显更少
	used := atomic.LoadInt64(&calls)
	if used >= 31 {
		t.Fatalf("续扫请求数 %d 不少于全量 31，说明没走断点", used)
	}
	t.Logf("续扫：%d 个视频，只用了 %d 次请求（全量需 31 次）", len(files2), used)

	if again := dbScanTake(key); len(again.Seen) != 0 {
		t.Fatal("扫完应清掉断点")
	}
}

// 风控打断后，同一个 client 在冷却期内不该再往外打请求。
func TestDoubaoCoolingBlocksRequestsMock(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(200) // 永远空体
	}))
	defer srv.Close()

	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	cli := newDoubaoAt("test", srv.URL)

	_, err := cli.post("/samantha/aispace/homepage", map[string]any{})
	if err == nil {
		t.Fatal("应该失败")
	}
	first := atomic.LoadInt64(&hits)
	t.Logf("第一次请求打了 %d 次，错误：%v", first, err)

	// 冷却期内再请求：不该产生任何新的网络请求
	_, err2 := cli.post("/samantha/aispace/homepage", map[string]any{})
	second := atomic.LoadInt64(&hits)
	if second != first {
		t.Fatalf("冷却期内又打了 %d 次（%d → %d），守卫没生效", second-first, first, second)
	}
	if !isThrottleErr(err2) {
		t.Fatalf("冷却期应返回风控错误，实际 %T：%v", err2, err2)
	}
	t.Logf("冷却期正确拦截：%v", err2)

	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()
}
