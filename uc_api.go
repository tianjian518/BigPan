package main

// ---------------------------------------------------------------------------
// UC 网盘 PC 接口（与夸克同源，只有域名与 pr 参数不同）
//
// 实测（2026-10-22）：
//   - 分享解析游客即可（token / detail 全通）；
//   - 分享取链也通，但直链带 callback，CDN 回调要求登录 cookie
//     （RequestDeniedByCallback: require login [auth not found]）；
//   - 因此 PC 接口只负责「转存」，播放取流走 TV 接口（见 uc_save.go）。
// ---------------------------------------------------------------------------

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uaUC        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) UCBrowser/15.0.0.1000 Chrome/100.0.4896.160 Safari/537.36"
	ucReferer   = "https://drive.uc.cn/"
	ucRootFID   = "0"
	ucAPIPrefix = "?pr=UCBrowser&fr=pc&uc_param_str="
)

// ucAPIBase 提成 var：单测可指向 httptest。
var ucAPIBase = "https://pc-api.uc.cn/1/clouddrive"

// UCFile UC 网盘里的一个文件或目录。
type UCFile struct {
	FID      string `json:"fid"`
	Name     string `json:"file_name"`
	Size     int64  `json:"size"`
	Pdir     string `json:"pdir_fid"`
	IsDir    bool   `json:"dir"`
	Category int    `json:"category"`
	Updated  int64  `json:"updated_at"`
	// ShareFidToken 分享文件令牌 —— 转存必带，只在分享目录列表里返回。
	ShareFidToken string `json:"share_fid_token"`
}

// UC UC 网盘的 PC 版客户端（Cookie 登录）。
type UC struct {
	mu     sync.Mutex
	cookie string
	hc     *http.Client
}

func newUC(cookie string) *UC {
	jar, _ := cookiejar.New(nil)
	if cookie != "" {
		u, _ := url.Parse("https://pc-api.uc.cn/")
		var cks []*http.Cookie
		for _, kv := range strings.Split(cookie, ";") {
			kv = strings.TrimSpace(kv)
			i := strings.Index(kv, "=")
			if i <= 0 {
				continue
			}
			cks = append(cks, &http.Cookie{Name: kv[:i], Value: kv[i+1:], Domain: ".uc.cn", Path: "/"})
		}
		jar.SetCookies(u, cks)
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &UC{cookie: cookie, hc: &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second}}
}

// Cookies 返回当前会话 cookie（UC 会轮换 __puus，取用时要用最新的）。
func (u *UC) Cookies() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	pu, _ := url.Parse("https://pc-api.uc.cn/")
	var b strings.Builder
	for _, c := range u.hc.Jar.Cookies(pu) {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

// api 调 UC 接口。path 不带前缀，统一补 pr/fr。
func (u *UC) api(method, path string, params map[string]string, body interface{}, out interface{}) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	qv := url.Values{}
	qv.Set("pr", "UCBrowser")
	qv.Set("fr", "pc")
	qv.Set("uc_param_str", "")
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
	req, err := http.NewRequest(method, ucAPIBase+path+"?"+qv.Encode(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", uaUC)
	req.Header.Set("Referer", ucReferer)
	req.Header.Set("Origin", "https://drive.uc.cn")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 UC 失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("UC 返回了无法识别的数据（HTTP %d）", resp.StatusCode)
		}
	}
	return nil
}

// Check 验证登录态。
func (u *UC) Check() (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			MemberType string `json:"member_type"`
			Nickname   string `json:"nickname"`
		} `json:"data"`
	}
	if err := u.api(http.MethodGet, "/member", nil, nil, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		msg := out.Message
		if msg == "" {
			msg = "登录态无效"
		}
		return "", fmt.Errorf("UC：%s（code %d）", msg, out.Code)
	}
	return out.Data.MemberType, nil
}

// ShareToken 换分享临时令牌（游客即可，不需要账号）。
func (u *UC) ShareToken(pwdID, passcode string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Stoken string `json:"stoken"`
		} `json:"data"`
	}
	err := u.api(http.MethodPost, "/share/sharepage/token",
		nil, map[string]string{"pwd_id": pwdID, "passcode": passcode}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.Stoken == "" {
		msg := out.Message
		if msg == "" {
			msg = "分享可能已失效或提取码不正确"
		}
		return "", fmt.Errorf("UC 分享：%s", msg)
	}
	return out.Data.Stoken, nil
}

