package main

// ---------------------------------------------------------------------------
// 分类支持两级：「父/子」（如 电视剧/国产剧）。
//
// 存储上仍是一个字符串（Item.Category / ShareMeta.Category），带不带斜杠
// 就是一级还是两级；目录拼接触发两层：网盘/电视剧/国产剧/剧名。
// 一级分类（"国产剧"）是老形态，继续有效——两级是增量，不是替换。
// ---------------------------------------------------------------------------

import (
	"fmt"
	"strings"
)

// normalizeCategory 规范化分类字符串：中英文斜杠都当分隔符，最多两级，
// 每段过 safeFilename 净化（防 : * ? 等），空段忽略（"电视剧/" 合法）。
// 返回规范化后的分类（"" = 不分类）。超过两级或段名无效返回 error。
func normalizeCategory(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// 中文斜杠／和反斜杠统一按分隔符处理，用户怎么顺手怎么输
	s = strings.NewReplacer("／", "/", "\\", "/").Replace(s)
	segs := make([]string, 0, 2)
	for _, seg := range strings.Split(s, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		segs = append(segs, seg)
		if len(segs) > 2 {
			return "", fmt.Errorf("分类最多两级，用「父/子」格式，如：电视剧/国产剧")
		}
	}
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		clean := safeFilename(seg)
		if clean == "未命名" { // safeFilename 对空段返回「未命名」，这里段不该为空
			return "", fmt.Errorf("分类名无效：%q", seg)
		}
		out = append(out, clean)
	}
	return strings.Join(out, "/"), nil
}

// splitCategory 把分类拆成一级、二级。二级为空表示这是一级分类。
func splitCategory(cat string) (parent, child string) {
	cat = strings.TrimSpace(cat)
	if i := strings.Index(cat, "/"); i >= 0 {
		return strings.TrimSpace(cat[:i]), strings.TrimSpace(cat[i+1:])
	}
	return cat, ""
}

// joinCategory 把一级、二级拼回存储形态。一级为空 = 不分类。
func joinCategory(parent, child string) string {
	parent, child = strings.TrimSpace(parent), strings.TrimSpace(child)
	if parent == "" {
		return ""
	}
	if child == "" {
		return parent
	}
	return parent + "/" + child
}
