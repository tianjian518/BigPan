package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTianyiSubAPI 走一遍订阅号 HTTP 接口（联网，默认跳过）。
// 用法：T189_UP=xxx go test -run TestTianyiSubAPI -v
func TestTianyiSubAPI(t *testing.T) {
	up := os.Getenv("T189_UP")
	if up == "" {
		t.Skip("未设置 T189_UP，跳过联网测试")
	}
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	subs := newTianyiSubStore(filepath.Join(dir, "tianyi_subs.json"))
	link := "https://content.21cn.com/h5/subscrip/index.html#/pages/own-home/index?uuid=" + up

	// 1. 新增订阅（关自动，避免测试改动水位）
	body := `{"link":"` + link + `","auto_import":false}`
	w := httptest.NewRecorder()
	apiTianyiSubs(subs)(w, httptest.NewRequest("POST", "/api/tianyi/subs", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("add http %d: %s", w.Code, w.Body.String())
	}
	var addResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &addResp)
	if ok, _ := addResp["ok"].(bool); !ok {
		t.Fatalf("add failed: %s", w.Body.String())
	}
	t.Logf("add ok: %s", w.Body.String())

	// 2. 列表
	w = httptest.NewRecorder()
	apiTianyiSubs(subs)(w, httptest.NewRequest("GET", "/api/tianyi/subs", nil))
	var listResp struct {
		Subs []tianyiSubEntry `json:"subs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Subs) != 1 || listResp.Subs[0].UserID != up {
		t.Fatalf("list wrong: %s", w.Body.String())
	}
	if listResp.Subs[0].Name == "" {
		t.Errorf("订阅号昵称为空")
	}

	// 3. 浏览分享
	w = httptest.NewRecorder()
	apiTianyiSubShares(store, subs)(w, httptest.NewRequest("GET", "/api/tianyi/sub-shares?user_id="+up+"&page=1&size=10", nil))
	var shareResp struct {
		OK     bool `json:"ok"`
		Total  int  `json:"total"`
		Shares []struct {
			ShareID  int64  `json:"share_id"`
			Name     string `json:"name"`
			IsFolder bool   `json:"is_folder"`
			Imported int    `json:"imported"`
		} `json:"shares"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &shareResp)
	if !shareResp.OK || len(shareResp.Shares) == 0 {
		t.Fatalf("sub-shares failed: %s", w.Body.String())
	}
	t.Logf("分享总数 %d，首页 %d 条，首条: %s (folder=%v)", shareResp.Total, len(shareResp.Shares), shareResp.Shares[0].Name, shareResp.Shares[0].IsFolder)

	// 4. 删除
	w = httptest.NewRecorder()
	apiTianyiSubItem(subs)(w, httptest.NewRequest("DELETE", "/api/tianyi/subs/"+up, nil))
	t.Logf("delete resp: %s", w.Body.String())
	if subs.Get(up) != nil {
		t.Errorf("删除后仍存在")
	}
}

// TestTianyiBackfillQuick 历史导入跑一条（联网，默认跳过）。
// 用法：T189_UP=xxx go test -run TestTianyiBackfillQuick -v
func TestTianyiBackfillQuick(t *testing.T) {
	up := os.Getenv("T189_UP")
	if up == "" {
		t.Skip("未设置 T189_UP")
	}
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	subs := newTianyiSubStore(filepath.Join(dir, "tianyi_subs.json"))
	subs.Put(tianyiSubEntry{UserID: up, Name: "测试", AutoImport: true, AddedAt: time.Now()})

	if err := tianyiBackfillStart(store, subs, "", up, 3, 10*time.Millisecond); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		j := tianyiBackfillJob.snapshot()
		if !j["running"].(bool) {
			t.Logf("结束：处理 %v，收录 %v，条目 %v，失败 %v，错误 %v", j["done"], j["imported"], j["added"], j["failed"], j["error"])
			if e, _ := j["imported"].(int); e < 1 {
				t.Errorf("没收录到任何分享：%v", j["error"])
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(store.List()) == 0 {
		t.Error("库里没有条目")
	}
	e := subs.Get(up)
	if e == nil || len(e.Done) == 0 {
		t.Error("断点没记录")
	}
}
