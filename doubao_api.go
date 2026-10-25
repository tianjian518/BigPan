package main

// ---------------------------------------------------------------------------
// 豆包云盘 接口层
//
// 2026-10-23 实测结论：
//   · 鉴权只认 sessionid（30 天有效）。LARK_SUITE_ACCESS_TOKEN、DPoP、
//     biz_auth 续期全都不需要 —— 官方那套 DPoP 续的是接口用不到的凭证。
//   · 限流是「突发限流」不是「速率上限」：无间隔连发几次就 429，
//     0.35 秒间隔连打 8 次全过。这里取 500 毫秒统一节流 + 429 退避。
//   · 视频档位 360p/540p/720p/1080p 全是 h264，浏览器可直解。
//   · 1080p 直链无 Cookie、无 Referer 即可直连 → 可 302，数据不过本机。
//
// 两个入口：
//   自己的盘  aispace/homepage + aispace/node_info + media/get_play_info
//   别人分享  aispace/share/overview + aispace/share/node_info + 同上取链
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	doubaoBase = "https://www.doubao.com"
	doubaoUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	doubaoQs   = "version_code=20800&device_platform=web&aid=497858"
)

// dbPageSize 每页条数。
//
// 默认只给 50 条 —— 一个几千项的目录要翻 20 多次页，请求数白白翻几十倍，
// 而豆包的风控就是按请求数算的。实测传 size=1000 能一次返回 1000 条，
// 请求数直接砍掉一个量级，这是减少风控最有效的一招。
const dbPageSize = 1000

// doubaoGap 全局限速的基础间隔。可被 -doubao-gap 覆盖。
//
// 豆包是「滑动窗口」限流，不是硬性持久封禁：
//   · 400ms（150 次/分钟）→ 约 300 个目录撞线
//   · 600ms（100 次/分钟）→ 机房出口实测连打 40 次无异常
//   · 1 秒（60 次/分钟）  → 家宽出口约 280 个目录撞线，机房出口 380 次还没事
//   · 2 秒（30 次/分钟）  → 5 分钟只打 150 次，最稳但慢
//
// 注意：固定值只有两种结果 —— 要么慢到扫描超时，要么快到撞风控。
// 所以这里取 600ms 作起点，再配 dbGapOnFail/dbGapOnOK 自适应：
// 一旦撞限流就成倍放大（最多 4 秒），连续顺畅 30 次再折半回落。
var doubaoGap = 600 * time.Millisecond

// 自适应间隔状态：dbGapCur 为 0 表示用 doubaoGap 基础值。
var (
	dbGapMu   sync.Mutex
	dbGapCur  time.Duration
	dbGapFail int
	dbGapOK   int
)

func dbGapNow() time.Duration {
	dbGapMu.Lock()
	defer dbGapMu.Unlock()
	if dbGapCur <= 0 {
		return doubaoGap
	}
	return dbGapCur
}

// dbGapOnFail 撞限流：连续 2 次就成倍放大间隔，最多 4 秒。
func dbGapOnFail() {
	dbGapMu.Lock()
	defer dbGapMu.Unlock()
	dbGapFail++
	if dbGapFail >= 2 {
		cur := dbGapCur
		if cur <= 0 {
			cur = doubaoGap
		}
		if next := cur * 2; next > 4*time.Second {
			dbGapCur = 4 * time.Second
		} else {
			dbGapCur = next
		}
		dbGapFail = 0
		log.Printf("[豆包] 撞限流，请求间隔放大到 %v", dbGapCur)
	}
}

// dbGapOnOK 连续成功 30 次，间隔折半回落，最低回到基础值。
func dbGapOnOK() {
	dbGapMu.Lock()
	defer dbGapMu.Unlock()
	dbGapFail = 0
	if dbGapCur <= doubaoGap {
		dbGapCur = 0
		dbGapOK = 0
		return
	}
	dbGapOK++
	if dbGapOK >= 30 {
		dbGapOK = 0
		if next := dbGapCur / 2; next <= doubaoGap {
			dbGapCur = 0
			log.Printf("[豆包] 恢复顺畅，请求间隔回落到基础值 %v", doubaoGap)
		} else {
			dbGapCur = next
			log.Printf("[豆包] 恢复顺畅，请求间隔回落到 %v", dbGapCur)
		}
	}
}

