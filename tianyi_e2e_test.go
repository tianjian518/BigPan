package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	tclient "bigpan/internal/tianyi/client"
)

// TestTianyiE2E 对真实分享链接做端到端扫描（联网测试，默认跳过）。
// 用法：
//
//	T189_LINKS='https://cloud.189.cn/t/xxxx（访问码：abcd）|https://...' go test -run TestTianyiE2E -v
func TestTianyiE2E(t *testing.T) {
	raw := os.Getenv("T189_LINKS")
	if raw == "" {
		t.Skip("未设置 T189_LINKS，跳过联网测试")
	}
	for _, link := range strings.Split(raw, "|") {
		link = strings.TrimSpace(link)
		if link == "" {
			continue
		}
		code, pwd, err := parseTianyiLink(link)
		if err != nil {
			t.Errorf("parse %q: %v", link, err)
			continue
		}
		start := time.Now()
		title, files, err := scanTianyiShare(code, pwd)
		if err != nil {
			t.Errorf("scan %s pwd=%s: %v", code, pwd, err)
			continue
		}
		fmt.Printf("\n[OK] %s  code=%s pwd=%s  %.1fs  视频 %d 个\n", title, code, pwd, time.Since(start).Seconds(), len(files))
		for i, f := range files {
			if i >= 3 {
				break
			}
			fmt.Printf("  - %s (%.2f GB) id=%s shareNumID=%s\n", f.Path, float64(f.Size)/1073741824, f.ID, f.ShareNumID)
		}
	}
}

// TestTianyiSub 实测订阅号：验证 userId + 拉首页分享列表（联网测试，默认跳过）。
// 用法：T189_UP=56673222a02292e570a02e226f5d256f go test -run TestTianyiSub -v
func TestTianyiSub(t *testing.T) {
	up := os.Getenv("T189_UP")
	if up == "" {
		t.Skip("未设置 T189_UP，跳过联网测试")
	}
	name, err := tianyiSubUser(nil, up)
	if err != nil {
		t.Errorf("订阅号校验失败: %v", err)
		return
	}
	fmt.Printf("\n[订阅号] %s (%s)\n", name, up)

	list, total, err := tianyiUpShares(nil, up, 1, 20)
	if err != nil {
		t.Errorf("拉取分享失败: %v", err)
		return
	}
	fmt.Printf("分享总数 %d，首页 %d 条\n", total, len(list))
	for i, s := range list {
		if i >= 5 {
			break
		}
		fmt.Printf("  - %s  shareId=%d fileId=%s folder=%v size=%.2fGB code=%s date=%s\n",
			s.Name, s.ShareId, s.RootFileId, s.IsFolder, float64(s.Size)/1073741824, s.ShareCode, s.ShareDate)
	}
}

// TestTianyiPlayURL 用真实 token 取一条直链并打头，确认取流链路（联网，默认跳过）。
// 用法：T189_TOKEN=xxx go test -run TestTianyiPlayURL -v
func TestTianyiPlayURL(t *testing.T) {
	tok := os.Getenv("T189_TOKEN")
	if tok == "" {
		t.Skip("未设置 T189_TOKEN，跳过联网测试")
	}
	cred := &tianyiCred{AccessToken: tok, ExpiresMs: time.Now().Add(time.Hour).UnixMilli()}
	shareID := int64(12325116478799) // 成龙合集
	fileID := "12355365144714580"    // 新警察故事
	if v := os.Getenv("T189_FILE"); v != "" {
		fileID = v
	}
	if v := os.Getenv("T189_SHARE"); v != "" {
		fmt.Sscanf(v, "%d", &shareID)
	}
	u, err := tianyiDownloadURL(cred, shareID, fileID)
	if err != nil {
		t.Fatalf("取直链失败: %v", err)
	}
	fmt.Printf("\nURL=%s\n", u)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求直链失败: %v", err)
	}
	defer resp.Body.Close()
	fmt.Printf("[直链响应] HTTP %d  Content-Type=%q  Accept-Ranges=%q  Content-Range=%q\n",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Accept-Ranges"), resp.Header.Get("Content-Range"))
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	fmt.Printf("[前 64 字节] %x\n", buf[:n])
}

// TestTianyiVLC 测天翼转码流（m3u8）备用通道是否可用。
func TestTianyiVLC(t *testing.T) {
	tok := os.Getenv("T189_TOKEN")
	if tok == "" {
		t.Skip("未设置 T189_TOKEN，跳过联网测试")
	}
	cred := &tianyiCred{AccessToken: tok, ExpiresMs: time.Now().Add(time.Hour).UnixMilli()}
	u, err := tianyiVLCPlayURL(cred, 12325116478799, "12355365144714580")
	if err != nil {
		t.Fatalf("转码流取址失败: %v", err)
	}
	fmt.Printf("\n[转码流] %s\n", u)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(u)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 200)
	n, _ := resp.Body.Read(buf)
	fmt.Printf("[转码流响应] HTTP %d CT=%q\n%s\n", resp.StatusCode, resp.Header.Get("Content-Type"), string(buf[:n]))
}

