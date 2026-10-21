package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 页面不该再用原生 prompt()：全部输入场景都走设计系统内的 askText 模态框
func TestUINoNativePrompt(t *testing.T) {
	html := renderIndex(nil, nil, "http://127.0.0.1:8080")
	if strings.Contains(html, "prompt(") {
		t.Fatal("页面里还有原生 prompt( 调用，应替换为 askText 模态框")
	}
	for _, marker := range []string{`id="askModal"`, "function askText(", "function closeAsk(", "askYes", "askInput"} {
		if !strings.Contains(html, marker) {
			t.Fatalf("页面缺少 askModal 组件标记: %s", marker)
		}
	}
}

// 页面内嵌 JS 必须语法合法：提取 <script> 块交 node --check（无 node 则跳过）
func TestUIInlineJSSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不可用，跳过 JS 语法校验")
	}
	html := renderIndex(nil, nil, "http://127.0.0.1:8080")
	re := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	ms := re.FindAllStringSubmatch(html, -1)
	if len(ms) == 0 {
		t.Fatal("页面里没找到 <script> 块")
	}
	dir := t.TempDir()
	for i, m := range ms {
		f := filepath.Join(dir, "block"+string(rune('0'+i))+".js")
		if err := os.WriteFile(f, []byte(m[1]), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("node", "--check", f).CombinedOutput()
		if err != nil {
			t.Fatalf("第 %d 个 script 块语法错误: %v\n%s", i+1, err, out)
		}
	}
}
