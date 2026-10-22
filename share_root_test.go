package main

import "testing"

// 分享自带容器目录的剥离规则（2026-10-22 UC 国漫「规范剧名/乱码原名」事故）。
func TestCommonShareRoot(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"同一乱码容器", []string{"【N】辶屰 -天- 牙阝-示申/a.mp4", "【N】辶屰 -天- 牙阝-示申/b.mp4"}, "【N】辶屰 -天- 牙阝-示申"},
		{"容器+子目录", []string{"剧名 (2026)/第 1 季/a.mkv", "剧名 (2026)/第 1 季/b.mkv"}, "剧名 (2026)"},
		{"多顶层不剥", []string{"A/a.mp4", "B/b.mp4"}, ""},
		{"根层文件不剥", []string{"a.mp4", "B/b.mp4"}, ""},
		{"单文件在容器内", []string{"X/a.mp4"}, "X"},
	}
	for _, c := range cases {
		if got := commonShareRoot(c.paths); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestStripShareRootThenBuildDir(t *testing.T) {
	// 用户起规范名「逆天邪神」，分享首层是乱码 → 不应多一层
	p := stripShareRoot("【N】辶屰 -天- 牙阝-示申/60~[4K].mp4", "【N】辶屰 -天- 牙阝-示申")
	if got := buildItemDir("uc", "国漫", "逆天邪神", p); got != "UC/国漫/逆天邪神" {
		t.Fatalf("乱码容器未剥净: %q", got)
	}
	// 容器下还有真实季目录 → 保留
	p2 := stripShareRoot("剧名 (2026)/第 1 季/a.mkv", "剧名 (2026)")
	if got := buildItemDir("uc", "国漫", "逆天邪神", p2); got != "UC/国漫/逆天邪神/第 1 季" {
		t.Fatalf("子目录应保留: %q", got)
	}
	// 多顶层（合集）→ 不剥，各自保留
	if got := buildItemDir("uc", "国漫", "合集", stripShareRoot("A/a.mp4", "")); got != "UC/国漫/合集/A" {
		t.Fatalf("多顶层应保留: %q", got)
	}
}
