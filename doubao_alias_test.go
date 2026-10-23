package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// 账号短标识：有 uid_tt 取其前 6 位，没有则用 Cookie 哈希兜底。
func TestDoubaoTag(t *testing.T) {
	ck := "sessionid=abc; uid_tt=0123456789abcdef0123456789abcdef; sid_tt=zzz"
	if got := doubaoTag(ck); got != "012345" {
		t.Fatalf("应取 uid_tt 前 6 位，实际 %q", got)
	}
	bare := doubaoTag("sessionid=only-one")
	if len(bare) != 6 {
		t.Fatalf("兜底短码应为 6 位，实际 %q", bare)
	}
	if doubaoTag("sessionid=another") == bare {
		t.Fatal("不同 Cookie 不该得到相同短码")
	}
	if doubaoTag(ck) != doubaoTag(ck) {
		t.Fatal("同一个 Cookie 的短码必须稳定")
	}
}

// 改名接口：去空格、截断到 20 字、落盘可读回。
func TestAccountAliasAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	as, err := NewAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	as.Put(Account{Provider: "doubao1", Cookie: "ck", Label: "012345"})

	h := apiAccountLabel(as)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/api/accounts/label",
		strings.NewReader(`{"provider":"doubao1","label":"  主号  "}`)))
	if rec.Code != 200 {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if got := as.Get("doubao1"); got == nil || got.Alias != "主号" {
		t.Fatalf("别名没写进去：%+v", got)
	}
	if got := as.Get("doubao1").Cookie; got != "ck" {
		t.Fatal("改名不能动 Cookie")
	}

	// 重新从磁盘加载，确认持久化
	as2, err := NewAccountStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if a := as2.Get("doubao1"); a == nil || a.Alias != "主号" {
		t.Fatalf("别名没落盘：%+v", a)
	}

	// 超长名字截到 20 字
	long := strings.Repeat("长", 30)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/api/accounts/label",
		strings.NewReader(`{"provider":"doubao1","label":"`+long+`"}`)))
	if n := len([]rune(as.Get("doubao1").Alias)); n != 20 {
		t.Fatalf("应截断到 20 字，实际 %d 字", n)
	}

	// 不存在的账号要报错，不能静默新建
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/api/accounts/label",
		strings.NewReader(`{"provider":"doubao9","label":"x"}`)))
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("不存在的账号应返回失败：%s", rec.Body.String())
	}
}
