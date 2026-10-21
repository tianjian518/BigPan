package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"

	tclient "bigpan/internal/tianyi/client"
)

// ---------------------------------------------------------------------------
// 天翼扫码登录
//
// 流程（cloudpan189-interface 封装好的三步）：
//   LoginInit  → 拿 uuid（它本身就是 open.e.189.cn 的二维码深链）
//   生成二维码 → 天翼 App「扫一扫」→ 手机上确认
//   LoginQuery → 轮询，确认后拿 accessToken + expiresIn
//
// 两个已知局限（底层库限制，认了）：
//   ① 轮询在「已扫码未确认」阶段与「未扫码」返回同一个错误
//      （QrCodeRollLoginFail），UI 只能显示「等待扫码」；
//   ② token 无刷新，过期只能重扫 —— 所以过期时间必须随凭证存下来。
// ---------------------------------------------------------------------------

type tianyiQRSession struct {
	id      string
	uuid    string
	status  int // 0 等待扫码 / 2 已确认 / -1 过期 / -2 失败
	message string
	done    bool
	err     string

	accessToken string
	expiresAt   int64 // 绝对过期时间（毫秒时间戳）
	label       string
	created     time.Time
}

var tianyiQRMu sync.Mutex
var tianyiQRSessions = map[string]*tianyiQRSession{}

func tianyiQRGC() {
	tianyiQRMu.Lock()
	defer tianyiQRMu.Unlock()
	for id, s := range tianyiQRSessions {
		if time.Since(s.created) > 10*time.Minute {
			delete(tianyiQRSessions, id)
		}
	}
}

// tianyiQRURL 二维码内容。
// uuid 本身就是 open.e.189.cn 的 qrClinentLogin 深链；外面再包一层
// cloud.189.cn 的 portal 入口（cloudpan189-share 生产验证过的格式，
// 两种形态都 302 到登录确认页）。若日后有用户反馈 App 扫了没反应，
// 换成裸 uuid 即可（一行的事）。
func tianyiQRURL(uuid string) string {
	return "https://cloud.189.cn/api/portal/loginUrl.action?redirectURL=https://cloud.189.cn&uuid=" + uuid
}

// startTianyiQR 发起一次天翼扫码会话，后台自动轮询。
func startTianyiQR() (*tianyiQRSession, error) {
	tianyiQRGC()
	info, err := tclient.LoginInit()
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败：%w", err)
	}
	s := &tianyiQRSession{
		id:      newID(),
		uuid:    info.UUID,
		status:  0,
		message: "等待扫码",
		created: time.Now(),
	}
	tianyiQRMu.Lock()
	tianyiQRSessions[s.id] = s
	tianyiQRMu.Unlock()
	go s.watch()
	return s, nil
}

// watch 后台轮询扫码状态（2s 一次，5 分钟过期）。
func (s *tianyiQRSession) watch() {
	deadline := time.Now().Add(5 * time.Minute)
	fails := 0
	for {
		time.Sleep(2 * time.Second)

		tianyiQRMu.Lock()
		if s.done {
			tianyiQRMu.Unlock()
			return
		}
		tianyiQRMu.Unlock()

		if time.Now().After(deadline) {
			tianyiQRMu.Lock()
			s.status, s.message, s.done = -1, "二维码已过期，请重新获取", true
			tianyiQRMu.Unlock()
			return
		}

		resp, err := tclient.LoginQuery(s.uuid)
		if err != nil {
			msg := err.Error()
			// 未扫码/未确认：底层就返回这一个错误码，当作「等待中」
			if strings.Contains(msg, "QrCodeRollLoginFail") {
				fails = 0
				continue
			}
			// 二维码失效类错误：直接标记过期，别让用户干等
			if strings.Contains(msg, "QrCodeInvalid") || strings.Contains(msg, "过期") ||
				strings.Contains(msg, "expire") || strings.Contains(msg, "Expire") {
				tianyiQRMu.Lock()
				s.status, s.message, s.done = -1, "二维码已失效，请重新获取", true
				tianyiQRMu.Unlock()
				return
			}
			fails++
			if fails >= 8 {
				tianyiQRMu.Lock()
				s.message = fmt.Sprintf("⚠️ 网络受限，正在重试（已失败 %d 次）", fails)
				tianyiQRMu.Unlock()
			}
			continue // 网络抖动，下轮再试
		}
		fails = 0

		if resp == nil || resp.AccessToken == "" {
			continue // 还没扫
		}

		// 扫码确认成功：换算过期时间，顺手验证 token 可用 + 拿账号标识
		expiresAt := tianyiExpiresAt(resp.ExpiresIn)
		label := "天翼账号"
		if name, err := tianyiFetchLoginName(resp.AccessToken, expiresAt); err == nil && name != "" {
			label = name
		} else if err != nil {
			// token 当场不可用（极端情况）：不落账，让用户重扫
			tianyiQRMu.Lock()
			s.err = fmt.Sprintf("登录态验证失败：%v", err)
			s.message, s.done = s.err, true
			tianyiQRMu.Unlock()
			return
		}
		tianyiQRMu.Lock()
		s.accessToken, s.expiresAt, s.label = resp.AccessToken, expiresAt, label
		s.status, s.message, s.done = 2, "登录成功", true
		tianyiQRMu.Unlock()
		return
	}
}