// TestTianyiSubPlay 订阅号分享的取流链路：列一层目录 → 取第一条视频的直链。
func TestTianyiSubPlay(t *testing.T) {
	tok := os.Getenv("T189_TOKEN")
	if tok == "" {
		t.Skip("未设置 T189_TOKEN，跳过联网测试")
	}
	cred := &tianyiCred{AccessToken: tok, ExpiresMs: time.Now().Add(time.Hour).UnixMilli()}
	const shareID = int64(12345118775311) // 豆瓣电影 Top250
	const rootID = "523431222274000935"
	list, err := tianyiListShareDir(cred, shareID, rootID, "", 5, 1)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	fmt.Printf("\n[目录] %d 项\n", len(list))
	var vid *tianyiDirEntry
	for i := range list {
		if !list[i].IsDir && isVideo(list[i].Name) {
			vid = &list[i]
			break
		}
	}
	if vid == nil {
		for i := range list {
			if list[i].IsDir {
				sub, e := tianyiListShareDir(cred, shareID, list[i].ID, "", 5, 1)
				if e != nil {
					t.Fatalf("列子目录失败: %v", e)
				}
				for j := range sub {
					if !sub[j].IsDir && isVideo(sub[j].Name) {
						vid = &sub[j]
						break
					}
				}
				break
			}
		}
	}
	if vid == nil {
		t.Skip("没找到视频")
	}
	fmt.Printf("[样本] %s id=%s\n", vid.Name, vid.ID)
	u, err := tianyiDownloadURL(cred, shareID, vid.ID)
	if err != nil {
		t.Fatalf("订阅分享取流失败: %v", err)
	}
	fmt.Printf("[直链] 取得，长度 %d\n", len(u))
}

// TestTianyiRawLink 打印 GetFileDownload 原始地址与首次响应，排查 400。
func TestTianyiRawLink(t *testing.T) {
	tok := os.Getenv("T189_TOKEN")
	if tok == "" {
		t.Skip("no token")
	}
	cred := &tianyiCred{AccessToken: tok, ExpiresMs: time.Now().Add(time.Hour).UnixMilli()}
	fileID := os.Getenv("T189_FILE")
	shareID := int64(12345118775311)
	fmt.Sscanf(os.Getenv("T189_SHARE"), "%d", &shareID)
	raw, e := tianyiRawDownloadURL(cred, shareID, fileID)
	if e != nil {
		t.Fatalf("raw err: %v", e)
	}
	fmt.Printf("\nRAW=%s\n", raw)
	req, _ := http.NewRequest("GET", raw, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/120.0")
	c := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 20 * time.Second}
	r2, e := c.Do(req)
	if e != nil {
		t.Fatalf("get err: %v", e)
	}
	defer r2.Body.Close()
	b := make([]byte, 400)
	n, _ := r2.Body.Read(b)
	fmt.Printf("STATUS=%d\nBODY=%s\n", r2.StatusCode, string(b[:n]))
}

// TestTianyiUAMatters 验证「取 Location 时用什么 UA」会不会影响天翼下发的 CDN 地址。
// 原版 cloudpan189-share 用 Chrome UA，我们用的是 TV UA —— 若结果不同，就是卡顿的真因。
func TestTianyiUAMatters(t *testing.T) {
	tok := os.Getenv("T189_TOKEN")
	if tok == "" {
		t.Skip("未设置 T189_TOKEN")
	}
	cred := &tianyiCred{AccessToken: tok, ExpiresMs: time.Now().Add(time.Hour).UnixMilli()}
	shareID := int64(12452120482890)
	fileID := "123271241370024013"
	raw, err := tianyiRawDownloadURL(cred, shareID, fileID)
	if err != nil {
		t.Fatalf("取原始地址失败: %v", err)
	}
	fmt.Printf("\nRAW host=%s\n", raw[:min(90, len(raw))])

	uas := map[string]string{
		"TV-默认":    tclient.UserAgent,
		"Chrome":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
		"netdisk":  "netdisk;P2SP;2.2.91.136;android-android;",
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	for name, ua := range uas {
		req, _ := http.NewRequest("GET", raw, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%-10s 请求失败: %v\n", name, err)
			continue
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		fmt.Printf("%-10s code=%d loc_host=%s\n", name, resp.StatusCode, hostOf(loc))
		if loc == "" {
			continue
		}
		// 看 CDN 给的 Content-Type
		r2, _ := http.NewRequest("GET", loc, nil)
		r2.Header.Set("Range", "bytes=0-1023")
		r2.Header.Set("User-Agent", ua)
		resp2, err := client.Do(r2)
		if err != nil {
			fmt.Printf("           CDN 请求失败: %v\n", err)
			continue
		}
		fmt.Printf("           CDN code=%d CT=%q\n", resp2.StatusCode, resp2.Header.Get("Content-Type"))
		resp2.Body.Close()
	}
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.Index(rest, "/"); j > 0 {
			return rest[:j]
		}
		return rest
	}
	return u
}
