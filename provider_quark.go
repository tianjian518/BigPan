package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 夸克网盘
//
// 实测得到的三个必要条件（缺任何一个都拿不到流）：
//   ① 客户端 UA —— 用 Chrome UA 会被 "download file size limit" 拒绝
//   ② HTTP/2   —— 用 HTTP/1.1 一律 412 Precondition Failed
//   ③ 保持会话 —— 夸克每次响应会轮换 __puus cookie，必须沿用
// ---------------------------------------------------------------------------

const (
	uaQuark      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.12 Safari/537.36"
	quarkAPIBase = "https://drive-pc.quark.cn/1/clouddrive"
	quarkReferer = "https://pan.quark.cn/"
	quarkRootFID = "0"
)

// QuarkFile 夸克网盘里的一个文件或目录。
type QuarkFile struct {
	FID   string `json:"fid"`
	Name  string `json:"file_name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"dir"`
	Pdir  string `json:"pdir_fid"`
}

// setJarCookies 把 "k=v; k2=v2" 写入 cookie jar（作用于 .quark.cn 全域，
// 这样 API 域和下载 CDN 域都能自动带上）。
func setJarCookies(jar http.CookieJar, raw string) {
	u, _ := url.Parse("https://drive-pc.quark.cn/")
	var cks []*http.Cookie
	for _, kv := range strings.Split(raw, ";") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := strings.Index(kv, "=")
		if i <= 0 {
			continue
		}
		cks = append(cks, &http.Cookie{
			Name: kv[:i], Value: kv[i+1:], Domain: ".quark.cn", Path: "/",
		})
	}
	jar.SetCookies(u, cks)
}

// newHTTP2Client 创建一个独立的、强制走 HTTP/2 的客户端。
// 每次调用都会产生独立的连接池 —— 这是"真多连接"的前提。
//
// 注意：必须用 ForceAttemptHTTP2，而不是 http2.ConfigureTransport。
// 后者在显式设置了 TLSClientConfig 时会失效，导致实际走 HTTP/1.1，
// 而夸克 CDN 对 HTTP/1.1 会严重限速甚至返回 412。
func newHTTP2Client(ua, cookie string) *http.Client {
	jar, _ := cookiejar.New(nil)
	if cookie != "" {
		setJarCookies(jar, cookie)
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   true, // 关键：走 HTTP/2
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &http.Client{Transport: tr, Jar: jar}
}

// Quark 夸克网盘客户端（用于 API 调用，单连接、保持会话）。
type Quark struct {
	mu     sync.Mutex
	cookie string
	hc     *http.Client
}

func newQuark(cookie string) *Quark {
	return &Quark{cookie: cookie, hc: newHTTP2Client(uaQuark, cookie)}
}

// Cookies 返回当前会话的最新 cookie（夸克会轮换 __puus，取用时要用最新的）。
func (q *Quark) Cookies() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	u, _ := url.Parse("https://drive-pc.quark.cn/")
	cks := q.hc.Jar.Cookies(u)
	var b strings.Builder
	for _, c := range cks {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

// api 调用夸克接口。
func (q *Quark) api(method, path string, params map[string]string, body interface{}, out interface{}) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	qv := url.Values{}
	qv.Set("pr", "ucpro")
	qv.Set("fr", "pc")
	for k, v := range params {
		qv.Set(k, v)
	}

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, quarkAPIBase+path+"?"+qv.Encode(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", uaQuark)
	req.Header.Set("Referer", quarkReferer)
	req.Header.Set("Origin", "https://pan.quark.cn")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := q.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接夸克失败：%w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("夸克返回了无法识别的数据（HTTP %d）", resp.StatusCode)
		}
	}
	return nil
}

// Check 验证登录态并返回会员标识。
func (q *Quark) Check() (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			MemberType string `json:"member_type"`
		} `json:"data"`
	}
	if err := q.api(http.MethodGet, "/member", nil, nil, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		msg := out.Message
		if msg == "" {
			msg = "登录态无效"
		}
		return "", fmt.Errorf("夸克：%s（code %d）", msg, out.Code)
	}
	return out.Data.MemberType, nil
}

// ListDir 列出某个目录下的内容。
func (q *Quark) ListDir(pdir string) ([]QuarkFile, error) {
	if pdir == "" {
		pdir = quarkRootFID
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []QuarkFile `json:"list"`
		} `json:"data"`
	}
	err := q.api(http.MethodGet, "/file/sort", map[string]string{
		"pdir_fid": pdir,
		"_page":    "1",
		"_size":    "200",
		"_sort":    "file_type:asc,updated_at:desc",
	}, nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("夸克：%s（code %d）", out.Message, out.Code)
	}
	return out.Data.List, nil
}