// dbNode 是豆包的节点（文件或目录）。node_type：1=目录 2=普通文件 6=视频。
type dbNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	NodeType int    `json:"node_type"`
	Size     int64  `json:"size"`
	ParentID string `json:"parent_id"`
}

func (n dbNode) IsDir() bool { return n.NodeType == 1 }

// dbMedia 是一档转码流。
type dbMedia struct {
	MainURL string `json:"main_url"`
	Meta    struct {
		Definition string `json:"definition"`
		Width      string `json:"width"`  // 豆包返回的是字符串 "1920"
		Height     string `json:"height"` // 同上
		CodecType  string `json:"codec_type"`
	} `json:"meta"`
}

type Doubao struct {
	cookie string
	hc     *http.Client
	tr     *http.Transport

	ipMu sync.Mutex
	ips  []string // 最近一次解析到的出口 IP，用来判断「是不是全都冷却了」

	base string // 仅测试用：把请求打到 mock 服务器

	// v2 新链路（feishu drive + DPoP）
	v2Key     *ecdsa.PrivateKey
	v2Token   string    // LARK_SUITE_ACCESS_TOKEN / biz_auth 换来的新 token
	v2TokenAt time.Time // v2Token 的写入时间，用于判断何时刷新
	v2CSRF    string    // biz_auth 刷新要带的 csrf
	v2Mu      sync.Mutex
}

// endpoint 拼出接口地址。base 为空时走线上域名。
func (d *Doubao) endpoint(path string) string {
	b := d.base
	if b == "" {
		b = doubaoBase
	}
	return b + path + "?" + doubaoQs
}

// ---- 出口池（按 IP 冷却）----
//
// 豆包的风控是按「出口 IP」算的，不是按协议栈：
// 同一个 IP 短时间打多了就返回 200 + 空体（连 429 都不给），
// 换一条出口 IP 立刻恢复。沙箱全新 IPv4 出口连打 20 次全通，
// 证明它没有封 IPv4 协议栈 —— 被封的是"那一个地址"，不是"那一种协议"。
//
// 所以策略：v4 优先，v4 冷却了就换 v6，两条都冷却就明确告诉用户
// 「正在风控中」，绝不返回空目录冒充"这个文件夹是空的"。

const (
	dbCoolFirst = 90 * time.Second // 首次风控冷却（实测约 100 秒恢复，留点余量）
	dbCoolMax   = 30 * time.Minute // 连续失败上限
)

type dbExitState struct {
	downTo time.Time
	fails  int
}

var (
	dbExitMu sync.Mutex
	dbExits  = map[string]*dbExitState{}
)

// dbExitUsable 判断某出口 IP 现在能不能用。冷却到期自动放行。
func dbExitUsable(ip string) bool {
	dbExitMu.Lock()
	defer dbExitMu.Unlock()
	st := dbExits[ip]
	return st == nil || !time.Now().Before(st.downTo)
}

// dbExitFail 记一次风控，冷却时间翻倍增长，返回本次冷却时长。
func dbExitFail(ip string) time.Duration {
	dbExitMu.Lock()
	defer dbExitMu.Unlock()
	st := dbExits[ip]
	if st == nil {
		st = &dbExitState{}
		dbExits[ip] = st
	}
	st.fails++
	d := dbCoolFirst
	for i := 1; i < st.fails; i++ {
		if d >= dbCoolMax {
			break
		}
		d *= 2
	}
	if d > dbCoolMax {
		d = dbCoolMax
	}
	st.downTo = time.Now().Add(d)
	return d
}

// dbExitOK 请求成功，清掉该 IP 的失败计数（恢复后重新排队）。
func dbExitOK(ip string) {
	if ip == "" {
		return
	}
	dbExitMu.Lock()
	delete(dbExits, ip)
	dbExitMu.Unlock()
}

// dbExitWait 所有出口都在冷却时，返回最快那个还要等多久。
func dbExitWait() time.Duration {
	dbExitMu.Lock()
	defer dbExitMu.Unlock()
	now := time.Now()
	var best time.Duration
	for _, st := range dbExits {
		if now.Before(st.downTo) {
			if d := st.downTo.Sub(now); best == 0 || d < best {
				best = d
			}
		}
	}
	return best
}

