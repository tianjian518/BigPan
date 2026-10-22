package main

// ---------------------------------------------------------------------------
// 夸克 TV 扫码登录：会话管理 + HTTP 接口 + 客户端工厂
//
// 与 115/百度的扫码共用同一套交互模式（后台 watch 轮询 + 前端 poll 快照），
// 但 session 结构独立（115 的 qrSession 绑死了 115 driver 类型）。
// 账号以 provider="quark_tv" 存入 AccountStore，与 "quark"（PC cookie）并存。
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// qtvQRSession 一次夸克 TV 扫码会话。
type qtvQRSession struct {
	id          string
	pv          string // 账号库 provider：quark_tv / uc_tv
	qtv         *QuarkTV
	queryToken  string
	status      int // 0 等待扫码 / 1 已扫码待确认 / 2 已确认 / -1 过期 / -2 失败
	message     string
	refreshToken string
	deviceID    string
	label       string
	done        bool
	err         string
	created     time.Time
}

var (
	qtvQRMu       sync.Mutex
	qtvQRSessions = map[string]*qtvQRSession{}
)

// qtvQRGC 清理超过 10 分钟的会话（与 115 的 qrGC 同策略，在 qrGC 里一并调用）。
func qtvQRGC() {
	qtvQRMu.Lock()
	defer qtvQRMu.Unlock()
	for id, s := range qtvQRSessions {
		if time.Since(s.created) > 10*time.Minute {
			delete(qtvQRSessions, id)
		}
	}
}

// startQRQuarkTV 发起一次夸克 TV 扫码会话，后台自动轮询。
//
// 设备号复用策略（2026-10-20「设备数超限」事故的修复）：
// 反复扫码如果每次都用随机新设备号，会把夸克账号的电视设备名额吃光，
// 之后所有 TV 取链都报「设备数超限」。所以：
//   - 已有 quark_tv 账号 → 复用账号里的设备号（重扫 = 刷新同一设备，不占新名额）；
//   - 首次登录 → 生成一个持久化到数据目录的设备号（容器重启/重建都不变）。
func startQRQuarkTV(as *AccountStore) (*qtvQRSession, error) {
	return startQRTV(as, NewQuarkTV(qtvStableDeviceID(as), "", ""), "quark_tv")
}

// startQRUC 发起 UC TV 扫码会话（与夸克共用同一套会话/轮询机制）。
func startQRUC(as *AccountStore) (*qtvQRSession, error) {
	return startQRTV(as, NewUCTV(ucStableDeviceID(as), "", ""), "uc_tv")
}

// startQRTV 通用扫码会话：客户端由调用方构造，pv 决定落库到哪个 provider。
func startQRTV(as *AccountStore, qtv *QuarkTV, pv string) (*qtvQRSession, error) {
	qtvQRGC()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	qrData, queryToken, err := qtv.QRStart(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败：%w", err)
	}
	// 二维码图是 base64 JPEG，前端 <img src="data:image/jpeg;base64,..."> 直接用；
	// 这里统一补成 data URI，和 115/百度的返回形态保持一致。
	qrURI := "data:image/jpeg;base64," + qrData
	s := &qtvQRSession{
		id:         newID(),
		pv:         pv,
		qtv:        qtv,
		queryToken: queryToken,
		status:     0,
		message:    "等待扫码",
		created:    time.Now(),
	}
	// 二维码图放 message 里带走（前端从快照的 qr 字段取），见 pollQRQuarkTV。
	s.label = qrURI
	qtvQRMu.Lock()
	qtvQRSessions[s.id] = s
	qtvQRMu.Unlock()
	// 【防崩】同上：轮询 panic 会让扫码静静失败。
	goSafe("夸克TV扫码轮询", s.watch)
	return s, nil
}

