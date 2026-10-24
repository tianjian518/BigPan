package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 新链路真机验证：解密 keypair → 列目录 → 取下载直链 → 直链可用。
// 需要 DOUBAO_CK；默认跳过。
func TestDoubaoV2Live(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	dbExitMu.Lock()
	dbExits = map[string]*dbExitState{}
	dbExitMu.Unlock()

	cli := newDoubao(ck)
	if cli.v2Key == nil {
		t.Fatal("keypair 解密失败，新链路没就绪")
	}
	t.Log("① DPoP 私钥解密 OK")

	// 列根目录
	_, root, err := cli.dbHome()
	if err != nil {
		t.Fatalf("② 列根目录失败：%v", err)
	}
	if len(root) == 0 {
		t.Fatal("根目录为空")
	}
	t.Logf("② 根目录 %d 项（新链路 feishu API）", len(root))

	// 递归进第一个子目录
	var dir *dbNode
	for i := range root {
		if root[i].IsDir() && root[i].Name == "天剑豆包影视" {
			dir = &root[i]
			break
		}
	}
	if dir == nil {
		for i := range root {
			if root[i].IsDir() && root[i].Name != "Via" {
				dir = &root[i]
				break
			}
		}
	}
	if dir == nil {
		t.Skip("没有可用子目录")
	}
	kids, err := cli.dbListDirAll(dir.ID)
	if err != nil {
		t.Fatalf("③ 子目录递归失败：%v", err)
	}
	t.Logf("③ 「%s」递归 %d 项", dir.Name, len(kids))

	// 优先走「天剑豆包影视 → 动漫剧集 → 某剧 → 视频」这条已知路径
	var file *dbNode
	pick := func(list []dbNode) *dbNode {
		for i := range list {
			if !list[i].IsDir() && isVideo(list[i].Name) && list[i].Key != "" {
				return &list[i]
			}
		}
		return nil
	}
	if dir.Name == "天剑豆包影视" {
		for _, k := range kids {
			if k.IsDir() && k.Name == "动漫剧集" {
				shows, err := cli.dbListDirAll(k.ID)
				if err != nil {
					t.Fatalf("动漫剧集失败：%v", err)
				}
				t.Logf("③b 动漫剧集 %d 部", len(shows))
				for _, show := range shows {
					if !show.IsDir() {
						continue
					}
					time.Sleep(400 * time.Millisecond)
					eps, err := cli.dbListDirAll(show.ID)
					if err != nil {
						continue
					}
					if f := pick(eps); f != nil {
						file = f
						break
					}
				}
			}
		}
	}
	if file == nil {
		if f := pick(kids); f != nil {
			file = f
		}
	}
	if file == nil {
		for i := range kids {
			if kids[i].IsDir() {
				time.Sleep(400 * time.Millisecond)
				sub, err := cli.dbListDirAll(kids[i].ID)
				if err != nil {
					continue
				}
				if f := pick(sub); f != nil {
					file = f
					break
				}
			}
		}
	}
	if file == nil {
		t.Skip("没找到视频文件")
	}
	t.Logf("④ 视频：%s（%d 字节）", file.Name, file.Size)

	// ⑤ 播放路由：新条目 ResolvePlay 应返回 /dbplay 本机转发地址
	old := globalAccounts
	defer func() { globalAccounts = old }()
	as, err := makeTestAccounts(ck)
	if err != nil {
		t.Fatal(err)
	}
	globalAccounts = as
	p := doubaoProvider{slot: 1}
	it := &Item{Provider: "doubao1", FileID: file.ID, FidToken: file.Key, Show: file.Name, Title: file.Name}
	pt, err := p.ResolvePlay(it)
	if err != nil {
		t.Fatalf("⑤ ResolvePlay 失败：%v", err)
	}
	if !strings.HasPrefix(pt.URL, "/dbplay/") {
		t.Fatalf("⑤ 新条目应返回 /dbplay 地址，实际 %q", pt.URL)
	}
	t.Logf("⑤ 播放地址：%s", pt.URL)
	t.Log("⑥（/dbplay 转发的 HTTP 层验证在部署后用 curl 做）")
}

func makeTestAccounts(ck string) (*AccountStore, error) {
	as, err := NewAccountStore("/tmp/dbv2-test-accounts.json")
	if err != nil {
		return nil, err
	}
	as.Put(Account{Provider: "doubao1", Cookie: ck})
	return as, nil
}
