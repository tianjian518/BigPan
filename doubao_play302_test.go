package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 造一个带豆包新链路分享条目的 store，并往 provider 的客户端缓存里
// 塞一个「v2 凭证就绪」的 Doubao 客户端。
//
// 关键点：新链取链（dbnDownloadURL）= dbnEnsureToken（token 4 天内有效，
// 直接返回缓存，零网络）+ dbnSignDPoP（本地 ECDSA 签名）——整条路不碰
// 网络，所以测试完全离线、可重复。
func setupDoubaoPlay302Test(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	// file_id 非纯数字 = 新链路（dbIsOldNodeID 判据）；fid_token 即 obj_token
	os.WriteFile(p, []byte(`[
{"id":"d1","title":"第01集","provider":"doubao1","dir":"豆包/影视/甲","share_id":"shrABC","file_id":"nodCNxx","fid_token":"objTOKEN","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	as, err := NewAccountStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	as.Put(Account{Provider: "doubao1", Label: "测试号", Cookie: "test-cookie", LoginAt: time.Now()})
	oldAcc := globalAccounts
	globalAccounts = as
	t.Cleanup(func() { globalAccounts = oldAcc })

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cli := newDoubao("test-cookie")
	cli.v2Key = key
	cli.v2Token = "test-access-token"
	cli.v2TokenAt = time.Now() // 4 天内有效，不会触发刷新
	dbMu.Lock()
	dbClients["doubao1"] = cli
	dbKeys["doubao1"] = "test-cookie"
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		delete(dbClients, "doubao1")
		delete(dbKeys, "doubao1")
		dbMu.Unlock()
	})
	return s
}

func playReq(t *testing.T, s *Store, target string) *httptest.ResponseRecorder {
	t.Helper()
	as, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := handlePlay(s, as)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetBasicAuth(*flagUser, *flagPass) // P1-3 起 /play 要鉴权
	h(rec, req)
	return rec
}

// 豆包 /play 必须 302 直连飞书 relay（§0 铁律 + §23.114 实测：
// 中转 conc=4 每次都暴力同一个 URL，触发 relay 多连接惩罚，卡顿真因）。
// 这条测试钉住「豆包不落中转」——评审时「是不是 302」的硬性检查项。
func TestHandlePlayDoubao302(t *testing.T) {
	s := setupDoubaoPlay302Test(t)
	rec := playReq(t, s, "/play?id=d1")
	if rec.Code != http.StatusFound {
		t.Fatalf("豆包应 302，got %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/space/api/box/stream/download/all/objTOKEN/") {
		t.Fatalf("302 目标不是豆包新链直链：%s", loc)
	}
	if !strings.Contains(loc, "authorization=DPoP+") || !strings.Contains(loc, "dpop=") {
		t.Fatalf("直链缺少 DPoP 鉴权参数：%s", loc)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
		t.Fatalf("302 未带可缓存头：%q", cc)
	}
}

// 每次 /play 都现炒新链（dropPlayTarget + resolveDirectURL）：
// DPoP proof 带随机 jti/nonce，两次请求的 302 目标必须不同。
// 意义：万一播放器开过 2+ 连接触发 relay 惩罚，惩罚锚定 URL——
// 换新链即清零，不让卡顿拖到 TTL 结束（§23.114 实验 6）。
func TestHandlePlayDoubaoFreshPerRequest(t *testing.T) {
	s := setupDoubaoPlay302Test(t)
	rec1 := playReq(t, s, "/play?id=d1")
	rec2 := playReq(t, s, "/play?id=d1")
	if rec1.Code != http.StatusFound || rec2.Code != http.StatusFound {
		t.Fatalf("两次都应 302，got %d / %d", rec1.Code, rec2.Code)
	}
	if loc1, loc2 := rec1.Header().Get("Location"), rec2.Header().Get("Location"); loc1 == loc2 {
		t.Fatal("两次 /play 返回同一条链——没有现炒，惩罚记忆会跨请求存活")
	}
}

// 取链失败（如账号没登录）时宁可 502，也绝不回落本机中转——
// 与天翼/UC 分支同一立场，中转等于拿服务器出口当视频带宽。
func TestHandlePlayDoubaoNoFallbackToRelay(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"d1","title":"第01集","provider":"doubao1","dir":"豆包/影视/甲","share_id":"shrABC","file_id":"nodCNxx","fid_token":"objTOKEN","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	// 空账号库 = 豆包没粘 Cookie，provider.ResolvePlay 必然失败
	oldAcc := globalAccounts
	globalAccounts, _ = NewAccountStore(filepath.Join(dir, "accounts.json"))
	t.Cleanup(func() { globalAccounts = oldAcc })

	rec := playReq(t, s, "/play?id=d1")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("无凭证应 502（不中转），got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "取直链失败") {
		t.Fatalf("错误信息应走 302 分支的话术，got %s", rec.Body.String())
	}
}

// 非豆包条目不受影响：UC 仍走自己的 302 分支（回归保护，
// 证明新分支没有把别人的路堵了）。
func TestHandlePlayUcStill302(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.json")
	os.WriteFile(p, []byte(`[
{"id":"u1","title":"第01集","provider":"uc","dir":"UC/影视/甲","share_id":"s1","file_id":"f1","ext":".mp4","size":1}
]`), 0644)
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	oldAcc := globalAccounts
	globalAccounts, _ = NewAccountStore(filepath.Join(dir, "accounts.json"))
	t.Cleanup(func() { globalAccounts = oldAcc })

	// UC 无凭证同样 502（走 UC 自己的分支），关键是错误信息不同
	rec := playReq(t, s, "/play?id=u1")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("UC 无凭证应 502，got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UC") {
		t.Fatalf("应走 UC 分支，got %s", rec.Body.String())
	}
}
