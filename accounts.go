package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	driver115 "github.com/SheltonZhu/115driver/pkg/driver"
)

// ---------------------------------------------------------------------------
// 账号凭证存储
// ---------------------------------------------------------------------------

// Account 是一个网盘账号的登录凭证。
//
// 单账号模型：每个网盘只保存**当前登录**的那一个账号。
// 换号 = 退出旧账号 → 导入新账号，凭证整体替换，不需要重启、不需要改配置。
type Account struct {
	Provider string    `json:"provider"` // 网盘标识：115 / aliyun / baidu ...
	Label    string    `json:"label"`    // 显示名称（昵称或 ID）
	Cookie   string    `json:"cookie"`   // 凭证字符串
	LoginAt  time.Time `json:"login_at"`

	// —— 夸克 TV（provider="quark_tv"）专用字段 ——
	// 单账号模型下 Cookie 字段复用：TV 账号的 Cookie 存 refresh_token（长期有效）。
	// access_token 是短命凭证，这里存一份避免每次播放都刷新；播放中 token 被
	// 刷新后由客户端回写（见 quark_tv.go 的 onRefresh）。
	DeviceID    string `json:"device_id,omitempty"`
	AccessToken string `json:"access_token,omitempty"`

	// —— 天翼云盘（provider="tianyi"）专用字段 ——
	// 单账号模型下 Cookie 字段复用：存扫码拿到的 accessToken。
	// 天翼 token 无刷新机制，过期只能重扫码，所以要把过期时间一并存下。
	TokenExpires int64 `json:"token_expires,omitempty"` // 绝对过期时间（毫秒时间戳）
}

// AccountStore 账号持久化（JSON 文件）。
type AccountStore struct {
	mu   sync.RWMutex
	path string
	list []Account
}

func NewAccountStore(path string) (*AccountStore, error) {
	s := &AccountStore{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.list)
	}
	return s, nil
}

func (s *AccountStore) save() {
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, b, 0600) // 凭证文件仅属主可读
}

func (s *AccountStore) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, len(s.list))
	copy(out, s.list)
	return out
}

func (s *AccountStore) Get(provider string) *Account {
	if s == nil { // 账号库未就绪（理论只发生在启动前的测试里），视为未登录
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.list {
		if s.list[i].Provider == provider {
			a := s.list[i]
			return &a
		}
	}
	return nil
}

func (s *AccountStore) Put(a Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].Provider == a.Provider {
			s.list[i] = a
			s.save()
			return
		}
	}
	s.list = append(s.list, a)
	s.save()
}

