package main

import (
	"os"
	"strings"
	"testing"
)

// 【2026-10-17 回归】网页媒体库面包屑「只有根目录有效、子级点击无反应」。
// 病根：onclick="goPath('+JSON.stringify(acc)+')" 生成的双引号把 HTML 属性值当场截断，
// 浏览器解析出 onclick="goPath([" 残缺属性 → 点击语法错误无反应；
// 而「根目录」是 goPath([]) 空数组不带引号所以正常。
// 正确做法：escA() 把 " 转成 &quot;、' 转成 &#39;，浏览器解码属性值后 JS 才完整。
func TestBreadcrumbOnclickEscaped(t *testing.T) {
	src, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("读 ui.go 失败: %v", err)
	}
	s := string(src)

	// 1) 不允许再出现未转义的 JSON.stringify 直接拼进 HTML 属性
	if strings.Contains(s, `onclick="goPath('+JSON.stringify(acc)+')`) {
		t.Errorf("发现未转义的面包屑 onclick 拼接：onclick=\"goPath('+JSON.stringify(acc)+')\" —— JSON 双引号会截断 HTML 属性，必须包 escA()")
	}

	// 2) 必须存在转义后的正确形式
	if !strings.Contains(s, `onclick="goPath('+escA(JSON.stringify(acc))+')`) {
		t.Errorf("未找到转义后的面包屑 onclick：onclick=\"goPath('+escA(JSON.stringify(acc))+')\"")
	}

	// 3) escA 本体必须健在（双引号与单引号都要转实体）
	if !strings.Contains(s, `replace(/"/g,'&quot;')`) || !strings.Contains(s, `replace(/'/g,'&#39;')`) {
		t.Errorf("escA() 的引号转实体逻辑缺失或被改坏")
	}

	// 4) 目录行/网格卡片的 data-p 同样必须走 escA（同级风险点一并锁死）
	for _, attr := range []string{`data-p="'+escA(JSON.stringify(p))`, `data-ids="'+escA(JSON.stringify(ids))`} {
		if !strings.Contains(s, attr) {
			t.Errorf("HTML 属性拼接缺少 escA 转义：%s", attr)
		}
	}
}
