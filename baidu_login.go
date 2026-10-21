package main

// ---------------------------------------------------------------------------
// 百度网盘登录
//
// 来源：巨库（Juku）里的百度实现——核心卖点是「免会员也能高速、流畅地播放
// 分享链接里的视频」。本文件只做登录这一件事：把账号变成一条可存盘的 BDUSS。
//
// 流程（百度 passport 标准扫码）：
//   getqrcode → 拿到 sign(轮询用) + imgurl(二维码图片)
//   → 手机百度/网盘 App 扫码 → channel/unicast 轮询
//   → 扫码确认后 data.v 里是加密串 → qrbdusslogin 解出 BDUSS
// BDUSS 即百度网盘的登录凭证，后续取分享直链、免会员加速都靠它。
//
// 关键修复：getqrcode / unicast / qrbdusslogin 必须共享同一会话 cookie，
// 否则百度无法把轮询关联到二维码，unicast 永远收不到已扫码的 v。
// 因此这里用独立的、带 cookiejar 的 http.Client（贴在 session 上）。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	baiduUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	baiduPassAPI = "https://passport.baidu.com"
	baiduPanAPI  = "https://pan.baidu.com"
)

// baiduQRSession 一次百度扫码登录会话（内存态）。
type baiduQRSession struct {
	id      string
	sign    string
	imgurl  string
	status  int // 0 等待扫码 / 1 已扫码待确认 / 2 成功 / -1 过期
	message string
	bduss   string
	label   string
	err     string
	done    bool
	created time.Time
	pollCount int // 空闲轮询计数（仅用于调试日志）
	hc      *http.Client // 带 cookiejar，维护整条登录会话
}

var baiduQRMu sync.Mutex
var baiduQRSessions = map[string]*baiduQRSession{}

func baiduQRGC() {
	baiduQRMu.Lock()
	defer baiduQRMu.Unlock()
	for id, s := range baiduQRSessions {
		if time.Since(s.created) > 10*time.Minute {
			delete(baiduQRSessions, id)
		}
	}
}

// startQRBaidu 发起扫码，并在后台自动轮询状态。
func startQRBaidu() (*baiduQRSession, error) {
	baiduQRGC()
	jar, _ := cookiejar.New(nil)
	// 注意：channel/unicast 是百度长轮询接口，会挂起约 30s 才回包（errno:1=无事件）。
	// 超时必须 > 30s，否则每次轮询会被提前砍断、永远收不到扫码事件。
	hc := &http.Client{Timeout: 45 * time.Second, Jar: jar}
	sign, img, err := baiduGetQRCode(hc)
	if err != nil {
		return nil, err
	}
	log.Printf("[baidu] 生成二维码 sign=%s", sign)
	s := &baiduQRSession{
		id:      newID(),
		sign:    sign,
		imgurl:  img,
		status:  0,
		message: "等待扫码",
		created: time.Now(),
		hc:      hc,
	}
	baiduQRMu.Lock()
	baiduQRSessions[s.id] = s
	baiduQRMu.Unlock()
	go s.watchBaidu()
	return s, nil
}

