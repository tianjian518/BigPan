package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pointQuarkAPI 把夸克 API 基址指到 httptest 假服务器，返回还原函数。
// quarkAPIBase 提成 var 就为了这个（同 provider_baidu_walk_test.go 套路）。
func pointQuarkAPI(t *testing.T, srvURL string) {
	t.Helper()
	old := quarkAPIBase
	quarkAPIBase = srvURL
	t.Cleanup(func() { quarkAPIBase = old })
}

// mockQuarkShare 起一个假夸克：ShareToken 回固定 stoken；detail 回给定的
// share 元数据 JSON 与文件列表 JSON。listJSON 传 "" 表示空列表。
func mockQuarkShare(t *testing.T, shareJSON, listJSON string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/token"):
			w.Write([]byte(`{"code":0,"message":"ok","data":{"stoken":"FAKE_STOKEN","title":"测试分享"}}`))
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/detail"):
			list := listJSON
			if list == "" {
				list = "[]"
			}
			fmt.Fprintf(w, `{"code":0,"message":"ok","data":{"share":%s,"list":%s}}`, shareJSON, list)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	pointQuarkAPI(t, srv.URL)
}

// TestShareStatusParsesViolation ShareStatus 正确解析审核屏蔽元数据。
func TestShareStatusParsesViolation(t *testing.T) {
	mockQuarkShare(t,
		`{"title":"M_人~余（2026）","status":3,"partial_violation":true,"violation_cnt":12,"file_num":1,"all_file_num":0}`,
		"")
	q := newQuark("")
	meta, err := q.ShareStatus("a057ca6793da", "FAKE_STOKEN")
	if err != nil {
		t.Fatalf("ShareStatus 失败: %v", err)
	}
	if !meta.Blocked() {
		t.Fatalf("partial_violation=true + violation_cnt=12 应判屏蔽，实际 Blocked()=false：%+v", meta)
	}
	if meta.ViolationCnt != 12 || meta.Status != 3 {
		t.Fatalf("元数据解析不全：%+v", meta)
	}
}

// TestShareStatusHealthyNotBlocked 正常分享（无 violation 字段）不误判屏蔽。
func TestShareStatusHealthyNotBlocked(t *testing.T) {
	mockQuarkShare(t, `{"title":"正常分享","status":1,"file_num":40,"all_file_num":40}`, "")
	q := newQuark("")
	meta, err := q.ShareStatus("healthy", "FAKE_STOKEN")
	if err != nil {
		t.Fatalf("ShareStatus 失败: %v", err)
	}
	if meta.Blocked() {
		t.Fatalf("正常分享不应判屏蔽：%+v", meta)
	}
}