// dbThrottleErr 表示豆包整体风控中，附带预计恢复时间。
// 这个错误会原样透到界面上，用户看到的是人话而不是"空目录"。
type dbThrottleErr struct{ wait time.Duration }

func (e *dbThrottleErr) Error() string {
	m := int(e.wait.Minutes() + 0.99)
	if m < 1 {
		m = 1
	}
	return fmt.Sprintf("豆包正在风控中，请约 %d 分钟后再试（冷却后会自动恢复，不用重粘 Cookie）", m)
}

// isThrottleErr 判断是否风控错误。
func isThrottleErr(err error) bool {
	_, ok := err.(*dbThrottleErr)
	return ok
}

// dbTrace 记录本次请求实际走了哪个出口 IP，用来在失败时精准冷却它。
type dbTrace struct{ ip string }

type dbTraceKey struct{}

// blockedFor 判断这台机器上所有出口是不是都在冷却，是则返回还要等多久。
//
// 为什么不能只靠拨号时判断：http.Transport 会复用空闲连接，
// 连接还在池子里时根本不会触发 DialContext，冷却就形同虚设。
// 所以每次请求前先看一眼，全冷却就别白打。
func (d *Doubao) blockedFor() time.Duration {
	d.ipMu.Lock()
	ips := d.ips
	d.ipMu.Unlock()
	if len(ips) == 0 {
		return 0
	}
	dbExitMu.Lock()
	defer dbExitMu.Unlock()
	now := time.Now()
	var best time.Duration
	for _, ip := range ips {
		st := dbExits[ip]
		if st == nil || !now.Before(st.downTo) {
			return 0 // 还有一条能用
		}
		if dd := st.downTo.Sub(now); best == 0 || dd < best {
			best = dd
		}
	}
	return best
}

// setIPs 记下解析结果（拨号时调用）。
func (d *Doubao) setIPs(v4, v6 []string) {
	all := make([]string, 0, len(v4)+len(v6))
	all = append(all, v4...)
	all = append(all, v6...)
	d.ipMu.Lock()
	d.ips = all
	d.ipMu.Unlock()
}

// newDoubao 构造客户端。
//
// 拨号顺序：**IPv4 优先 → IPv6 兜底**，跳过正在冷却的 IP。
// 全部冷却时直接返回 dbThrottleErr，不去撞无谓的请求。
func newDoubao(cookie string) *Doubao { return newDoubaoAt(cookie, "") }

// initV2 从 Cookie 里提取新链路的凭证。缺哪样都不影响老链路，只是降级。
func (d *Doubao) initV2() {
	get := func(k string) string {
		for _, kv := range strings.Split(d.cookie, ";") {
			kk, vv, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || strings.TrimSpace(kk) != k {
				continue
			}
			// 注意：必须用 PathUnescape。QueryUnescape 会把 base64 里的
			// 「+」解成空格，keypair 直接解不出来。
			v, err := url.PathUnescape(strings.TrimSpace(vv))
			if err != nil {
				v = strings.TrimSpace(vv)
			}
			return v
		}
		return ""
	}
	d.v2Token = get("LARK_SUITE_ACCESS_TOKEN")
	d.v2CSRF = get("passport_csrf_token")
	kp := get("feishu_dpop_keypair")
	if kp == "" || d.v2Token == "" {
		return
	}
	sk, err := decryptDPoPKeyPair(kp, dbnSecret)
	if err != nil {
		log.Printf("[豆包v2] keypair 解密失败，沿用老链路：%v", err)
		return
	}
	d.v2Key = sk
	log.Printf("[豆包v2] 新链路就绪（DPoP 私钥已解密，token 自动续期可用）")
}