// watchBaidu 后台轮询扫码状态，扫码确认后自动完成登录并校验。
func (s *baiduQRSession) watchBaidu() {
	deadline := time.Now().Add(10 * time.Minute)
	fails := 0
	for {
		time.Sleep(2 * time.Second)

		baiduQRMu.Lock()
		if s.done {
			baiduQRMu.Unlock()
			return
		}
		baiduQRMu.Unlock()

		if time.Now().After(deadline) {
			baiduQRMu.Lock()
			s.status, s.message, s.done = -1, "二维码已过期，请重新获取", true
			baiduQRMu.Unlock()
			return
		}

		status, v, raw, err := baiduPoll(s.hc, s.sign)
		if err != nil {
			fails++
			if fails <= 2 || fails%10 == 0 {
				log.Printf("[baidu] 轮询异常 sign=%s fails=%d err=%v", s.sign, fails, err)
			}
			if fails >= 8 {
				baiduQRMu.Lock()
				s.message = fmt.Sprintf("⚠️ 网络受限，正在重试（已失败 %d 次）", fails)
				baiduQRMu.Unlock()
			}
			continue
		}
		if fails > 0 && fails%10 == 0 {
			log.Printf("[baidu] 长轮询正常 sign=%s（连续成功）", s.sign)
		}
		fails = 0
		// 空闲轮询打原始回包：前 5 次全打，之后每 5 次打一次。
		// 这样扫码期间日志持续可见（能看到百度确实在返回 errno:1，通道是活的）。
		if status == 0 {
			s.pollCount++
			if s.pollCount <= 5 || s.pollCount%5 == 0 {
				log.Printf("[baidu] 空闲轮询#%d raw=%s", s.pollCount, trunc(raw, 120))
			}
		}

		baiduQRMu.Lock()
		switch {
		case v != "":
			s.status, s.message = 1, "已扫码，正在登录"
			baiduQRMu.Unlock()
			log.Printf("[baidu] 收到扫码确认 sign=%s，开始换 BDUSS", s.sign)
			bduss, lerr := baiduFinishLogin(s.hc, v)
			baiduQRMu.Lock()
			if lerr != nil {
				s.err = "登录失败: " + lerr.Error()
				s.message, s.done = s.err, true
				log.Printf("[baidu] 换 BDUSS 失败: %v", lerr)
				break
			}
			if lbl, cerr := baiduCheck(bduss); cerr != nil {
				s.err = "BDUSS 无效: " + cerr.Error()
				s.message, s.done = s.err, true
				log.Printf("[baidu] BDUSS 校验失败: %v", cerr)
				break
			} else {
				s.bduss, s.label = bduss, lbl
				s.status, s.message, s.done = 2, "登录成功", true
				log.Printf("[baidu] 登录成功 label=%s", lbl)
			}
		case status == 1:
			s.status, s.message = 1, "已扫码，请在手机上确认"
		default:
			s.status, s.message = 0, "等待扫码"
		}
		baiduQRMu.Unlock()
		if s.done {
			return
		}
	}
}

