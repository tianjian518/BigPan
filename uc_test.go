package main

import "testing"

// UC 分享链接解析：域名变体 / 提取码 / 误判。
func TestParseUCLink(t *testing.T) {
	cases := []struct {
		in, id, pwd string
		ok          bool
	}{
		{"https://drive.uc.cn/s/752ceb102b314", "752ceb102b314", "", true},
		{"https://drive.uc.cn/s/752ceb102b314?public=1", "752ceb102b314", "", true},
		{"https://drive.uc.cn/s/01933eb190984?pwd=ab12", "01933eb190984", "ab12", true},
		{"https://fast.uc.cn/s/abc123 提取码：xy99", "abc123", "xy99", true},
		{"https://pan.quark.cn/s/xxxx", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		id, pwd, err := parseUCLink(c.in)
		if c.ok {
			if err != nil || id != c.id {
				t.Fatalf("%q → id=%q err=%v（期望 %q）", c.in, id, err, c.id)
			}
			if pwd != c.pwd {
				t.Fatalf("%q → pwd=%q（期望 %q）", c.in, pwd, c.pwd)
			}
		} else if err == nil {
			t.Fatalf("%q 应当解析失败", c.in)
		}
	}
}

func TestUCProviderRegistered(t *testing.T) {
	p := providerByName("uc")
	if p == nil {
		t.Fatal("uc provider 未注册")
	}
	if !p.MatchLink("https://drive.uc.cn/s/752ceb102b314") {
		t.Fatal("MatchLink 应识别 UC 分享")
	}
	if p.MatchLink("https://pan.quark.cn/s/xxxx") {
		t.Fatal("MatchLink 不应误判夸克")
	}
	if !p.Wants302() {
		t.Fatal("UC 应走 302")
	}
	if providerCN("uc") != "UC" {
		t.Fatalf("providerCN=%q", providerCN("uc"))
	}
}

func TestUCShareURL(t *testing.T) {
	u := shareURL("uc", "752ceb102b314", "")
	if u != "https://drive.uc.cn/s/752ceb102b314" {
		t.Fatalf("shareURL=%q", u)
	}
}