// DownloadURL 取文件的下载直链。
// 注意：必须用客户端 UA，否则会被 "download file size limit" 拒绝。
func (q *Quark) DownloadURL(fid string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			DownloadURL string `json:"download_url"`
			FileName    string `json:"file_name"`
			Size        int64  `json:"size"`
		} `json:"data"`
	}
	err := q.api(http.MethodPost, "/file/download", nil,
		map[string]interface{}{"fids": []string{fid}}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("夸克：%s（code %d）", out.Message, out.Code)
	}
	if len(out.Data) == 0 || out.Data[0].DownloadURL == "" {
		return "", fmt.Errorf("夸克未返回下载地址")
	}
	return out.Data[0].DownloadURL, nil
}

// ResolvePath 按路径逐级查找文件（形如 "夸克影视/华语电影"），返回最后一级的 fid。
func (q *Quark) ResolvePath(parts []string) (string, error) {
	cur := quarkRootFID
	for _, name := range parts {
		list, err := q.ListDir(cur)
		if err != nil {
			return "", err
		}
		found := ""
		for _, f := range list {
			if f.Name == name {
				found = f.FID
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("在网盘里找不到「%s」", name)
		}
		cur = found
	}
	return cur, nil
}

// ---------------------------------------------------------------------------
// 全局夸克客户端（带缓存，cookie 变化时重建）
// ---------------------------------------------------------------------------

var (
	quarkMu    sync.Mutex
	quarkCur   *Quark
	quarkCurCk string
)

// quarkClient 返回当前配置的夸克客户端；未配置返回 nil。
func quarkClient(as *AccountStore) *Quark {
	if as == nil {
		return nil
	}
	acc := as.Get("quark")
	if acc == nil || acc.Cookie == "" {
		return nil
	}
	quarkMu.Lock()
	defer quarkMu.Unlock()
	if quarkCur != nil && quarkCurCk == acc.Cookie {
		return quarkCur
	}
	quarkCur = newQuark(acc.Cookie)
	quarkCurCk = acc.Cookie
	return quarkCur
}

// ---------------------------------------------------------------------------
// 夸克分享（虚拟网盘：不转存，只记录"文件在哪里"）
// ---------------------------------------------------------------------------

// ShareToken 换取分享的临时令牌（游客即可，不需要账号）。
func (q *Quark) ShareToken(pwdID, passcode string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Stoken string `json:"stoken"`
			Title  string `json:"title"`
		} `json:"data"`
	}
	err := q.api(http.MethodPost, "/share/sharepage/token", nil,
		map[string]interface{}{"pwd_id": pwdID, "passcode": passcode}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.Stoken == "" {
		msg := out.Message
		if msg == "" {
			msg = "分享可能已失效或提取码不正确"
		}
		return "", fmt.Errorf("夸克分享：%s", msg)
	}
	return out.Data.Stoken, nil
}

// ShareTitle 只取分享标题（用于展示）。
func (q *Quark) ShareTitle(pwdID, passcode string) string {
	var out struct {
		Code int `json:"code"`
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := q.api(http.MethodPost, "/share/sharepage/token", nil,
		map[string]interface{}{"pwd_id": pwdID, "passcode": passcode}, &out); err != nil {
		return ""
	}
	return out.Data.Title
}

// ShareList 列出分享中某个目录下的内容。
func (q *Quark) ShareList(pwdID, stoken, pdirFID string) ([]QuarkFile, error) {
	if pdirFID == "" {
		pdirFID = quarkRootFID
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []QuarkFile `json:"list"`
		} `json:"data"`
	}
	err := q.api(http.MethodGet, "/share/sharepage/detail", map[string]string{
		"pwd_id":        pwdID,
		"stoken":        stoken,
		"pdir_fid":      pdirFID,
		"force":         "0",
		"_page":         "1",
		"_size":         "200",
		"_fetch_banner": "0",
		"_fetch_share":  "0",
		"_fetch_total":  "1",
		"_sort":         "file_type:asc,updated_at:desc",
	}, nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("夸克分享：%s（code %d）", out.Message, out.Code)
	}
	return out.Data.List, nil
}

// ShareDownloadURL 取分享文件的直链 —— 关键：不转存，直接取。
// 这是"虚拟网盘"的核心：文件始终在别人网盘里，一个字节都不占自己空间。
func (q *Quark) ShareDownloadURL(pwdID, stoken, fid string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			DownloadURL string `json:"download_url"`
			FileName    string `json:"file_name"`
			Size        int64  `json:"size"`
		} `json:"data"`
	}
	err := q.api(http.MethodPost, "/file/download", nil,
		map[string]interface{}{"fids": []string{fid}, "pwd_id": pwdID, "stoken": stoken}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("夸克：%s（code %d）", out.Message, out.Code)
	}
	if len(out.Data) == 0 || out.Data[0].DownloadURL == "" {
		return "", fmt.Errorf("夸克未返回下载地址（分享可能已失效）")
	}
	return out.Data[0].DownloadURL, nil
}