func (s *AccountStore) Delete(provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].Provider == provider {
			s.list = append(s.list[:i], s.list[i+1:]...)
			s.save()
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 换号：退出旧账号 / 导入新账号（全自动收尾）
// ---------------------------------------------------------------------------

// retireOldAccount 旧账号退场收尾：
//   - 丢弃它签发的全部播放直链（换号后必须重新解析，绝不能复用旧号的链接）
//   - 百度额外清理旧号盘里的临时转存文件（只清 30 分钟前的，不打断正在播的）
func retireOldAccount(old Account) {
	dropAllPlayTargets()
	log.Printf("[账号] %s 旧凭证已停用（%s），播放直链缓存已清空", providerCN(old.Provider), old.Label)
	// 夸克临时目录 fid 是「旧号盘里」的目录，换号/退号后必须重新确认
	// （新号的盘里没有 /bigpan_tmp/quark，复用旧 fid 会转存失败）。
	if old.Provider == "quark" || old.Provider == "quark_tv" {
		quarkTmpDirReset()
		quarkTempReset()
		quarkFailCacheReset()
		quarkLinkReset() // 302 直链短缓存也绑账号，一起清
	}
	if old.Provider == "uc" || old.Provider == "uc_tv" {
		ucTempReset() // 新号的盘里没有 /bigpan_tmp/uc，旧 fid/记账都要丢
	}
	if old.Provider == "baidu" && old.Cookie != "" {
		// 【防崩】一次性后台任务：panic 只记日志，不影响主流程。
		ck, label := old.Cookie, old.Label
		goSafe("旧百度账号清理", func() {
			time.Sleep(10 * time.Second) // 给正在播放的请求一点缓冲
			baiduPurgeOrphans(ck, 30*time.Minute)
			log.Printf("[账号] 旧百度账号（%s）的临时转存文件已在后台清理", label)
		})
	}
	if old.Provider == "tianyi" {
		tianyiResetClients() // 旧 token 的 API 门面不能复用
	}
}

// installAccount 写入账号凭证（首次导入或换号）。
//
// 换号时自动做两件事，用户只需要「退出 → 导入新号」，其余全自动：
//  ① 旧凭证的播放直链全部作废（否则换号后还会拿到旧号的链接）
//  ② 旧百度账号的临时转存文件后台清理（否则会永远占着旧号的空间）
func installAccount(as *AccountStore, a Account) {
	if old := as.Get(a.Provider); old != nil && old.Cookie != "" && old.Cookie != a.Cookie {
		retireOldAccount(*old)
	}
	as.Put(a)
	dropAllPlayTargets() // 新凭证立即生效
}

// uninstallAccount 退出登录：删凭证 + 收尾。
func uninstallAccount(as *AccountStore, provider string) bool {
	old := as.Get(provider)
	if !as.Delete(provider) {
		return false
	}
	if old != nil {
		retireOldAccount(*old)
	} else {
		dropAllPlayTargets()
	}
	return true
}

// ---------------------------------------------------------------------------
// 115 网盘客户端
// ---------------------------------------------------------------------------

// Pan115 封装 115 的登录态与客户端。
type Pan115 struct {
	mu     sync.Mutex
	cl     *driver115.Pan115Client
	cookie string
}

// client 返回一个已登录的 115 客户端；cookie 变化时自动重建。
func (p *Pan115) client(cookie string) (*driver115.Pan115Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cl != nil && p.cookie == cookie {
		return p.cl, nil
	}
	cl := driver115.New(driver115.UA(driver115.UA115Browser))
	cr := &driver115.Credential{}
	if err := cr.FromCookie(cookie); err != nil {
		return nil, fmt.Errorf("Cookie 格式不正确: %w", err)
	}
	cl.ImportCredential(cr)
	// 用 CookieCheck 而非 LoginCheck：前者不会把其他设备挤下线
	if err := cl.CookieCheck(); err != nil {
		return nil, fmt.Errorf("登录态已失效，请重新扫码: %w", err)
	}
	p.cl, p.cookie = cl, cookie
	return cl, nil
}

// ---------------------------------------------------------------------------
// 扫码登录会话
// ---------------------------------------------------------------------------

type qrSession struct {
	id      string
	cl      *driver115.Pan115Client
	sess    *driver115.QRCodeSession
	status  int // 0 等待扫码 / 1 已扫码待确认 / 2 已确认 / -1 过期 / -2 取消
	message string
	cookie  string
	label   string
	done    bool
	err     string
	created time.Time
}

var qrMu sync.Mutex
var qrSessions = map[string]*qrSession{}

func qrGC() {
	qrMu.Lock()
	defer qrMu.Unlock()
	for id, s := range qrSessions {
		if time.Since(s.created) > 10*time.Minute {
			delete(qrSessions, id)
		}
	}
	// 夸克 TV 扫码会话（独立锁+独立 map，10 分钟同策略）
	qtvQRGC()
}

// startQR115 发起一次 115 扫码会话，并在后台自动轮询状态。
func startQR115() (*qrSession, error) {
	qrGC()
	cl := driver115.New(driver115.UA(driver115.UA115Browser))
	cl.Client.SetTimeout(15 * time.Second) // 避免个别请求挂死
	sess, err := cl.QRCodeStart()
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败: %w", err)
	}
	s := &qrSession{
		id:      newID(),
		cl:      cl,
		sess:    sess,
		status:  0,
		message: "等待扫码",
		created: time.Now(),
	}
	qrMu.Lock()
	qrSessions[s.id] = s
	qrMu.Unlock()
	// 【防崩】轮询循环 panic 会让这次扫码静静失败，用户只会看到二维码不刷新。
	goSafe("115扫码轮询", s.watch)
	return s, nil
}

