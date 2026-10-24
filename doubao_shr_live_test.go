package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 真机：用一条真实的三方 /drive/shr/ 分享验证解析链。
// 只列分享根 + 取名字，不做全量扫描（大分享要几十分钟）。
func TestDoubaoShrLive(t *testing.T) {
	ck := strings.TrimSpace(os.Getenv("DOUBAO_CK"))
	if ck == "" {
		t.Skip("未设置 DOUBAO_CK，跳过真机测试")
	}
	link := os.Getenv("DOUBAO_SHR")
	if link == "" {
		link = "https://www.doubao.com/drive/shr/FfItfM7OrltbDjdonW6ccbsWnyd"
	}
	sid, err := parseDoubaoLink(link)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !strings.HasPrefix(sid, doubaoShrPrefix) {
		t.Fatalf("应为 shr: 前缀，实际 %q", sid)
	}
	cli := newDoubao(ck)
	if cli.v2Key == nil {
		t.Fatal("v2 未就绪（缺 keypair）")
	}
	tok := strings.TrimPrefix(sid, doubaoShrPrefix)
	name, err := cli.dbnNodeName(tok)
	if err != nil {
		t.Fatalf("取分享名失败：%v", err)
	}
	t.Logf("分享名：%q", name)
	if name == "" {
		t.Fatal("分享名为空")
	}
	kids, err := cli.dbnToList(tok)
	if err != nil {
		t.Fatalf("列分享根失败：%v", err)
	}
	var names []string
	for i, k := range kids {
		if i >= 8 {
			names = append(names, "...")
			break
		}
		names = append(names, k.Name)
		time.Sleep(300 * time.Millisecond)
	}
	t.Logf("分享根 %d 项：%v", len(kids), names)
	if len(kids) == 0 {
		t.Fatal("分享根为空")
	}
	// ResolvePlay 路由：文件条目应返回 302 直链（feishu stream 域名）
	var file *dbNode
	for i := range kids {
		if !kids[i].IsDir() && kids[i].Key != "" {
			file = &kids[i]
			break
		}
	}
	if file == nil {
		t.Log("（分享根没有散文件，跳过取链验证——下钻逻辑与自盘扫描同一套）")
		return
	}
	u, err := cli.dbnDownloadURL(file.Key)
	if err != nil {
		t.Fatalf("拼直链失败：%v", err)
	}
	if !strings.Contains(u, "feishu.cn") || !strings.Contains(u, "authorization=DPoP+") {
		t.Fatalf("直链格式不对：%s", u[:min(120, len(u))])
	}
	t.Logf("直链 OK：%s…", u[:min(90, len(u))])
}