// baiduGetQRCode 取二维码：返回轮询用的 sign 与展示用的二维码图片地址。
func baiduGetQRCode(hc *http.Client) (sign, imgurl string, err error) {
	ts := fmt.Sprintf("%d", time.Now().UnixNano()/int64(time.Millisecond))
	u := baiduPassAPI + "/v2/api/getqrcode?lp=pc&_=" + ts
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", baiduUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Errno  int    `json:"errno"`
		Sign   string `json:"sign"`
		ImgURL string `json:"imgurl"`
		Link   string `json:"link"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("百度返回无法解析: %s", trunc(string(body), 200))
	}
	if out.Errno != 0 || out.Sign == "" {
		return "", "", fmt.Errorf("获取二维码失败(errno=%d)", out.Errno)
	}
	img := out.ImgURL
	if img == "" {
		img = out.Link
	}
	if img != "" && !strings.HasPrefix(img, "http") {
		img = "https://" + img
	}
	return out.Sign, img, nil
}

// baiduPoll 轮询扫码状态，返回 (status, v, raw, err)。v 非空即已扫码确认。
// 注意：unicast 是长轮询，服务端会挂起约 30s 后返回 errno:1（无事件，属正常）。
// 该响应不应被当成错误，否则会被失败计数误判；只有真正网络异常才算 err。
// raw 返回原始响应体，便于排查扫码为何没反应。
func baiduPoll(hc *http.Client, sign string) (status int, v string, raw string, err error) {
	ts := fmt.Sprintf("%d", time.Now().UnixNano()/int64(time.Millisecond))
	u := fmt.Sprintf("%s/channel/unicast?channel_id=%s&tpl=netdisk&callback=&apiver=v3&tt=%s&_=%s",
		baiduPassAPI, url.QueryEscape(sign), ts, ts)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", baiduUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	raw = strings.TrimSpace(string(body))
	// 去掉 JSONP 包裹：callback(...) 或裸 (...)
	if i := strings.Index(raw, "("); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.LastIndex(raw, ")"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, "", "", fmt.Errorf("轮询返回空响应")
	}
	// 百度 unicast 的真实返回格式（实测）：
	//   未扫码： {"errno":1}                                                 长轮询超时，无事件
	//   已扫码： {"errno":0,"channel_id":"<sign>","channel_v":"{\"status\":1}"}                     待确认
	//   已确认： {"errno":0,"channel_id":"<sign>","channel_v":"{\"status\":0,\"v\":\"<令牌>\"}"}     拿到 v
	// ⚠️ 关键：事件数据在 channel_v 里，且 channel_v 是「被转义的 JSON 字符串」，必须再解一层。
	// 早期代码只看顶层 v / data.v，永远读不到 channel_v → 把已扫码事件误判成无事件。
	var out struct {
		Errno    int    `json:"errno"`
		ChannelV string `json:"channel_v"` // 百度真实事件的载体
		// 兼容其它可能形态
		Status int    `json:"status"`
		V      string `json:"v"`
		Data   struct {
			V string `json:"v"`
		} `json:"data"`
	}
	if e := json.Unmarshal([]byte(raw), &out); e != nil {
		return 0, "", raw, fmt.Errorf("轮询返回无法解析: %s", trunc(raw, 120))
	}
	// ① 主路径：解析 channel_v（百度真实格式）
	if out.ChannelV != "" {
		var cv struct {
			Status int    `json:"status"`
			V      string `json:"v"`
		}
		if json.Unmarshal([]byte(out.ChannelV), &cv) == nil {
			switch {
			case cv.V != "": // 已确认，拿到令牌 v
				log.Printf("[baidu] 轮询收到确认 v(长度%d) raw=%s", len(cv.V), trunc(raw, 200))
				return 2, cv.V, raw, nil
			case cv.Status == 1 || cv.Status == 2: // 已扫码，待手机确认
				log.Printf("[baidu] 轮询收到扫码事件 status=%d raw=%s", cv.Status, trunc(raw, 200))
				return cv.Status, "", raw, nil
			}
		}
	}
	// ② 兼容：v 在顶层或 data 里
	vt := out.Data.V
	if vt == "" {
		vt = out.V
	}
	switch {
	case vt != "":
		status = 2 // 已拿到加密令牌，立刻去换 BDUSS
	case out.Status == 1 || out.Status == 2:
		status = out.Status
	default:
		status = 0 // 无事件（含 errno:1 长轮询超时），继续轮询
	}
	if status != 0 {
		log.Printf("[baidu] 轮询收到事件 sign=%s status=%d raw=%s", sign, status, trunc(raw, 200))
	}
	return status, vt, raw, nil
}

// baiduQRCB 换 BDUSS 时带的 JSONP callback 名（与巨库一致）。
const baiduQRCB = "68647976601306097149819007990813932172694353001433054093944634591855431833976560521225596406614545549772963113914808580371219879997166438125740282911150571516864797660130609714981900799081393217269435300143305409394463459185543183397655394245057746333217197532963996371363321113864768612440340372808892707005449"

// reBDUSSAny 从任意文本（Set-Cookie 头 / 响应体 / cookie 串）里抠 BDUSS 值。
// 注意：不会误匹配 BDUSS_BFESS（其后是下划线而非等号）。
var reBDUSSAny = regexp.MustCompile(`(?i)(?:^|[;\s])BDUSS=([^;,\s]+)`)

// baiduCookie 把存盘凭证规范化成可用的 Cookie 头：
//   - 已是完整 Cookie 串（含 =）  → 原样返回
//   - 只有裸 BDUSS 值            → 补成 "BDUSS=<v>"
func baiduCookie(acc string) string {
	acc = strings.TrimSpace(acc)
	if acc == "" {
		return ""
	}
	if strings.Contains(acc, "=") {
		return acc
	}
	return "BDUSS=" + acc
}

// jarCookieString 把 cookie jar 里 .baidu.com 域的所有 cookie 拼成 Cookie 头字符串。
// 关键：百度登录最终下发的 BDUSS/STOKEN 等，往往是 qrbdusslogin 自动跟随重定向的
// 中途通过 Set-Cookie 写进 jar 的，直接读 resp.Cookies() 会漏掉。
func jarCookieString(hc *http.Client) string {
	if hc == nil || hc.Jar == nil {
		return ""
	}
	seen := map[string]bool{}
	var parts []string
	for _, host := range []string{"https://pan.baidu.com", "https://passport.baidu.com", "https://www.baidu.com", "https://baidu.com"} {
		pu, e := url.Parse(host)
		if e != nil {
			continue
		}
		for _, c := range hc.Jar.Cookies(pu) {
			if c.Name == "" || c.Value == "" || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// cookieNames 只列出 cookie 名（不泄露值），用于日志。
func cookieNames(s string) string {
	var names []string
	for _, p := range strings.Split(s, ";") {
		p = strings.TrimSpace(p)
		if i := strings.Index(p, "="); i > 0 {
			names = append(names, p[:i])
		}
	}
	return strings.Join(names, ",")
}

// baiduFinishLogin 用扫码令牌 v 调 qrbdusslogin 完成登录，返回可用于取流的 Cookie 串。
// 依次尝试：cookie jar（含重定向中途下发）→ 最终响应的 Set-Cookie → 响应体里的 BDUSS=
//        → JSON/JSONP → v 兜底。
func baiduFinishLogin(hc *http.Client, v string) (string, error) {
	ts := fmt.Sprintf("%d", time.Now().UnixNano()/int64(time.Millisecond))
	// ⚠️ 关键：qrbdusslogin 的「票据」参数名是 **bduss**（不是 v）。
	// 必须把 channel_v 里的 v 作为 bduss 的值传进去；写成 v=<票据> 会得到
	// 310005「验证信息已过期，请重新验证」。
	u := fmt.Sprintf("%s/v3/login/main/qrbdusslogin?bduss=%s&u=&loginVersion=v4&qrcode=1&tpl=netdisk&apiver=v3&tt=%s&traceid=&callback=bd__cbs__cupstt%s",
		baiduPassAPI, url.QueryEscape(v), ts, baiduQRCB)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", baiduUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := string(body)

	// ① 首选：cookie jar（整段，含 STOKEN 等）
	if full := jarCookieString(hc); reBDUSSAny.MatchString("; "+full) {
		log.Printf("[baidu] 登录成功；会话 cookie: %s", cookieNames(full))
		return full, nil
	}
	// ② 最终响应的 Set-Cookie
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if m := reBDUSSAny.FindStringSubmatch("; " + sc); m != nil {
			return "BDUSS=" + m[1], nil
		}
	}
	// ③ 响应体里的 BDUSS=xxx
	if m := reBDUSSAny.FindStringSubmatch("; " + text); m != nil {
		return "BDUSS=" + m[1], nil
	}
	// ④ JSON / JSONP
	js := text
	if i := strings.Index(js, "("); i >= 0 {
		js = js[i+1:]
	}
	if i := strings.LastIndex(js, ")"); i >= 0 {
		js = js[:i]
	}
	var out struct {
		Errno int    `json:"errno"`
		Bduss string `json:"bduss"`
		Data  struct {
			Bduss   string `json:"bduss"`
			Session struct {
				Bduss  string `json:"bduss"`
				Ptoken string `json:"ptoken"`
				Stoken string `json:"stoken"`
				Ubi    string `json:"ubi"`
			} `json:"session"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(js)), &out) == nil {
		if s := out.Data.Session; s.Bduss != "" {
			// 顺带把 PTOKEN / STOKEN / UBI 一起带上 —— 取流（播放线）要用
			parts := []string{"BDUSS=" + s.Bduss}
			if s.Ptoken != "" {
				parts = append(parts, "PTOKEN="+s.Ptoken)
			}
			if s.Stoken != "" {
				parts = append(parts, "STOKEN="+s.Stoken)
			}
			if s.Ubi != "" {
				parts = append(parts, "UBI="+s.Ubi)
			}
			return strings.Join(parts, "; "), nil
		}
		if out.Data.Bduss != "" {
			return "BDUSS=" + out.Data.Bduss, nil
		}
		if out.Bduss != "" {
			return "BDUSS=" + out.Bduss, nil
		}
	}
	// ⑤ v 里兜底
	if bd, e := baiduExtractBDUSS(v); e == nil && bd != "" {
		return "BDUSS=" + bd, nil
	}
	log.Printf("[baidu] qrbdusslogin 解析失败 status=%d 最终URL=%s body=%s jar=%s",
		resp.StatusCode, trunc(resp.Request.URL.String(), 160), trunc(text, 300), cookieNames(jarCookieString(hc)))
	return "", fmt.Errorf("qrbdusslogin 未返回 BDUSS")
}