// watch 在后台轮询 115 的扫码状态，避免前端轮询阻塞。
func (s *qrSession) watch() {
	deadline := time.Now().Add(5 * time.Minute)
	fails := 0
	for {
		time.Sleep(2 * time.Second)

		qrMu.Lock()
		if s.done {
			qrMu.Unlock()
			return
		}
		qrMu.Unlock()

		if time.Now().After(deadline) {
			qrMu.Lock()
			s.status, s.message, s.done = -1, "二维码已过期，请重新获取", true
			qrMu.Unlock()
			return
		}

		st, err := s.cl.QRCodeStatus(s.sess)
		if err != nil {
			fails++
			// 连续失败较多时，多半是本机网络对 115 的接口不友好
			if fails >= 8 {
				qrMu.Lock()
				s.message = fmt.Sprintf("⚠️ 网络受限，正在重试（已失败 %d 次）", fails)
				qrMu.Unlock()
			}
			continue // 网络抖动，下轮再试
		}
		fails = 0

		qrMu.Lock()
		switch {
		case st.IsWaiting():
			s.status, s.message = 0, "等待扫码"
		case st.IsScanned():
			s.status, s.message = 1, "已扫码，请在手机上确认"
		case st.IsAllowed():
			cr, err := s.cl.QRCodeLogin(s.sess)
			if err != nil {
				s.err = fmt.Sprintf("登录失败: %v", err)
				s.message, s.done = s.err, true
			} else {
				s.cookie = cr.Cookie()
				s.label = "115 账号"
				s.status, s.message, s.done = 2, "登录成功", true
			}
		case st.IsExpired():
			s.status, s.message, s.done = -1, "二维码已过期，请重新获取", true
		case st.IsCanceled():
			s.status, s.message, s.done = -2, "已取消登录", true
		default:
			s.message = st.Msg
		}
		qrMu.Unlock()

		if s.done {
			return
		}
	}
}

// qrSnapshot 是扫码会话状态的只读快照（避免数据竞争）。
type qrSnapshot struct {
	Status  int
	Message string
	Done    bool
	Cookie  string
	Label   string
	Err     string
}

// pollQR115 读取扫码会话的当前状态（纯内存读取，不发起网络请求）。
func pollQR115(id string) (*qrSnapshot, error) {
	qrMu.Lock()
	defer qrMu.Unlock()
	s, ok := qrSessions[id]
	if !ok {
		return nil, fmt.Errorf("会话不存在或已过期")
	}
	return &qrSnapshot{
		Status:  s.status,
		Message: s.message,
		Done:    s.done,
		Cookie:  s.cookie,
		Label:   s.label,
		Err:     s.err,
	}, nil
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// apiLoginStart 开始扫码：POST /api/login/115/start
func apiLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}
	s, err := startQR115()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	png, err := s.sess.QRCode()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "生成二维码失败"})
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":  true,
		"sid": s.id,
		"qr":  "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

// apiLoginPoll 轮询扫码状态：GET /api/login/115/poll?sid=xxx
func apiLoginPoll(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("sid")
		snap, err := pollQR115(sid)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		resp := map[string]interface{}{
			"ok":      true,
			"status":  snap.Status,
			"message": snap.Message,
			"done":    snap.Done,
		}
		if snap.Err != "" {
			resp["error"] = snap.Err
		}
		if snap.Done && snap.Cookie != "" {
			// 落盘前先验证凭证确实可用
			p := &Pan115{}
			if _, err := p.client(snap.Cookie); err != nil {
				resp["error"] = err.Error()
			} else {
				installAccount(as, Account{
					Provider: "115",
					Label:    snap.Label,
					Cookie:   snap.Cookie,
					LoginAt:  time.Now(),
				})
				resp["label"] = snap.Label
			}
		}
		writeJSON(w, resp)
	}
}