// TestScanQuarkShareBlockedByAudit 分享被审核屏蔽（列表空 + 元数据实锤）
// → 报「审核屏蔽」，而不是老的「没有找到视频文件」。
// 复刻 2026-10-20《美人余》事故：ShareToken 成功但内容全下架。
func TestScanQuarkShareBlockedByAudit(t *testing.T) {
	mockQuarkShare(t,
		`{"title":"M_人~余（2026）","partial_violation":true,"violation_cnt":12}`,
		"")
	_, _, err := scanQuarkShare("a057ca6793da", "")
	if err == nil {
		t.Fatal("屏蔽分享应返回错误")
	}
	if !strings.Contains(err.Error(), "审核屏蔽") {
		t.Fatalf("错误应含「审核屏蔽」，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "12") {
		t.Fatalf("错误应带出下架文件数，实际：%v", err)
	}
}

// TestScanQuarkShareEmptyNoViolation 列表空但无屏蔽元数据 → 维持老文案
// 「没有找到视频文件」（139 ISO 分享这类「分享还在、扫不出视频」的场景
// 不能被误判成失效，shareGone 靠这个区分）。
func TestScanQuarkShareEmptyNoViolation(t *testing.T) {
	mockQuarkShare(t, `{"title":"ISO 分享","status":1}`, "")
	_, _, err := scanQuarkShare("isoshare", "")
	if err == nil {
		t.Fatal("空分享应返回错误")
	}
	if !strings.Contains(err.Error(), "没有找到视频文件") {
		t.Fatalf("无屏蔽元数据时应维持老文案，实际：%v", err)
	}
	if shareGone(err) {
		t.Fatal("「没有找到视频文件」不是失效类错误，shareGone 必须返回 false（防误删 139 ISO 分享）")
	}
}

// TestShareGoneAuditBlocked 「审核屏蔽」文案必须被 shareGone 判失效——
// 否则巡检永远漏判（美人余 9 条烂条目在库里躺了 6 小时的直接原因）。
func TestShareGoneAuditBlocked(t *testing.T) {
	err := fmt.Errorf("分享内容被网盘审核屏蔽（12 个文件违规下架），请更换分享源或删除该条目")
	if !shareGone(err) {
		t.Fatal("「审核屏蔽」必须判失效")
	}
}

// TestQuarkFailCache 分享级失败缓存：命中 / 过期 / 清除 / 账号切换全清。
func TestQuarkFailCache(t *testing.T) {
	quarkFailCacheReset()
	const sid, fid = "shareX", "fileY"

	// 没记过 → 不命中
	if _, ok := quarkFailCached(sid, fid); ok {
		t.Fatal("空缓存不应命中")
	}

	// 记过 → 命中且错误原样返回
	want := fmt.Errorf("分享内容被夸克审核屏蔽（3 个文件违规下架）")
	quarkFailRemember(sid, fid, want)
	got, ok := quarkFailCached(sid, fid)
	if !ok || got == nil || got.Error() != want.Error() {
		t.Fatalf("缓存应命中且错误一致，实际 ok=%v err=%v", ok, got)
	}

	// 别的条目不受影响
	if _, ok := quarkFailCached("other", "other"); ok {
		t.Fatal("缓存不应串条目")
	}

	// 清除后不命中
	quarkFailClear(sid, fid)
	if _, ok := quarkFailCached(sid, fid); ok {
		t.Fatal("清除后不应命中")
	}

	// TTL 过期不命中（把记账时间改到过去）
	quarkFailRemember(sid, fid, want)
	quarkFailMu.Lock()
	quarkFailCache[quarkFailKey(sid, fid)] = quarkFailEntry{err: want, at: time.Now().Add(-2 * quarkFailTTL)}
	quarkFailMu.Unlock()
	if _, ok := quarkFailCached(sid, fid); ok {
		t.Fatal("超过 TTL 不应命中")
	}

	// 账号切换全清
	quarkFailRemember(sid, fid, want)
	quarkFailCacheReset()
	if _, ok := quarkFailCached(sid, fid); ok {
		t.Fatal("Reset 后不应命中")
	}
}

// TestQuarkLinkCacheTTL 302 直链短缓存：命中直接返、过期重取、Reset 清空
// （2026-10-20 §23.59：播放器每连接都要一次链，缓存 60s 把 API 调用量
// 降到约十分之一，顺带降风控面）。
func TestQuarkLinkCacheTTL(t *testing.T) {
	quarkLinkReset()
	if _, ok := quarkLinkCached("it-1"); ok {
		t.Fatal("空缓存不应命中")
	}
	quarkLinkRemember("it-1", "https://cdn.example/a.mp4")
	u, ok := quarkLinkCached("it-1")
	if !ok || u != "https://cdn.example/a.mp4" {
		t.Fatalf("应命中刚存的链，got %q ok=%v", u, ok)
	}
	// 过期：把时间戳拨到 TTL 之外
	quarkLinkMu.Lock()
	quarkLinkCache["it-1"] = quarkLinkEntry{url: u, at: time.Now().Add(-2 * quarkLinkTTL)}
	quarkLinkMu.Unlock()
	if _, ok := quarkLinkCached("it-1"); ok {
		t.Fatal("超过 TTL 不应命中")
	}
	quarkLinkRemember("it-1", "https://cdn.example/b.mp4")
	quarkLinkReset()
	if _, ok := quarkLinkCached("it-1"); ok {
		t.Fatal("Reset 后不应命中")
	}
}