// baiduExtractBDUSS 从 v 字段兜底解出 BDUSS（v 是 unicode 转义的键值对）。
func baiduExtractBDUSS(v string) (string, error) {
	s := strings.TrimPrefix(v, "(")
	s = strings.TrimSuffix(s, ")")
	s = strings.NewReplacer(`\u0026`, "&", `\u003d`, "=", `\u0027`, "'").Replace(s)
	if m := regexp.MustCompile(`(?i)bduss=([^&\s]+)`).FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("v 中未找到 bduss")
}

// baiduCheck 校验凭证是否仍有效，并返回账号显示名。
// cookie 可为裸 BDUSS 值或完整 Cookie 串（自动规范化）。
func baiduCheck(cookie string) (string, error) {
	u := baiduPanAPI + "/rest/2.0/membership/user?method=query&channel=chunlei&web=1&app_id=250528"
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", baiduUA)
	req.Header.Set("Cookie", baiduCookie(cookie))
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Errno int `json:"errno"`
		Data  struct {
			UserName  string `json:"user_name"`
			BaiduName string `json:"baidu_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("百度返回无法解析")
	}
	if out.Errno != 0 {
		return "", fmt.Errorf("登录态无效(errno=%d)", out.Errno)
	}
	name := out.Data.BaiduName
	if name == "" {
		name = out.Data.UserName
	}
	if name == "" {
		name = "百度网盘"
	}
	return name, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- HTTP 接口 ----

// apiLoginStartBaidu 开始扫码：POST /api/login/baidu/start
func apiLoginStartBaidu(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}
	s, err := startQRBaidu()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":  true,
		"sid": s.id,
		"qr":  s.imgurl,
		"ttl": 600, // 二维码有效期（秒），前端据此倒计时
	})
}

// apiLoginPollBaidu 轮询扫码状态：GET /api/login/baidu/poll?sid=xxx
func apiLoginPollBaidu(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("sid")
		baiduQRMu.Lock()
		s, ok := baiduQRSessions[sid]
		baiduQRMu.Unlock()
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "会话不存在或已过期"})
			return
		}
		resp := map[string]interface{}{
			"ok":      true,
			"status":  s.status,
			"message": s.message,
			"done":    s.done,
		}
		if s.err != "" {
			resp["error"] = s.err
		}
		if s.done && s.bduss != "" {
			label := s.label
			if label == "" {
				label = "百度网盘"
			}
			installAccount(as, Account{
				Provider: "baidu",
				Label:    label,
				Cookie:   s.bduss,
				LoginAt:  time.Now(),
			})
			resp["label"] = label
		}
		writeJSON(w, resp)
	}
}