// apiAccountImport 手动导入凭证（兜底）：POST /api/accounts/import
// body: {"provider":"115","cookie":"UID=..;CID=..;SEID=..;KID=.."}
func apiAccountImport(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			Cookie   string `json:"cookie"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
			return
		}
		req.Provider = strings.TrimSpace(req.Provider)
		req.Cookie = strings.TrimSpace(req.Cookie)
		if req.Provider == "" || req.Cookie == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "凭证不能为空"})
			return
		}

		// —— 豆包多账号：doubao1..doubaoN，一个槽位一个账号 ——
		// 只认 sessionid（30 天有效），整段 Cookie 直接存也不影响。
		if strings.HasPrefix(req.Provider, "doubao") {
			cli := newDoubao(req.Cookie)
			_, children, err := cli.dbHome()
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "Cookie 验证失败：" + err.Error()})
				return
			}
			installAccount(as, Account{
				Provider: req.Provider,
				Label:    providerCN(req.Provider) + "（" + strconv.Itoa(len(children)) + " 个顶层目录）",
				Cookie:   req.Cookie,
				LoginAt:  time.Now(),
			})
			resetDoubaoClients()
			writeJSON(w, map[string]interface{}{"ok": true, "detail": "已连上，根目录 " + strconv.Itoa(len(children)) + " 项"})
			return
		}

		switch req.Provider {
		case "115":
			p := &Pan115{}
			if _, err := p.client(req.Cookie); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			installAccount(as, Account{
				Provider: "115",
				Label:    "115 账号（手动导入）",
				Cookie:   req.Cookie,
				LoginAt:  time.Now(),
			})
		case "mobile139":
			// 移动云盘：网页版 Authorization 里的 token（裸串或带 Basic 前缀均可）。
			// 校验能解出手机号，Label 存手机号方便辨认。
			tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Cookie), "Basic "))
			account := ""
			if raw, err := base64.StdEncoding.DecodeString(tok); err == nil {
				for _, p := range strings.Split(string(raw), ":") {
					if len(p) == 11 && strings.HasPrefix(p, "1") {
						account = p
						break
					}
				}
			}
			if account == "" {
				writeJSON(w, map[string]interface{}{
					"ok":    false,
					"error": "token 无法识别出手机号 —— 请复制网页请求头 Authorization 里 Basic 后面那串",
				})
				return
			}
			installAccount(as, Account{
				Provider: "mobile139",
				Label:    account,
				Cookie:   tok,
				LoginAt:  time.Now(),
			})
		case "baidu":
			// 百度网盘：支持两种粘贴 ——
			//  1) 裸 BDUSS 值
			//  2) 整段 Cookie（原样保留，含 STOKEN 等其它 cookie —— 播放取流要用）
			raw := strings.TrimSpace(req.Cookie)
			if raw == "" {
				writeJSON(w, map[string]interface{}{
					"ok":    false,
					"error": "请粘贴 BDUSS，或整段 Cookie（登录 pan.baidu.com 后 F12→网络→复制任一请求的整串 Cookie）",
				})
				return
			}
			acc := baiduCookie(raw) // 裸值 → BDUSS=xxx；整段 → 原样保留
			label, err := baiduCheck(acc)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			installAccount(as, Account{
				Provider: "baidu",
				Label:    label,
				Cookie:   acc,
				LoginAt:  time.Now(),
			})
		default:
			writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": "暂不支持该网盘的手动导入（目前支持：115 / 移动云盘 / 百度）",
			})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	}
}

// apiAccounts 账号列表 / 删除
func apiAccounts(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 路径形如 /api/accounts 或 /api/accounts/115
		rest := r.URL.Path[len("/api/accounts"):]
		switch {
		case rest == "" || rest == "/":
			if r.Method != http.MethodGet {
				http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
				return
			}
			// 不返回 Cookie 明文，只返回概要
			type view struct {
				Provider string    `json:"provider"`
				Label    string    `json:"label"`
				LoginAt  time.Time `json:"login_at"`
			}
			list := as.List()
			out := make([]view, 0, len(list))
			for _, a := range list {
				out = append(out, view{a.Provider, a.Label, a.LoginAt})
			}
			writeJSON(w, map[string]interface{}{"ok": true, "accounts": out})
		default:
			if r.Method != http.MethodDelete {
				http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
				return
			}
			prov := rest[1:]
			ok := uninstallAccount(as, prov)
			// 夸克是「一个入口、两种凭证」：Cookie（quark）与扫码（quark_tv）
			// 一起退，避免用户退了一种以为全退了，前端也说不清哪个还活着。
			if prov == "quark" && uninstallAccount(as, "quark_tv") {
				ok = true
			}
			if ok {
				writeJSON(w, map[string]interface{}{
					"ok":   true,
					"note": "已退出。播放直链缓存已清空，导入新账号后立即生效。",
				})
			} else {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "账号不存在"})
			}
		}
	}
}