// newDoubaoAt 同 newDoubao，但可以把请求打到指定 base（测试用）。
func newDoubaoAt(cookie, base string) *Doubao {
	d := &Doubao{cookie: strings.TrimSpace(cookie), base: base}
	d.initV2()
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || net.ParseIP(host) != nil {
				// IP 直连（DNS 回退 / 测试服务器）：也要登记成出口，
				// 否则 blockedFor 看不到任何 IP，冷却守卫就失效了。
				if net.ParseIP(host) != nil {
					d.setIPs([]string{host}, nil)
				}
				c, e := dialer.DialContext(ctx, network, addr)
				if e == nil {
					if t, ok := ctx.Value(dbTraceKey{}).(*dbTrace); ok {
						t.ip = host
					}
				}
				return c, e
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				return dialer.DialContext(ctx, network, addr)
			}
			var v4, v6 []string
			for _, ia := range ips {
				if ia.IP.To4() == nil {
					v6 = append(v6, ia.IP.String())
				} else {
					v4 = append(v4, ia.IP.String())
				}
			}
			d.setIPs(v4, v6) // 记下这台机器有哪些出口，post 前用来判断全冷却
			for _, list := range [][]string{v4, v6} {
				for _, ip := range list {
					if !dbExitUsable(ip) {
						continue // 这条出口正在冷却，换下一条
					}
					c, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
					if e == nil {
						if t, ok := ctx.Value(dbTraceKey{}).(*dbTrace); ok {
							t.ip = ip
						}
						return c, nil
					}
				}
			}
			if w := dbExitWait(); w > 0 {
				return nil, &dbThrottleErr{wait: w}
			}
			return nil, fmt.Errorf("连接 %s 失败（IPv4/IPv6 均不可达）", host)
		},
	}
	d.tr = tr
	d.hc = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	return d
}

// ---- 全局限速 ----

var (
	dbGateMu   sync.Mutex
	dbLastCall time.Time
)

// dbThrottle 保证任意两次豆包请求至少间隔当前生效的间隔（自适应）。
func dbThrottle() {
	gap := dbGapNow()
	dbGateMu.Lock()
	defer dbGateMu.Unlock()
	if d := gap - time.Since(dbLastCall); d > 0 {
		time.Sleep(d)
	}
	dbLastCall = time.Now()
}

func dbReqID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// coolDown 把这次用的出口 IP 打进冷却，并清掉空闲连接。
// 不清连接的话下一次请求会复用同一条 TCP，等于还用同一个 IP，白等。
func (d *Doubao) coolDown(ip string) {
	if ip == "" {
		return
	}
	dbExitFail(ip)
	if d.tr != nil {
		d.tr.CloseIdleConnections()
	}
}