// ShareDetail 列出分享目录（单页 200，自动翻页）。
func (u *UC) ShareDetail(pwdID, stoken, pdir string) ([]UCFile, error) {
	if pdir == "" {
		pdir = ucRootFID
	}
	var all []UCFile
	for page := 1; page <= 50; page++ {
		var out struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				List []UCFile `json:"list"`
			} `json:"data"`
		}
		err := u.api(http.MethodGet, "/share/sharepage/detail", map[string]string{
			"pwd_id":        pwdID,
			"stoken":        stoken,
			"pdir_fid":      pdir,
			"force":         "0",
			"_page":         strconv.Itoa(page),
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
			return nil, fmt.Errorf("UC 分享：%s（code %d）", out.Message, out.Code)
		}
		all = append(all, out.Data.List...)
		if len(out.Data.List) < 200 {
			break
		}
	}
	return all, nil
}

// SaveFile 把分享文件转存到自己盘，返回异步任务 ID。
func (u *UC) SaveFile(pwdID, stoken, shareFID, fidToken, toPdir string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	err := u.api(http.MethodPost, "/share/sharepage/save", nil, map[string]interface{}{
		"fid_list":       []string{shareFID},
		"fid_token_list": []string{fidToken},
		"to_pdir_fid":    toPdir,
		"pwd_id":         pwdID,
		"stoken":         stoken,
		"pdir_fid":       "0",
		"scene":          "link",
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.TaskID == "" {
		msg := out.Message
		if msg == "" {
			msg = "转存失败（分享可能已失效）"
		}
		return "", fmt.Errorf("UC 转存：%s（code %d）", msg, out.Code)
	}
	return out.Data.TaskID, nil
}

// PollTask 轮询转存任务，返回新文件 fid（取第一个）。
func (u *UC) PollTask(taskID string) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		var out struct {
			Code int `json:"code"`
			Data struct {
				Status   int    `json:"status"`
				TaskID   string `json:"task_id"`
				SaveAs   struct {
					FID string `json:"fid"`
				} `json:"save_as"`
				SaveAsTop struct {
					FID string `json:"fid"`
				} `json:"save_as_top_fids"`
			} `json:"data"`
		}
		err := u.api(http.MethodGet, "/task", map[string]string{
			"task_id": taskID,
			"retry_index": "0",
			"device_id":   "",
		}, nil, &out)
		if err == nil && out.Code == 0 {
			if out.Data.SaveAs.FID != "" {
				return out.Data.SaveAs.FID, nil
			}
			if out.Data.Status == 2 {
				// UC 的转存任务不回传 fid（实测 2026-10-22：status=2、task_type=17，
				// save_as 为空）。按「已完成」返回空 fid，由调用方列目录按名找。
				return "", nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("UC 转存超时（任务 %s 未完成）", taskID)
		}
		time.Sleep(700 * time.Millisecond)
	}
}

// ListDir 列出自己盘的目录。
func (u *UC) ListDir(pdir string) ([]UCFile, error) {
	if pdir == "" {
		pdir = ucRootFID
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []UCFile `json:"list"`
		} `json:"data"`
	}
	err := u.api(http.MethodGet, "/file/sort", map[string]string{
		"pdir_fid": pdir,
		"_page":    "1",
		"_size":    "200",
		"_sort":    "file_type:asc,updated_at:desc",
	}, nil, &out)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("UC：%s（code %d）", out.Message, out.Code)
	}
	return out.Data.List, nil
}

// Mkdir 新建目录，返回 fid。
func (u *UC) Mkdir(pdir, name string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FID string `json:"fid"`
		} `json:"data"`
	}
	err := u.api(http.MethodPost, "/file", map[string]string{"method": "mkdir"}, map[string]interface{}{
		"pdir_fid": pdir,
		"file_name": name,
		"dir_path":  "",
		"dir_init_lock": false,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("UC 建目录：%s（code %d）", out.Message, out.Code)
	}
	return out.Data.FID, nil
}

// Delete 批量删除（进回收站）。
func (u *UC) Delete(fids []string) error {
	if len(fids) == 0 {
		return nil
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	err := u.api(http.MethodPost, "/file/delete", nil, map[string]interface{}{
		"action_type": 2,
		"filelist":    fids,
		"exclude_fids": []string{},
	}, &out)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("UC 删除：%s（code %d）", out.Message, out.Code)
	}
	return nil
}

// DownloadURL 取自己盘文件的下载直链（转存后用；不带分享 callback）。
func (u *UC) DownloadURL(fid string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			DownloadURL string `json:"download_url"`
		} `json:"data"`
	}
	err := u.api(http.MethodPost, "/file/download", nil, map[string]interface{}{
		"fids": []string{fid},
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("UC：%s（code %d）", out.Message, out.Code)
	}
	if len(out.Data) == 0 || out.Data[0].DownloadURL == "" {
		return "", fmt.Errorf("UC 未返回下载地址")
	}
	return out.Data[0].DownloadURL, nil
}
