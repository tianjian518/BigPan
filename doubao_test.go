package main

import (
	"net/url"
	"testing"
)

// 豆包分享链接解析：老格式可用、新格式明确拒绝。
func TestParseDoubaoLink(t *testing.T) {
	ok := []struct{ in, id string }{
		{"https://www.doubao.com/drive/s/3185df556eea23ef", "3185df556eea23ef"},
		{"https://www.doubao.com/drive/s/012c3d6a1bc03aa9?foo=1", "012c3d6a1bc03aa9"},
		{"doubao.com/drive/s/a8c78f219d8d9e90", "a8c78f219d8d9e90"},
	}
	for _, c := range ok {
		id, err := parseDoubaoLink(c.in)
		if err != nil || id != c.id {
			t.Fatalf("%q → %q, %v", c.in, id, err)
		}
	}
	// 新格式（/drive/shr/）解析为 shr: 前缀，交给 v2 链路
	if sid, err := parseDoubaoLink("https://www.doubao.com/drive/shr/UE3vfZo2Tl81AvdaG5Nc269onrg"); err != nil || sid != "shr:UE3vfZo2Tl81AvdaG5Nc269onrg" {
		t.Fatalf("新格式应解析为 shr: 前缀，实际 %q err=%v", sid, err)
	}
	if _, err := parseDoubaoLink("https://pan.quark.cn/s/xxxx"); err == nil {
		t.Fatal("夸克链接不该被当成豆包")
	}
}

// 伪链接解析：doubao://<槽位>/<node_id>
func TestParseDoubaoOwnLink(t *testing.T) {
	pv, id, name, ok := parseDoubaoOwnLink("doubao://doubao3/22359807859609602")
	if !ok || pv != "doubao3" || id != "22359807859609602" || name != "" {
		t.Fatalf("got %q %q %q %v", pv, id, name, ok)
	}
	// 带目录名（浏览面板生成的新格式）：名字要能解回来，含斜杠/空格也不许丢
	pv, id, name, ok = parseDoubaoOwnLink("doubao://doubao1/1234567/" + url.PathEscape("动漫 剧集/第一季"))
	if !ok || pv != "doubao1" || id != "1234567" || name != "动漫 剧集/第一季" {
		t.Fatalf("带名字的伪链接解析错了：%q %q %q %v", pv, id, name, ok)
	}
	if _, _, _, ok := parseDoubaoOwnLink("https://www.doubao.com/drive/s/abc"); ok {
		t.Fatal("普通链接不该被当成伪链接")
	}
	if _, _, _, ok := parseDoubaoOwnLink("doubao://doubao1"); ok {
		t.Fatal("缺 node_id 应当不识别")
	}
}

// 多账号隔离：每个槽位只认自己的伪链接，但都认普通分享链接。
func TestDoubaoProviderSlots(t *testing.T) {
	for i := 1; i <= doubaoSlots; i++ {
		p, ok := providerByName("doubao" + string(rune('0'+i))).(doubaoProvider)
		if !ok {
			t.Fatalf("doubao%d 未注册", i)
		}
		if !p.MatchLink("https://www.doubao.com/drive/s/3185df556eea23ef") {
			t.Fatalf("doubao%d 应识别分享链接", i)
		}
		own := "doubao://doubao3/123"
		want := i == 3
		if p.MatchLink(own) != want {
			t.Fatalf("doubao%d 对 doubao3 伪链接的判定错误（期望 %v）", i, want)
		}
		if !p.Wants302() {
			t.Fatalf("doubao%d 应当走 302", i)
		}
		if providerCN("doubao"+string(rune('0'+i))) == "" {
			t.Fatalf("doubao%d 没有中文名", i)
		}
	}
}

func TestDoubaoPickURL(t *testing.T) {
	var list []dbMedia
	mk := func(w, u string) dbMedia {
		var m dbMedia
		m.Meta.Width = w
		m.MainURL = u
		return m
	}
	list = append(list, mk("640", "u360"), mk("1920", "u1080"), mk("960", "u540"))
	if got := dbPickURL(list); got != "u1080" {
		t.Fatalf("应选最宽档位，得到 %q", got)
	}
	if dbPickURL(nil) != "" {
		t.Fatal("空列表应返回空串")
	}
}
