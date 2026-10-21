package main

import "testing"

// shareURL 反构的分享链接必须满足两条铁律：
//  1. 与各家 parseXXXLink 接受的输入格式对应——粘回来还能解析；
//  2. 提取码进 URL 前必须 QueryEscape——含特殊字符时链接不烂。
func TestShareURL(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		shareID  string
		passcode string
		want     string
	}{
		{"夸克无码", "quark", "abc123DEF", "", "https://pan.quark.cn/s/abc123DEF"},
		{"夸克带码", "quark", "abc123DEF", "a1b2", "https://pan.quark.cn/s/abc123DEF?pwd=a1b2"},
		{"115无码", "115", "sw1234ab", "", "https://115.com/s/sw1234ab"},
		{"115带码", "115", "sw1234ab", "ab12", "https://115.com/s/sw1234ab?password=ab12"},
		{"百度带前导1", "baidu", "1aBcDeFgHiJkL", "", "https://pan.baidu.com/s/1aBcDeFgHiJkL"},
		{"百度带码", "baidu", "1aBcDeFgHiJkL", "x3y4", "https://pan.baidu.com/s/1aBcDeFgHiJkL?pwd=x3y4"},
		{"天翼短链", "tianyi", "QwErTy1234", "", "https://cloud.189.cn/t/QwErTy1234"},
		{"天翼访问码无参数位", "tianyi", "QwErTy1234", "9z8y", "https://cloud.189.cn/t/QwErTy1234"},
		{"移动云盘无码", "mobile139", "i2abc-3def_4", "", "https://yun.139.com/shareweb/#/w/i/i2abc-3def_4"},
		{"移动云盘带码", "mobile139", "i2abc-3def_4", "p0q1", "https://yun.139.com/shareweb/#/w/i/i2abc-3def_4?pwd=p0q1"},
		{"空分享ID", "quark", "", "a1b2", ""},
		{"未知网盘", "onedrive", "xyz", "", ""},
		{"提取码特殊字符转义", "quark", "abc123", "a&b=c", "https://pan.quark.cn/s/abc123?pwd=a%26b%3Dc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shareURL(c.provider, c.shareID, c.passcode); got != c.want {
				t.Errorf("shareURL(%q,%q,%q) = %q, want %q", c.provider, c.shareID, c.passcode, got, c.want)
			}
		})
	}
}