// post 调一个豆包接口，返回 data 原始 JSON。
//
// 重试策略（配合出口池）：
//   - 命中风控（429 / 200+空体 / code 限流）→ 冷却当前出口 IP，
//     **立刻**换下一条出口重试，不做无意义的等待；
//   - 纯网络失败（连不上、超时）→ 按 1s / 2s / 3s 退避；
//   - 所有出口都在冷却 → 返回 dbThrottleErr（人话提示），不返回空数据。
func (d *Doubao) post(path string, body any) (json.RawMessage, error) {
	if d.cookie == "" {
		return nil, fmt.Errorf("豆包未登录（请先粘贴 Cookie）")
	}
	// 连接池里的空闲连接会绕过拨号，所以每次请求前先看一眼出口状态：
	// 全都冷却就直接给提示，别拿用户的等待时间去撞墙。
	if w := d.blockedFor(); w > 0 {
		return nil, &dbThrottleErr{wait: w}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	limitHit := false // 上一轮是不是风控失败（是则立刻换出口，不退避）
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 && !limitHit {
			time.Sleep(time.Duration(attempt) * time.Second) // 1s / 2s / 3s
		}
		limitHit = false
		dbThrottle()
		// 每一轮都重新看一次出口：上一轮可能刚把 v4 打黑，
		// 这时如果 v6 也没了就别再白打一轮。
		if w := d.blockedFor(); w > 0 {
			return nil, &dbThrottleErr{wait: w}
		}

		trace := &dbTrace{}
		ctx := context.WithValue(context.Background(), dbTraceKey{}, trace)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			d.endpoint(path), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", doubaoUA)
		req.Header.Set("Origin", doubaoBase)
		req.Header.Set("Referer", doubaoBase+"/")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("agw-js-conv", "str")
		req.Header.Set("x-request-id", dbReqID())
		req.Header.Set("Cookie", d.cookie)

		resp, err := d.hc.Do(req)
		if err != nil {
			if isThrottleErr(err) {
				return nil, err // 全部出口冷却，直接告诉用户
			}
			lastErr = fmt.Errorf("连接豆包失败：%w", err)
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = fmt.Errorf("读取豆包响应失败：%w", rerr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			d.coolDown(trace.ip)
			dbGapOnFail()
			lastErr = fmt.Errorf("豆包接口限流")
			limitHit = true
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("豆包返回 HTTP %d：%s", resp.StatusCode, dbSnippet(raw))
		}
		// 豆包风控时返回 HTTP 200 + **空响应体**（不是 429）。
		// 这是当前出口 IP 被限，不是「这个目录本来就是空的」。
		if len(bytes.TrimSpace(raw)) == 0 {
			d.coolDown(trace.ip)
			dbGapOnFail()
			lastErr = fmt.Errorf("豆包返回空响应（出口被风控）")
			limitHit = true
			continue
		}
		var out struct {
			Code    int             `json:"code"`
			Msg     string          `json:"msg"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("豆包返回无法识别（HTTP %d，%d 字节）：%s", resp.StatusCode, len(raw), dbSnippet(raw))
		}
		if out.Code != 0 {
			msg := out.Msg
			if msg == "" {
				msg = out.Message
			}
			low := strings.ToLower(msg)
			if strings.Contains(low, "too many") || strings.Contains(msg, "频繁") || out.Code == 429 {
				d.coolDown(trace.ip)
				dbGapOnFail()
				lastErr = fmt.Errorf("豆包接口限流：%s", msg)
				limitHit = true
				continue
			}
			switch out.Code {
			case 710012001:
				return nil, fmt.Errorf("豆包登录已失效，请重新粘贴 Cookie")
			case 672020011:
				return nil, fmt.Errorf("该分享是豆包新格式（飞书体系），暂不支持")
			case 671010000:
				return nil, fmt.Errorf("豆包参数不合法")
			}
			return nil, fmt.Errorf("豆包接口错误 %d：%s", out.Code, msg)
		}
		dbExitOK(trace.ip) // 这条出口是好的，放回池子
		dbGapOnOK()
		return out.Data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("豆包接口请求失败")
	}
	// 试完所有出口还是被限 → 给明确提示，不要拿"空目录"糊弄用户
	if limitHit {
		if w := dbExitWait(); w > 0 {
			return nil, &dbThrottleErr{wait: w}
		}
	}
	return nil, lastErr
}

func dbSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > 160 {
		return string(r[:160])
	}
	return s
}

// dbUserInfo 取账号昵称与手机号（用于自动命名账号）。
//
// 这个接口是 OpenList 豆包驱动在用的，我之前试了 8 个 /samantha/* 路径全 404，
// 漏了它。返回里有 name / screen_name / mobile（打码）/ avatar_url。
func (d *Doubao) dbUserInfo() (name, mobile string, err error) {
	if w := d.blockedFor(); w > 0 {
		return "", "", &dbThrottleErr{wait: w}
	}
	trace := &dbTrace{}
	ctx := context.WithValue(context.Background(), dbTraceKey{}, trace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		doubaoBase+"/passport/account/info/v2/", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", doubaoUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", d.cookie)
	dbThrottle()
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(bytes.TrimSpace(raw)) == 0 {
		return "", "", fmt.Errorf("豆包账号信息不可用（HTTP %d）", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Name       string `json:"name"`
			ScreenName string `json:"screen_name"`
			Mobile     string `json:"mobile"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	name = out.Data.Name
	if name == "" {
		name = out.Data.ScreenName
	}
	dbExitOK(trace.ip)
	return strings.TrimSpace(name), out.Data.Mobile, nil
}

// ---- 自己的盘 ----

// dbIsOldNodeID 新链路的节点 token 是飞书字母串，老的 node_id 是纯数字。
// 据此路由：老条目继续走老链路（sessionid），新条目走 DPoP。
func dbIsOldNodeID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (d *Doubao) dbnToList(nodeID string) ([]dbNode, error) {
	nodes, err := d.dbnListChildren(nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]dbNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, dbnToOldNode(n))
	}
	return out, nil
}

// dbHome 列根目录，返回根节点 ID 与顶层条目。
//
// ⚠️ 必须优先走 aispace（自家盘），不能用飞书新链路打头。
// 理由（2026-10-25 实测定论，§23.121）：
//   - aispace 返回的节点带**数字 node_id + tos key**，取链走 get_play_info，
//     直链落在豆包自家 CDN（v9/v26-videoweb.doubao.com）：单文件 9MB/s，
//     且**不限连接数**，256KB 小块 0.2 秒/块 —— 爆米花/拖进度条都不卡；
//   - 飞书新链路返回的是 obj_token（nodcn…），取链只能走 relay
//     （internal-api-drive-stream.feishu.cn）：一文件只给一条全速连接，
//     256KB 小块要 2~3 秒/块，WebDAV 播放就是 100KB/s。
//
// 自家盘内容在 aispace 里是完整的（天剑豆包影视下实测 1390+ 视频）。
// 只有 aispace 失败时才降级飞书，保证新版分享照样能浏览。
func (d *Doubao) dbHome() (rootID string, children []dbNode, err error) {
	data, aerr := d.post("/samantha/aispace/homepage", map[string]any{})
	if aerr == nil {
		body := dbUnwrap(data)
		var out struct {
			RootID   string   `json:"root_id"`
			Children []dbNode `json:"children"`
		}
		if e := json.Unmarshal(body, &out); e == nil && (out.RootID != "" || len(out.Children) > 0) {
			return out.RootID, out.Children, nil
		}
		log.Printf("[豆包] aispace 根目录解析为空，尝试新链路")
	} else {
		log.Printf("[豆包] aispace 根目录失败，降级新链路：%v", aerr)
	}
	if d.v2Key != nil {
		if ch, e := d.dbnToList(""); e == nil {
			// 飞书根里混着几百个飞书文档（豆包文件和文档同一空间），
			// 浏览根只保留文件夹
			out := ch[:0]
			for _, n := range ch {
				if n.IsDir() && n.Name != "" {
					out = append(out, n)
				}
			}
			return "", out, nil
		} else {
			log.Printf("[豆包v2] 列根目录失败：%v", e)
		}
	}
	if aerr != nil {
		return "", nil, aerr
	}
	return "", nil, fmt.Errorf("豆包根目录结构异常：%s", dbSnippet(dbUnwrap(data)))
}

// dbPage 是目录接口的一页。

// dbUnwrap 豆包 2026-10 起把业务字段包进 data：{code,msg,data:{...}}。
// 老代码按顶层解析会拿到全空值（表现为"分享为空"/"没有可播放的转码流"），
// 这里统一剥一层；本来就是顶层结构时原样返回。
func dbUnwrap(raw json.RawMessage) json.RawMessage {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.Data) > 0 && string(probe.Data) != "null" {
		return probe.Data
	}
	return raw
}

