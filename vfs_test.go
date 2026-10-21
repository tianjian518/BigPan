package main

import "testing"

// TestSafeFilenameTraversal 防路径穿越：整名为 . / .. 时必须被改写。
func TestSafeFilenameTraversal(t *testing.T) {
	cases := map[string]string{
		"..":        "_..",
		".":         "_.",
		"a..b":      "a..b",   // 中间含点，正常
		"../../etc": ".._.._etc", // 斜杠先被替换，不会穿越
		"":          "未命名",
		"  正常剧名 ":  "正常剧名",
	}
	for in, want := range cases {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
