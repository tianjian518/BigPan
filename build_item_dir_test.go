package main

import "testing"

// 【2026-10-17】buildItemDir 折叠规则升级 + 目录名剥刮削标记（备忘录 §22.7）。
// 背景：用户给网盘原名起规范名后，字符串不相等 → 同名折叠失效 →
// 「规范名/变形名」两层目录；且目录名带的 [tmdbid=xxx]/{tmdb-xxx} 会触发
// 网易爆米花把当前目录渲染成剧集条目（目录套目录）。
// 新规则：第一段与剧名「剥标记后同名」或「同 tmdbid」→ 折叠；目录各段剥标记。
func TestBuildItemDirStripAndFold(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		category string
		show     string
		filePath string
		want     string
	}{
		{
			// 病历原型：规范名「示例剧」+ 网盘变形名「Z==剧」，同 tmdbid → 折成一层
			name: "同tmdbid折叠", provider: "baidu", category: "国产剧",
			show: "示例剧（2026）[tmdbid=279923]", filePath: "Z==剧（2026）[tmdbid=279923]/S01E01.mkv",
			want: "百度/国产剧/示例剧（2026）",
		},
		{
			name: "同tmdbid折叠·大括号格式", provider: "quark", category: "国产剧",
			show: "示例剧二 (2026) [tmdbid=294446]", filePath: "M_剧二（2026）[tmdbid=294446]/S01E01.mkv",
			want: "夸克/国产剧/示例剧二 (2026)",
		},
		{
			// 老场景不回归：剧名与分享目录同名（都不带标记）→ 照旧折叠
			name: "同名折叠", provider: "mobile139", category: "国漫",
			show: "将夜（2026）", filePath: "将夜（2026）/第1集.mp4",
			want: "移动/国漫/将夜（2026）",
		},
		{
			// ExampleShow：show 带 {tmdb-85271}，分享内子目录 Season 1 是真实
			// 子目录（无 tmdbid）→ 不折叠，但目录名的 {tmdb-85271} 要剥掉
			name: "真实子目录不折叠·剥大括号标记", provider: "quark", category: "欧美剧",
			show: "ExampleShow (2021) {tmdb-85271}", filePath: "ExampleShow (2021) {tmdb-85271}/Season 1/S01E01.mkv",
			want: "夸克/欧美剧/ExampleShow (2021)/Season 1",
		},
		{
			// 不同 tmdbid = 不同剧，不折叠
			name: "不同tmdbid不折叠", provider: "quark", category: "国产剧",
			show: "甲剧（2026）[tmdbid=111]", filePath: "乙剧（2026）[tmdbid=222]/S01E01.mkv",
			want: "夸克/国产剧/甲剧（2026）/乙剧（2026）",
		},
		{
			// 分享内没有子目录（平铺）：剧名照旧剥标记
			name: "平铺分享", provider: "baidu", category: "",
			show: "雷霆令[tmdbid=999]", filePath: "S01E01.mkv",
			want: "百度/雷霆令",
		},
		{
			// 分享内多层子目录，第一段同 tmdbid 折掉，后面的真实子目录保留
			name: "折叠后仍保留深层子目录", provider: "baidu", category: "国产剧",
			show: "示例剧（2026）[tmdbid=279923]", filePath: "Z==剧（2026）[tmdbid=279923]/【Z】正片/S01E01.mkv",
			want: "百度/国产剧/示例剧（2026）/【Z】正片",
		},
		{
			// 无分类
			name: "无分类", provider: "quark", category: "",
			show: "某剧[tmdbid=5]", filePath: "某剧[tmdbid=5]/S01E01.mkv",
			want: "夸克/某剧",
		},
	}
	for _, c := range cases {
		got := buildItemDir(c.provider, c.category, c.show, c.filePath)
		if got != c.want {
			t.Errorf("[%s] buildItemDir(%q,%q,%q,%q)\n  got  %q\n  want %q",
				c.name, c.provider, c.category, c.show, c.filePath, got, c.want)
		}
	}
}

func TestStripScrape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"示例剧（2026）[tmdbid=279923]", "示例剧（2026）"},
		{"ExampleShow (2021) {tmdb-85271}", "ExampleShow (2021)"},
		{"某剧[tmdbid=1] {tmdb-2}", "某剧"},                       // 多标记叠加
		{" 示例剧（2026）[tmdbid=279923] ", "示例剧（2026）"},          // 前后空白
		{"将夜（2026）", "将夜（2026）"},                          // 无标记不动
		{"[tmdbid=279923]", ""},                                   // 整个就是标记 → 空
		{"甲[tmdbid=1]乙", "甲[tmdbid=1]乙"},                      // 标记不在尾部 → 不剥
	}
	for _, c := range cases {
		if got := stripScrape(c.in); got != c.want {
			t.Errorf("stripScrape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTmdbidOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"示例剧（2026）[tmdbid=279923]", "279923"},
		{"ExampleShow (2021) {tmdb-85271}", "85271"},
		{"将夜（2026）", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := tmdbidOf(c.in); got != c.want {
			t.Errorf("tmdbidOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
