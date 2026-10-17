package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 批量写入：一次落盘、同路径覆盖、不产生重复。
func TestAddBatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(filepath.Join(dir, "items.json"))

	n := s.AddBatch([]Item{
		{Title: "第1集", Dir: "百度/剧A", Provider: "baidu", ShareID: "s1", FileID: "f1", Ext: ".mp4"},
		{Title: "第2集", Dir: "百度/剧A", Provider: "baidu", ShareID: "s1", FileID: "f2", Ext: ".mp4"},
	})
	if n != 2 || len(s.List()) != 2 {
		t.Fatalf("批量写入异常: n=%d len=%d", n, len(s.List()))
	}
	// 同路径覆盖：数量不变，size 更新
	id0 := s.List()[0].ID
	s.AddBatch([]Item{{Title: "第1集", Dir: "百度/剧A", Provider: "baidu", ShareID: "s1", FileID: "f1", Ext: ".mp4", Size: 999}})
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("覆盖后数量变了: %d", len(list))
	}
	if list[0].ID != id0 || list[0].Size != 999 {
		t.Fatalf("覆盖逻辑不对: %+v", list[0])
	}
}

// 追更列表分页：不传 size 返回全部；传 size 只返回当前页，total 正确。
func TestAPISharesPagination(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 25; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(
			`{"id":"%d","title":"t","provider":"baidu","dir":"百度/剧%d","show":"剧%d","share_id":"s%d","file_id":"f","ext":".mp4","added_at":"2026-10-%02dT10:00:00+08:00"}`,
			i, i, i, i, i+1))
	}
	sb.WriteString("]")
	os.WriteFile(p, []byte(sb.String()), 0644)
	s, _ := NewStore(p)

	old := globalShares
	defer func() { globalShares = old }()
	gs, _ := NewShareStore(filepath.Join(dir, "shares.json"))
	globalShares = gs

	h := apiShares(s)
	call := func(q string) map[string]interface{} {
		req := httptest.NewRequest("GET", "/api/shares"+q, nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var out map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
		return out
	}

	all := call("")
	if int(all["total"].(float64)) != 25 {
		t.Fatalf("total=%v", all["total"])
	}
	if len(all["shares"].([]interface{})) != 25 {
		t.Fatalf("不分页应返回 25 条")
	}

	p2 := call("?page=2&size=10")
	if int(p2["total"].(float64)) != 25 || len(p2["shares"].([]interface{})) != 10 {
		t.Fatalf("分页异常: total=%v len=%d", p2["total"], len(p2["shares"].([]interface{})))
	}
	if int(p2["page"].(float64)) != 2 || int(p2["size"].(float64)) != 10 {
		t.Fatalf("page/size 回显异常")
	}

	// 越界页：返回空列表，不报错
	p9 := call("?page=9&size=10")
	if len(p9["shares"].([]interface{})) != 0 {
		t.Fatalf("越界页应返回空")
	}
}