// watch 后台轮询扫码状态（2 秒一轮，5 分钟超时）。
func (s *qtvQRSession) watch() {
	deadline := time.Now().Add(5 * time.Minute)
	fails := 0
	for {
		time.Sleep(2 * time.Second)

		qtvQRMu.Lock()
		if s.done {
			qtvQRMu.Unlock()
			return
		}
		qtvQRMu.Unlock()

		if time.Now().After(deadline) {
			qtvQRMu.Lock()
			s.status, s.message, s.done = -1, "二维码已过期，请重新获取", true
			qtvQRMu.Unlock()
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		code, err := s.qtv.QRCode(ctx, s.queryToken)
		cancel()
		if err != nil {
			fails++
			if fails >= 8 {
				qtvQRMu.Lock()
				s.message = fmt.Sprintf("⚠️ 网络受限，正在重试（已失败 %d 次）", fails)
				qtvQRMu.Unlock()
			}
			continue
		}
		fails = 0

		if code == "" {
			// 未扫码或已扫码未确认：保持等待态（TV 接口不区分这两种状态，
			// 统一提示等待，确认前用户看到的就是「等待扫码」）。
			continue
		}

		// 已确认：换 token
		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		refreshToken, err := s.qtv.LoginWithCode(ctx2, code)
		cancel2()
		qtvQRMu.Lock()
		if err != nil {
			s.err = fmt.Sprintf("登录失败: %v", err)
			s.message, s.status, s.done = s.err, -2, true
		} else {
			s.refreshToken = refreshToken
			s.deviceID = s.qtv.DeviceID()
			s.status, s.message, s.done = 2, "登录成功", true
		}
		qtvQRMu.Unlock()
		return
	}
}

// qtvQRSnapshot 扫码会话快照（字段与 115 的 qrSnapshot 对齐，外加 qr 图）。
type qtvQRSnapshot struct {
	Provider     string
	Status       int
	Message      string
	Done         bool
	QR           string // data URI，仅在 start 时由调用方一并取走
	RefreshToken string
	DeviceID     string
	Label        string
	Err          string
}

// pollQRQuarkTV 读取会话快照（纯内存，不发请求）。
func pollQRQuarkTV(id string) (*qtvQRSnapshot, error) {
	qtvQRMu.Lock()
	defer qtvQRMu.Unlock()
	s, ok := qtvQRSessions[id]
	if !ok {
		return nil, fmt.Errorf("会话不存在或已过期")
	}
	return &qtvQRSnapshot{
		Provider:     s.pv,
		Status:       s.status,
		Message:      s.message,
		Done:         s.done,
		QR:           s.label, // start 时暂存在 label 里（见 startQRQuarkTV）
		RefreshToken: s.refreshToken,
		DeviceID:     s.deviceID,
		Err:          s.err,
	}, nil
}

// qtvStableDeviceID 取一个稳定的夸克 TV 设备号。
// 优先复用已登录账号的设备号；没有账号时用数据目录里持久化的那个
// （首次生成后写盘，之后重启/重扫/容器重建都读同一个，保证一个部署
// 只占夸克账号一个电视设备名额）。
func qtvStableDeviceID(as *AccountStore) string {
	return qtvStableDeviceIDFor(as, "quark_tv", "qtv_device_id")
}

// qtvStableDeviceIDFor 按 provider 取稳定设备号（文件按 provider 分开存）。
func qtvStableDeviceIDFor(as *AccountStore, provider, filename string) string {
	if a := as.Get(provider); a != nil && a.DeviceID != "" {
		return a.DeviceID
	}
	p := filepath.Join(filepath.Dir(as.path), filename)
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 32 {
			return id
		}
	}
	id := qtvGenDeviceID()
	_ = os.WriteFile(p, []byte(id), 0600)
	return id
}

// apiLoginStartQuark 开始扫码：POST /api/login/quark/start
func apiLoginStartQuark(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		s, err := startQRQuarkTV(as)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":  true,
			"sid": s.id,
			"qr":  s.label,
			"ttl": 300,
		})
	}
}

// apiLoginPollQuark 轮询扫码状态：GET /api/login/quark/poll?sid=xxx
func apiLoginPollQuark(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("sid")
		snap, err := pollQRQuarkTV(sid)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		resp := map[string]interface{}{
			"ok":      true,
			"status":  snap.Status,
			"message": snap.Message,
			"done":    snap.Done,
			"qr":      snap.QR,
		}
		if snap.Err != "" {
			resp["error"] = snap.Err
		}
		if snap.Done && snap.RefreshToken != "" {
			// 落盘前先验证：用新 refresh_token 换一次 access_token 并探活
			var qtv *QuarkTV
			label := "夸克TV（扫码登录）"
			if snap.Provider == "uc_tv" {
				qtv = NewUCTV(snap.DeviceID, "", snap.RefreshToken)
				label = "UC TV（扫码登录）"
			} else {
				qtv = NewQuarkTV(snap.DeviceID, "", snap.RefreshToken)
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			verr := qtv.RefreshToken(ctx)
			cancel()
			if verr != nil {
				resp["error"] = "凭证验证失败: " + verr.Error()
			} else {
				installAccount(as, Account{
					Provider:    snap.Provider,
					Label:       label,
					Cookie:      snap.RefreshToken, // 单账号模型：Cookie 字段复用存 refresh_token
					DeviceID:    snap.DeviceID,
					AccessToken: qtv.accessTokenOrEmpty(),
					LoginAt:     time.Now(),
				})
			}
		}
		writeJSON(w, resp)
	}
}

// accessTokenOrEmpty 取当前 access_token（探活用，落库那份）。
func (q *QuarkTV) accessTokenOrEmpty() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.accessToken
}

// quarkTVClient 从账号库构造夸克 TV 客户端；未登录返回 nil。
// token 刷新后自动回写账号库（避免容器重启后丢掉新 token）。
func quarkTVClient(as *AccountStore) *QuarkTV {
	a := as.Get("quark_tv")
	if a == nil || a.Cookie == "" { // Cookie 字段存的是 refresh_token
		return nil
	}
	// 老账号可能没有设备号字段：补一个稳定设备号并回写，避免每次重启
	// 随机生成新设备号在夸克账号侧堆积名额（2026-10-20 事故的同款隐患）。
	deviceID := a.DeviceID
	if deviceID == "" {
		deviceID = qtvStableDeviceID(as)
		a.DeviceID = deviceID
		as.Put(*a)
	}
	q := NewQuarkTV(deviceID, a.AccessToken, a.Cookie)
	q.onRefresh = func(accessToken, refreshToken string) {
		cur := as.Get("quark_tv")
		if cur == nil {
			return
		}
		cur.AccessToken = accessToken
		cur.Cookie = refreshToken
		as.Put(*cur)
	}
	return q
}
