package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 真机测试：需要 DOUBAO_CK 环境变量（整段 Cookie）。
// 默认跳过，不影响 CI。
func TestDoubaoLive(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	cli := newDoubao(ck)
	root, kids, err := cli.dbHome()
	if err != nil {
		t.Fatalf("列根目录失败：%v", err)
	}
	t.Logf("root_id=%s，顶层 %d 项", root, len(kids))

	// 出口池应该是空的（成功请求会把 IP 清出去）
	dbExitMu.Lock()
	n := len(dbExits)
	dbExitMu.Unlock()
	if n != 0 {
		t.Fatalf("成功请求后出口池应为空，实际还有 %d 条", n)
	}

	// 连续 10 次确认 v4 出口没有"打几次就空"的问题
	for i := 0; i < 10; i++ {
		if _, _, err := cli.dbHome(); err != nil {
			t.Fatalf("第 %d 次失败：%v", i+1, err)
		}
	}
	t.Log("连打 10 次全部正常")
}

// 把本机所有出口 IP 强制冷却，验证会返回「风控中」而不是空目录。
func TestDoubaoAllExitsCooling(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()
	defer func() {
		dbExitMu.Lock()
		dbExits = map[string]*dbExitState{}
		dbExitMu.Unlock()
	}()

	cli := newDoubao(ck)
	// 先成功一次，让它记住这台机器有哪些出口 IP（并把连接池填上，
	// 这样才能真的验证「空闲连接不会绕过冷却」）
	if _, _, err := cli.dbHome(); err != nil {
		t.Fatalf("预热请求失败：%v", err)
	}
	cli.ipMu.Lock()
	ips := append([]string(nil), cli.ips...)
	cli.ipMu.Unlock()
	if len(ips) == 0 {
		t.Skip("没解析到出口 IP")
	}
	t.Logf("本机出口：%v", ips)

	now := time.Now()
	dbExitMu.Lock()
	for _, ip := range ips {
		dbExits[ip] = &dbExitState{downTo: now.Add(3 * time.Minute), fails: 1}
	}
	dbExitMu.Unlock()

	_, _, err := cli.dbHome()
	if err == nil {
		t.Fatal("全出口冷却时不该请求成功（空闲连接绕过了冷却）")
	}
	if !isThrottleErr(err) {
		t.Fatalf("应返回风控错误，实际 %T：%v", err, err)
	}
	if !strings.Contains(err.Error(), "风控") {
		t.Fatalf("提示里应有「风控」二字，实际：%v", err)
	}
	t.Logf("正确拒绝：%v", err)
}

// 端到端：走真实的 ListShareFiles，确认
//
//	① 标题用目录名（不是「豆包文件」）
//	② ShareID 是 node:<id>（多个目录不会互相覆盖）
//	③ 同一个目录导入第二次能识别成「已存在」
func TestDoubaoOwnImportNamingLive(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()
	old := globalAccounts
	defer func() { globalAccounts = old; resetDoubaoClients() }()

	as, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	as.Put(Account{Provider: "doubao1", Cookie: ck})
	globalAccounts = as

	cli := newDoubao(ck)
	_, kids, err := cli.dbHome()
	if err != nil {
		t.Fatal(err)
	}
	var target dbNode
	for _, k := range kids {
		if k.IsDir() && k.Name == "软件脚本" {
			target = k
		}
	}
	if target.ID == "" {
		t.Skip("没有「软件脚本」目录")
	}
	link := "doubao://doubao1/" + target.ID + "/" + url.PathEscape(target.Name)
	p := doubaoProvider{slot: 1}
	info, err := p.ListShareFiles(link)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	t.Logf("title=%q shareID=%q 文件=%d", info.Title, info.ShareID, len(info.Files))
	if info.Title != "软件脚本" {
		t.Fatalf("标题应为目录名「软件脚本」，实际 %q", info.Title)
	}
	if info.ShareID != doubaoNodeIDPrefix+target.ID {
		t.Fatalf("ShareID 应为 node:%s，实际 %q", target.ID, info.ShareID)
	}
	// 不带名字的老链接仍能导入，只是标题会退到通用名
	oldLink := "doubao://doubao1/" + target.ID
	info2, err := p.ListShareFiles(oldLink)
	if err != nil {
		t.Fatalf("老格式链接导入失败：%v", err)
	}
	if info2.ShareID != info.ShareID {
		t.Fatalf("同一个目录的 ShareID 应相同：%q vs %q", info.ShareID, info2.ShareID)
	}
}
