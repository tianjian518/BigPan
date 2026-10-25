package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 订阅号/资源号标题里的符号污染：emoji、装饰符号、零宽字符一律清掉，
// 中英文与常用标点原样保留（Emby 刮削器认不了 emoji 目录名）。
func TestCleanShareTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"✨流浪地球❄️", "流浪地球"},
		{"⛄国产剧⭐【1080P】", "国产剧【1080P】"},
		{"\u200d\uFE0F测试\u200d", "测试"},
		{"行\t分隔", "行分隔"},               // 控制字符整体丢掉
		{"第 一 季", "第 一 季"},             // 空格保留
		{"Castle (2019) S01", "Castle (2019) S01"},
		{"中文，标点。！？", "中文，标点。！？"},
		{"全角ＡＢＣ１２３", "全角ＡＢＣ１２３"},
		{"破折号——省略……", "破折号——省略……"},
		{"❄️", ""}, // 清完为空：上层会兜「未命名」
		{"", ""},
	}
	for _, c := range cases {
		if got := cleanShareTitle(c.in); got != c.want {
			t.Errorf("cleanShareTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// itemDirWithShow 只换剧名段：分类段、分享内子目录、文件后缀都不能动。
func TestItemDirWithShow(t *testing.T) {
	cases := []struct {
		name    string
		item    Item
		newShow string
		want    string
	}{
		{
			name:    "平铺单文件",
			item:    Item{Provider: "tianyi", Dir: "天翼/老剧", Show: "老剧"},
			newShow: "新剧",
			want:    "天翼/新剧",
		},
		{
			name:    "带一级分类",
			item:    Item{Provider: "tianyi", Dir: "天翼/电视剧/老剧", Show: "老剧", Category: "电视剧"},
			newShow: "新剧",
			want:    "天翼/电视剧/新剧",
		},
		{
			name:    "带两级分类与子目录",
			item:    Item{Provider: "tianyi", Dir: "天翼/电视剧/国产剧/老剧/第一季/蓝光", Show: "老剧", Category: "电视剧/国产剧"},
			newShow: "新剧",
			want:    "天翼/电视剧/国产剧/新剧/第一季/蓝光",
		},
		{
			name:    "英文网盘名目录",
			item:    Item{Provider: "baidu", Dir: "baidu/老剧", Show: "老剧"},
			newShow: "新剧",
			want:    "baidu/新剧",
		},
		{
			name:    "目录里找不到旧剧名时按分类段数兜底",
			item:    Item{Provider: "tianyi", Dir: "天翼/电视剧/老剧", Show: "对不上的旧名", Category: "电视剧"},
			newShow: "新剧",
			want:    "天翼/电视剧/新剧",
		},
	}
	for _, c := range cases {
		if got := itemDirWithShow(&c.item, c.newShow); got != c.want {
			t.Errorf("%s: itemDirWithShow = %q, want %q", c.name, got, c.want)
		}
	}
}

// 改名接口端到端：Show/Dir 整批跟改，追更登记同步，别的分享不波及。
func TestApiShareRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"a1","title":"第1集","provider":"tianyi","dir":"天翼/电视剧/❄️老剧","show":"❄️老剧","category":"电视剧","share_id":"123","file_id":"f1","ext":".mp4","sub_user_id":"u1"},
{"id":"a2","title":"第2集","provider":"tianyi","dir":"天翼/电视剧/❄️老剧/子目录","show":"❄️老剧","category":"电视剧","share_id":"123","file_id":"f2","ext":".mp4","sub_user_id":"u1"},
{"id":"b1","title":"第1集","provider":"tianyi","dir":"天翼/另一部","show":"另一部","share_id":"456","file_id":"f3","ext":".mp4"}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}

	old := globalShares
	gs, _ := NewShareStore(filepath.Join(dir, "shares.json"))
	globalShares = gs
	defer func() { globalShares = old }()

	body := `{"provider":"tianyi","share_id":"123","show":"国产剧"}`
	req := httptest.NewRequest(http.MethodPost, "/api/shares/rename", strings.NewReader(body))
	rec := httptest.NewRecorder()
	apiShareRename(s, "")(rec, req)

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Fatalf("改名失败: %v", resp)
	}
	if resp["renamed"].(float64) != 2 {
		t.Fatalf("应改 2 条，实际 %v", resp["renamed"])
	}
	items := s.List()
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if byID["a1"].Show != "国产剧" || byID["a1"].Dir != "天翼/电视剧/国产剧" {
		t.Fatalf("a1 没跟改: %+v", byID["a1"])
	}
	// 分享内子目录必须保留
	if byID["a2"].Dir != "天翼/电视剧/国产剧/子目录" {
		t.Fatalf("a2 子目录丢了: %+v", byID["a2"])
	}
	// 别的分享不能动
	if byID["b1"].Show != "另一部" || byID["b1"].Dir != "天翼/另一部" {
		t.Fatalf("波及其他分享: %+v", byID["b1"])
	}
	// 追更登记同步换名 + 补上分类
	m := globalShares.Get("tianyi", "123")
	if m == nil || m.Show != "国产剧" || m.Category != "电视剧" {
		t.Fatalf("追更登记没同步: %+v", m)
	}
}

// 空剧名（全是符号）必须被拒，不能把剧名清成空目录。
func TestApiShareRenameRejectsEmpty(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.AddBatch([]Item{{ID: "a1", Title: "第1集", Provider: "tianyi", Dir: "天翼/老剧", Show: "老剧", ShareID: "123", FileID: "f1", Ext: ".mp4"}})

	req := httptest.NewRequest(http.MethodPost, "/api/shares/rename", strings.NewReader(`{"provider":"tianyi","share_id":"123","show":"❄️✨"}`))
	rec := httptest.NewRecorder()
	apiShareRename(s, "")(rec, req)

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != false {
		t.Fatalf("纯符号剧名应被拒绝: %v", resp)
	}
	if s.List()[0].Show != "老剧" {
		t.Fatal("被拒后条目不该被动过")
	}
}