type dbPage struct {
	Children   []dbNode `json:"children"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

// dbListDir 列自己盘某个目录（单页）。
// 数字 node_id 是老体系（自家盘 / 老格式分享）→ 直接走 aispace，拿 tos key，
// 取链落豆包自家 CDN；只有飞书 token（字母串）才走新链路。见 dbHome 注释。
func (d *Doubao) dbListDir(nodeID string) ([]dbNode, error) {
	if !dbIsOldNodeID(nodeID) && d.v2Key != nil {
		if ch, e := d.dbnToList(nodeID); e == nil {
			return ch, nil
		} else {
			log.Printf("[豆包v2] 列目录失败（token=%s），降级老链路：%v", nodeID, e)
		}
	}
	return d.dbListDirPage(nodeID, "")
}

func (d *Doubao) dbListDirPage(nodeID, cursor string) ([]dbNode, error) {
	body := map[string]any{"node_id": nodeID, "size": dbPageSize}
	if cursor != "" {
		body["cursor"] = cursor
	}
	data, err := d.post("/samantha/aispace/node_info", body)
	if err != nil {
		return nil, err
	}
	data = dbUnwrap(data)
	var out dbPage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("豆包目录结构异常：%s", dbSnippet(data))
	}
	return out.Children, nil
}

// dbListDirAll 列完整个目录（自动翻页）。
// 豆包单个目录超过一页时会给 has_more + next_cursor，不翻页就会漏文件。
//
// 中途失败**必须整单失败**，不能拿半截数据当完整结果 ——
// 那正是之前「扫描少了几千个文件」的元凶之一：前几页正常、第 4 页被风控，
// 代码 break 后把前 3 页交出去，看起来一切正常但少了 90% 的内容。
func (d *Doubao) dbListDirAll(nodeID string) ([]dbNode, error) {
	if !dbIsOldNodeID(nodeID) && d.v2Key != nil {
		if ch, e := d.dbnToList(nodeID); e == nil {
			return ch, nil
		} else {
			log.Printf("[豆包v2] 列目录失败（token=%s），降级老链路：%v", nodeID, e)
		}
	}
	var all []dbNode
	cursor := ""
	for page := 0; page < 200; page++ {
		body := map[string]any{"node_id": nodeID, "size": dbPageSize}
		if cursor != "" {
			body["cursor"] = cursor
		}
		data, err := d.post("/samantha/aispace/node_info", body)
		if err != nil {
			return nil, err
		}
		var out dbPage
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("豆包目录结构异常：%s", dbSnippet(data))
		}
		all = append(all, out.Children...)
		if !out.HasMore || out.NextCursor == "" || out.NextCursor == "-1" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

// ---- 别人的分享 ----

// dbShareOverview 取分享的顶层节点。
func (d *Doubao) dbShareOverview(shareID string) (title string, nodes []dbNode, err error) {
	data, err := d.post("/samantha/aispace/share/overview", map[string]any{"share_id": shareID})
	if err != nil {
		return "", nil, err
	}
	data = dbUnwrap(data)
	var out struct {
		Share *struct {
			ShareName string `json:"share_name"`
			Name      string `json:"name"`
		} `json:"share"`
		NodeList []dbNode `json:"node_list"`
		HasMore  bool     `json:"has_more"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", nil, fmt.Errorf("豆包分享结构异常：%s", dbSnippet(data))
	}
	if len(out.NodeList) == 0 {
		return "", nil, fmt.Errorf("豆包分享为空或已失效")
	}
	title = ""
	if out.Share != nil {
		title = out.Share.ShareName
		if title == "" {
			title = out.Share.Name
		}
	}
	if title == "" && len(out.NodeList) > 0 {
		title = out.NodeList[0].Name
	}
	return title, out.NodeList, nil
}