// tianyiExpiresAt 把登录返回的 ExpiresIn 换算成绝对过期时间（毫秒时间戳）。
// 天翼这个字段语义不定：可能是剩余秒数，也可能是绝对毫秒时间戳
// （cloudpan189-share 用 >1e10 区分，照做）；拿不到按 7 天兜底。
func tianyiExpiresAt(expiresIn int64) int64 {
	now := time.Now().UnixMilli()
	if expiresIn <= 0 {
		return now + 7*24*3600*1000
	}
	if expiresIn > 1e10 {
		return expiresIn // 已是毫秒时间戳
	}
	return now + expiresIn*1000
}

// tianyiFetchLoginName 用 token 调一次 GetUserInfo：验证凭证可用 + 拿登录名
// （一般是手机号，用作账号标签）。
func tianyiFetchLoginName(accessToken string, expiresAt int64) (string, error) {
	cred := &tianyiCred{AccessToken: accessToken, ExpiresMs: expiresAt}
	var resp *tclient.GetUserInfoResponse
	err := tianyiDo(cred, func(cl tclient.Client) error {
		r, e := cl.GetUserInfo(context.Background())
		resp = r
		return e
	})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.ResCode != 0 {
		return "", fmt.Errorf("凭证验证失败（res_code=%v）", resp.ResCode)
	}
	if resp.LoginName == "" {
		return "天翼账号", nil
	}
	return resp.LoginName, nil
}

// tianyiQRSnapshot 扫码会话状态快照（避免数据竞争）。
type tianyiQRSnapshot struct {
	Status      int
	Message     string
	Done        bool
	AccessToken string
	ExpiresAt   int64
	Label       string
	Err         string
}

func pollTianyiQR(id string) (*tianyiQRSnapshot, error) {
	tianyiQRMu.Lock()
	defer tianyiQRMu.Unlock()
	s, ok := tianyiQRSessions[id]
	if !ok {
		return nil, fmt.Errorf("会话不存在或已过期")
	}
	return &tianyiQRSnapshot{
		Status:      s.status,
		Message:     s.message,
		Done:        s.done,
		AccessToken: s.accessToken,
		ExpiresAt:   s.expiresAt,
		Label:       s.label,
		Err:         s.err,
	}, nil
}

// tianyiResetClients 丢掉所有缓存的 API 门面（换号/退号时调，
// 旧 token 的客户端不能复用）。
func tianyiResetClients() {
	tianyiAPIMu.Lock()
	tianyiAPICache = map[string]*tianyiAPI{}
	tianyiAPIMu.Unlock()
}

// apiLoginStartTianyi 开始扫码：POST /api/login/tianyi/start
func apiLoginStartTianyi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s, err := startTianyiQR()
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	png, err := qrcode.Encode(tianyiQRURL(s.uuid), qrcode.Medium, 256)
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

// apiLoginPollTianyi 轮询扫码状态：GET /api/login/tianyi/poll?sid=xxx
func apiLoginPollTianyi(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("sid")
		snap, err := pollTianyiQR(sid)
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
		if snap.Done && snap.Status == 2 && snap.AccessToken != "" {
			installAccount(as, Account{
				Provider:     "tianyi",
				Label:        snap.Label,
				Cookie:       snap.AccessToken, // 单账号模型：Cookie 字段复用存 accessToken
				TokenExpires: snap.ExpiresAt,
				LoginAt:      time.Now(),
			})
			resp["label"] = snap.Label
			log.Printf("[天翼] 扫码登录成功（%s），token 过期时间 %s",
				snap.Label, time.UnixMilli(snap.ExpiresAt).Format("2006-01-02 15:04"))
		}
		writeJSON(w, resp)
	}
}
