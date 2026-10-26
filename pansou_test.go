package main

// 资源搜索代理：解析 PanSou 的 merged_by_type、拍平、排序、拼提取码。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPanSouSearchProxy(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("kw") != "庆余年" {
			http.Error(w, "kw missing", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"total": 3,
				"merged_by_type": map[string]any{
					"quark": []map[string]any{
						{"url": "https://pan.quark.cn/s/aaa", "password": "1234", "note": "庆余年 第二季", "datetime": "2026-10-24 10:00:00"},
						{"url": "https://pan.quark.cn/s/bbb", "password": "", "note": "庆余年 第一季", "datetime": "2026-01-01 10:00:00"},
					},
					"baidu": []map[string]any{
						{"url": "https://pan.baidu.com/s/ccc", "password": "abcd", "note": "庆余年 合集", "datetime": "2026-10-25 09:00:00"},
					},
				},
			},
		})
	}))
	defer fake.Close()

	old := panSouCfg
	defer func() { panSouCfg = old }()
	panSouCfg = panSouConfig{URL: fake.URL, Limit: 60}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/pansou/search?kw=庆余年", nil)
	apiPanSouSearch()(rec, req)

	var out struct {
		OK    bool         `json:"ok"`
		Total int          `json:"total"`
		Items []panSouItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}
	if !out.OK || len(out.Items) != 3 {
		t.Fatalf("ok=%v items=%d，应为 true/3：%s", out.OK, len(out.Items), rec.Body.String())
	}
	// 按时间倒序：10-25 → 10-24 → 01-01
	if out.Items[0].CloudCN != "百度" || out.Items[1].CloudCN != "夸克" {
		t.Fatalf("排序不对：%s / %s", out.Items[0].CloudCN, out.Items[1].CloudCN)
	}
	// 提取码要拼进链接
	if out.Items[1].Link != "https://pan.quark.cn/s/aaa?pwd=1234" {
		t.Fatalf("提取码没拼对：%s", out.Items[1].Link)
	}
	// 没有提取码的保持原样
	if out.Items[2].Link != "https://pan.quark.cn/s/bbb" {
		t.Fatalf("无提取码被改了：%s", out.Items[2].Link)
	}
}

func TestPanSouSearchGuard(t *testing.T) {
	old := panSouCfg
	defer func() { panSouCfg = old }()

	// 没配置地址：要报错，不能去请求任何东西
	panSouCfg = panSouConfig{Limit: 60}
	rec := httptest.NewRecorder()
	apiPanSouSearch()(rec, httptest.NewRequest(http.MethodGet, "/api/pansou/search?kw=x", nil))
	if rec.Code != 200 || !containsStr(rec.Body.String(), "还没配置") {
		t.Fatalf("未配置时应报错：%s", rec.Body.String())
	}

	// 空关键词
	rec = httptest.NewRecorder()
	apiPanSouSearch()(rec, httptest.NewRequest(http.MethodGet, "/api/pansou/search?kw=", nil))
	if !containsStr(rec.Body.String(), "请输入关键词") {
		t.Fatalf("空关键词应报错：%s", rec.Body.String())
	}

	// 非法地址（file:// 之类）必须拒绝，防 SSRF
	panSouCfg = panSouConfig{URL: "file:///etc/passwd", Limit: 60}
	rec = httptest.NewRecorder()
	apiPanSouSearch()(rec, httptest.NewRequest(http.MethodGet, "/api/pansou/search?kw=x", nil))
	if !containsStr(rec.Body.String(), "地址格式不对") {
		t.Fatalf("非法协议应被拒：%s", rec.Body.String())
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