// dbShareListDir 列分享内某个目录（自动翻页）。中途失败同样整单失败。
func (d *Doubao) dbShareListDir(shareID, nodeID string) ([]dbNode, error) {
	var all []dbNode
	cursor := ""
	for page := 0; page < 200; page++ {
		body := map[string]any{"share_id": shareID, "node_id": nodeID, "need_full_path": false, "size": dbPageSize}
		if cursor != "" {
			body["cursor"] = cursor
		}
		data, err := d.post("/samantha/aispace/share/node_info", body)
		if err != nil {
			return nil, err
		}
		data = dbUnwrap(data)
		var out dbPage
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("豆包分享目录结构异常：%s", dbSnippet(data))
		}
		all = append(all, out.Children...)
		if !out.HasMore || out.NextCursor == "" || out.NextCursor == "-1" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

// ---- 取播放地址 ----

// dbPlayInfo 取某个文件的转码流列表。自己盘传 node_id，分享文件再带上 share_id。
func (d *Doubao) dbPlayInfo(nodeID, key, shareID string) ([]dbMedia, error) {
	body := map[string]any{"key": key, "node_id": nodeID}
	if shareID != "" {
		body["share_id"] = shareID
	}
	data, err := d.post("/samantha/media/get_play_info", body)
	if err != nil {
		return nil, err
	}
	data = dbUnwrap(data)
	var out struct {
		MediaInfo []dbMedia `json:"media_info"`
		MediaType string    `json:"media_type"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("豆包播放信息结构异常：%s", dbSnippet(data))
	}
	if len(out.MediaInfo) == 0 {
		return nil, fmt.Errorf("豆包没有可播放的转码流（可能仍在转码）")
	}
	return out.MediaInfo, nil
}

// dbPickURL 选清晰度最高的那一档，返回直链。
// 豆包 definition 是相对原片的标签（4K 源转出来的「1080p」实际宽度 2592），
// 所以按 width 排序而不是按标签字符串。
func dbPickURL(list []dbMedia) string {
	best := ""
	bestW := -1
	for i := range list {
		if list[i].MainURL == "" {
			continue
		}
		w, _ := strconv.Atoi(list[i].Meta.Width)
		if w > bestW {
			bestW = w
			best = list[i].MainURL
		}
	}
	return best
}
