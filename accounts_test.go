package main

import (
	"path/filepath"
	"testing"
	"time"
)

// 换号：新凭证覆盖旧凭证，并且旧直链缓存必须被清空。
func TestInstallAccountSwitchesCredential(t *testing.T) {
	as, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 先塞一条旧号的直链缓存
	putPlayTarget("ep1", &playTarget{url: "https://old.example/x", exp: time.Now().Add(time.Hour)})
	if len(ptCache) == 0 {
		t.Fatal("缓存未写入")
	}

	installAccount(as, Account{Provider: "quark", Label: "小号", Cookie: "ck-old", LoginAt: time.Now()})
	if got := as.Get("quark"); got == nil || got.Label != "小号" {
		t.Fatalf("首次导入失败: %+v", got)
	}

	// 换大号
	installAccount(as, Account{Provider: "quark", Label: "大号", Cookie: "ck-new", LoginAt: time.Now()})
	got := as.Get("quark")
	if got == nil || got.Label != "大号" || got.Cookie != "ck-new" {
		t.Fatalf("换号未生效: %+v", got)
	}
	if len(as.List()) != 1 {
		t.Fatalf("同一网盘应只有 1 条凭证，实际 %d", len(as.List()))
	}
	if len(ptCache) != 0 {
		t.Fatalf("换号后旧直链缓存未清空，剩 %d 条", len(ptCache))
	}
}

// 退出登录：删凭证 + 清缓存，且不影响其他网盘。
func TestUninstallAccount(t *testing.T) {
	as, _ := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	installAccount(as, Account{Provider: "quark", Label: "夸克号", Cookie: "ck-q"})
	installAccount(as, Account{Provider: "baidu", Label: "百度号", Cookie: "BDUSS=x"})
	putPlayTarget("ep1", &playTarget{url: "https://old/x", exp: time.Now().Add(time.Hour)})

	if !uninstallAccount(as, "quark") {
		t.Fatal("退出失败")
	}
	if as.Get("quark") != nil {
		t.Fatal("夸克凭证未删除")
	}
	if as.Get("baidu") == nil {
		t.Fatal("百度凭证被误删")
	}
	if len(ptCache) != 0 {
		t.Fatal("退出后直链缓存未清空")
	}
}

// 同一凭证重复导入不应产生多余记录，也不应误清其他网盘缓存之外的东西。
func TestInstallSameAccountIdempotent(t *testing.T) {
	as, _ := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	a := Account{Provider: "mobile139", Label: "13800000000", Cookie: "tok"}
	installAccount(as, a)
	installAccount(as, a)
	if n := len(as.List()); n != 1 {
		t.Fatalf("重复导入产生了 %d 条", n)
	}
}
