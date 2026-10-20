package main

// ---------------------------------------------------------------------------
// 夸克 TV 版客户端（与 OpenList drivers/quark_uc_tv 同源的逆向 TV 接口）
//
// 为什么需要它：夸克 PC 接口的直链绑登录 Cookie，302 会被拒（strm.go 铁律），
// 只能本机中转；而 TV 版接口返回的直链不带任何认证头，可以直接 302 跳给
// 播放器，数据不经过本机 —— 这就是「夸克 TV 302」的由来。
//
// 接口形态全部来自 OpenList 驱动源码（drivers/quark_uc_tv）+ 沙箱实测：
//   - 授权接口 /oauth/authorize 已实测通（返回真二维码），签名算法复刻无误；
//   - 登录 = TV 版 OAuth2 二维码流；取链 = /file?method=download|streaming；
//   - 写操作（转存等）TV 接口一律不支持，转存仍走 PC 接口（见 quark_save.go）。
//
// 官方文档明示：「TV 版支持 302，但是只能访问和下载两个操作」。
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// qtvClientID / qtvSignKey TV 端 App 的客户端标识与签名密钥（驱动源码常量）。
	qtvClientID = "d3194e61504e493eb6222857bccfed94"
	qtvSignKey  = "kw2dvtd7p4t3pjl2d9ed9yc8yej8kw2d"
	qtvAppVer   = "1.8.2.2"
	qtvChannel  = "GENERAL"
	// qtvUA 伪装成小米电视（驱动源码同款；只用于 TV 接口请求，直链本身不校验 UA）。
	qtvUA = "Mozilla/5.0 (Linux; U; Android 13; zh-cn; M2004J7AC Build/UKQ1.231108.001) AppleWebKit/533.1 (KHTML, like Gecko) Mobile Safari/533.1"
)

// qtvAPIBase TV 开放接口域名（QuarkTV；UC TV 变体是 open-api-drive.uc.cn，暂不接）。
// 声明成 var 而非 const：单测里要指向 httptest 服务器（同 qtvCodeAPI）。
var qtvAPIBase = "https://open-api-drive.quark.cn"

// qtvCodeAPI 扫码登录换 token 的域名（驱动同款：api.extscreen.com/quarkdrive）。
// 声明成 var 而非 const：单测里要指向 httptest 服务器。
var qtvCodeAPI = "http://api.extscreen.com/quarkdrive"

// 设备参数（驱动源码同款，固定值，伪装 M2004J7AC 电视）。
const (
	qtvDeviceBrand  = "Xiaomi"
	qtvPlatform     = "tv"
	qtvDeviceName   = "M2004J7AC"
	qtvDeviceModel  = "M2004J7AC"
	qtvBuildDevice  = "M2004J7AC"
	qtvBuildProduct = "M2004J7AC"
	qtvDeviceGpu    = "Adreno (TM) 550"
)

// QuarkTV 电视端客户端：持有一台「电视设备」的登录态。
// 与 PC 版 Quark（cookie）完全独立，两套凭证互不影响。
type QuarkTV struct {
	mu           sync.Mutex
	deviceID     string
	accessToken  string
	refreshToken string
	hc           *http.Client
	// onRefresh token 刷新成功后回调（供外层回写 AccountStore，避免重启后丢新 token）。
	onRefresh func(accessToken, refreshToken string)
}

// NewQuarkTV 用已保存的凭证构造客户端；凭证为空时只影响需要登录的调用
// （扫码登录流程本身不需要 access_token）。
func NewQuarkTV(deviceID, accessToken, refreshToken string) *QuarkTV {
	if deviceID == "" {
		deviceID = qtvGenDeviceID()
	}
	return &QuarkTV{
		deviceID:     deviceID,
		accessToken:  accessToken,
		refreshToken: refreshToken,
		hc:           &http.Client{Timeout: 20 * time.Second},
	}
}

// qtvGenDeviceID 生成一个设备 ID（驱动：md5(时间戳字符串)）。
func qtvGenDeviceID() string {
	sum := md5.Sum([]byte(time.Now().String()))
	return hex.EncodeToString(sum[:])
}

// DeviceID 返回设备 ID（入库用，签名与设备名额都绑它）。
func (q *QuarkTV) DeviceID() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.deviceID
}

// qtvSign 复刻驱动 generateReqSign：
//
//	x-pan-token = sha256(方法&路径&13位毫秒时间戳&signKey)
//	req_id      = md5(deviceID + timestamp)
//
// 算法已用 /oauth/authorize 实测验证（返回 HTTP 200 + 真二维码）。
func (q *QuarkTV) qtvSign(method, pathname string) (tm, token, reqID string) {
	timestamp := strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10)
	tokenData := method + "&" + pathname + "&" + timestamp + "&" + qtvSignKey
	sum := sha256.Sum256([]byte(tokenData))
	rid := md5.Sum([]byte(q.deviceID + timestamp))
	return timestamp, hex.EncodeToString(sum[:]), hex.EncodeToString(rid[:])
}

// qtvDeviceParams 所有 TV 接口都要带的一整套设备参数（驱动源码同款）。
func (q *QuarkTV) qtvDeviceParams(reqID, accessToken string) url.Values {
	v := url.Values{}
	v.Set("req_id", reqID)
	v.Set("access_token", accessToken)
	v.Set("app_ver", qtvAppVer)
	v.Set("device_id", q.deviceID)
	v.Set("device_brand", qtvDeviceBrand)
	v.Set("platform", qtvPlatform)
	v.Set("device_name", qtvDeviceName)
	v.Set("device_model", qtvDeviceModel)
	v.Set("build_device", qtvBuildDevice)
	v.Set("build_product", qtvBuildProduct)
	v.Set("device_gpu", qtvDeviceGpu)
	v.Set("activity_rect", "{}")
	v.Set("channel", qtvChannel)
	return v
}

// qtvRequest 发一次带签名的 TV 接口请求。access_token 放在 query 里
// （驱动同款：access_token 是请求参数，不是请求头）。
// 只做网络层与 JSON 解析，业务错误码由调用方判断。
func (q *QuarkTV) qtvRequest(ctx context.Context, method, pathname string, params map[string]string, out interface{}) error {
	tm, token, reqID := q.qtvSign(method, pathname)
	q.mu.Lock()
	accessToken := q.accessToken
	q.mu.Unlock()

	qv := q.qtvDeviceParams(reqID, accessToken)
	for k, v := range params {
		qv.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, method, qtvAPIBase+pathname+"?"+qv.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", qtvUA)
	req.Header.Set("x-pan-tm", tm)
	req.Header.Set("x-pan-token", token)
	req.Header.Set("x-pan-client-id", qtvClientID)

	resp, err := q.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接夸克TV接口失败：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("夸克TV返回无法识别（HTTP %d）", resp.StatusCode)
		}
	}
	return nil
}

// ---- 扫码登录（OAuth2 二维码流） ----

// qtvAuthResp /oauth/authorize 与 /oauth/code 的公共返回结构。
type qtvAuthResp struct {
	Status     int    `json:"status"`
	Errno      int    `json:"errno"`
	ErrorInfo  string `json:"error_info"`
	QRData     string `json:"qr_data"`     // base64 二维码图（JPEG）
	QueryToken string `json:"query_token"` // 轮询凭证
	Code       string `json:"code"`        // 扫码确认后下发
}

// QRStart 获取登录二维码。返回 base64 图（前端直接 <img src>）与 query_token。
func (q *QuarkTV) QRStart(ctx context.Context) (qrData, queryToken string, err error) {
	var out qtvAuthResp
	err = q.qtvRequest(ctx, http.MethodGet, "/oauth/authorize", map[string]string{
		"auth_type": "code",
		"client_id": qtvClientID,
		"scope":     "netdisk",
		"qrcode":    "1",
		"qr_width":  "460",
		"qr_height": "460",
	}, &out)
	if err != nil {
		return "", "", err
	}
	if out.QRData == "" {
		return "", "", fmt.Errorf("夸克TV未返回二维码：%s", out.ErrorInfo)
	}
	return out.QRData, out.QueryToken, nil
}

// QRCode 轮询扫码结果：用户扫了并确认后返回 code（用于换 token）。
// 未扫码 / 已扫码未确认时返回空 code、nil error（调用方继续轮询）。
func (q *QuarkTV) QRCode(ctx context.Context, queryToken string) (code string, err error) {
	var out qtvAuthResp
	err = q.qtvRequest(ctx, http.MethodGet, "/oauth/code", map[string]string{
		"client_id":   qtvClientID,
		"scope":       "netdisk",
		"query_token": queryToken,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.Code, nil
}

// qtvTokenResp 换 token 的返回结构（codeApi 域名，非开放接口域名）。
// 注意 Data 里的 status/errno/error_info：无效 code 时顶层 code 仍是 200，
// 真错误藏在 data 里（实测 errno=11004「授权码Code无效」）。
type qtvTokenResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Status       int    `json:"status"`
		Errno        int    `json:"errno"`
		ErrorInfo    string `json:"error_info"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	} `json:"data"`
}

// qtvExchangeToken 用 code（首次）或 refresh_token（刷新）换 access_token。
// 驱动同款：token 接口在 codeApi 域名，body 带一整套设备参数。
func (q *QuarkTV) qtvExchangeToken(ctx context.Context, codeOrRefresh string, isRefresh bool) error {
	_, _, reqID := q.qtvSign(http.MethodPost, "/token")
	body := q.qtvDeviceParams(reqID, "")
	body.Set("device_id", q.deviceID)
	if isRefresh {
		body.Set("refresh_token", codeOrRefresh)
	} else {
		body.Set("code", codeOrRefresh)
	}
	b, err := json.Marshal(map[string]string(bodyToMap(body)))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qtvCodeAPI+"/token", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// User-Agent 必须带：api.extscreen.com 前面有 WAF，对「无 UA 头」的请求
	// 直接回 403 + HTML 挑战页（2026-10-20 实测，Go 默认不发 UA，踩过）。
	// OpenList 驱动走 resty 框架自带默认 UA，所以驱动侧从未暴露此问题。
	req.Header.Set("User-Agent", qtvUA)
	resp, err := q.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接夸克TV登录服务失败：%w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var out qtvTokenResp
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("夸克TV登录返回无法识别（HTTP %d）：%s", resp.StatusCode, qtvSnippet(data))
	}
	if out.Code != 200 || out.Data.RefreshToken == "" {
		// 注意：无效 code 时顶层 code 仍是 200，真错误在 data.status/errno/error_info
		//（实测：code=200 但 data.errno=11004「授权码Code无效」），先取 data 里的。
		msg := out.Data.ErrorInfo
		if msg == "" {
			msg = out.Message
		}
		if msg == "" {
			msg = fmt.Sprintf("code=%d", out.Code)
		}
		return fmt.Errorf("夸克TV登录失败：%s", msg)
	}
	q.mu.Lock()
	q.accessToken = out.Data.AccessToken
	q.refreshToken = out.Data.RefreshToken
	hook := q.onRefresh
	q.mu.Unlock()
	if hook != nil {
		hook(out.Data.AccessToken, out.Data.RefreshToken)
	}
	return nil
}

// bodyToMap url.Values → map[string]string（token 接口的 body 是 JSON 不是表单）。
func bodyToMap(v url.Values) map[string]string {
	m := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m
}

// qtvSnippet 把非 JSON 响应体截一段出来供报错定位（WAF 挑战页等场景）。
func qtvSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	if s == "" {
		s = "(空响应)"
	}
	return s
}

// LoginWithCode 扫码确认后走这里：code 换 token，返回 refresh_token（供入库）。
func (q *QuarkTV) LoginWithCode(ctx context.Context, code string) (refreshToken string, err error) {
	if err := q.qtvExchangeToken(ctx, code, false); err != nil {
		return "", err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.refreshToken, nil
}

// RefreshToken 用 refresh_token 换新的 access_token（token 过期时调用）。
func (q *QuarkTV) RefreshToken(ctx context.Context) error {
	q.mu.Lock()
	rt := q.refreshToken
	q.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("夸克TV未登录（无 refresh_token）")
	}
	return q.qtvExchangeToken(ctx, rt, true)
}

// qtvFileResp /file 接口的返回结构。
type qtvFileResp struct {
	Status    int    `json:"status"`
	Errno     int    `json:"errno"`
	ErrorInfo string `json:"error_info"`
	Data      struct {
		DownloadURL string `json:"download_url"`
		VideoInfo   []struct {
			Resolution string  `json:"resolution"`
			URL        string  `json:"url"`
			Width      int     `json:"width"`
			Height     int     `json:"height"`
			Size       int64   `json:"size"`
			Bitrate    float64 `json:"bitrate"`
		} `json:"video_info"`
	} `json:"data"`
}

// tokenInvalid 判断是否 token 失效（驱动同款：errno 10001/11001 或错误信息含 token 字样）。
func (r *qtvFileResp) tokenInvalid() bool {
	if r.Errno == 10001 || r.Errno == 11001 {
		return true
	}
	low := strings.ToLower(strings.TrimSpace(r.ErrorInfo))
	return strings.Contains(low, "access token") || strings.Contains(low, "access_token") ||
		strings.Contains(low, "token无效") || strings.Contains(low, "token 无效")
}

// qtvFileLink 取链公共逻辑：token 失效自动刷新重试一次（驱动同款自愈）。
func (q *QuarkTV) qtvFileLink(ctx context.Context, method, fid string) (*qtvFileResp, error) {
	params := func() map[string]string {
		return map[string]string{
			"method":     method, // download=原画 / streaming=转码流
			"group_by":   "source",
			"fid":        fid,
			"resolution": "low,normal,high,super,2k,4k",
			"support":    "dolby_vision",
		}
	}
	var out qtvFileResp
	if err := q.qtvRequest(ctx, http.MethodGet, "/file", params(), &out); err != nil {
		return nil, err
	}
	if out.tokenInvalid() {
		if rerr := q.RefreshToken(ctx); rerr == nil {
			var out2 qtvFileResp
			if err2 := q.qtvRequest(ctx, http.MethodGet, "/file", params(), &out2); err2 == nil {
				return &out2, nil
			}
		}
		return nil, fmt.Errorf("夸克TV登录态失效，请重新扫码：%s", out.ErrorInfo)
	}
	if out.Errno != 0 || out.Status >= 400 {
		return nil, fmt.Errorf("夸克TV：%s", out.ErrorInfo)
	}
	return &out, nil
}

// DownloadURL 取原画直链（fid = 转存后在自己盘里的文件 ID）。
// 返回的直链不带任何认证要求，可安全 302。
func (q *QuarkTV) DownloadURL(ctx context.Context, fid string) (string, error) {
	out, err := q.qtvFileLink(ctx, "download", fid)
	if err != nil {
		return "", err
	}
	if out.Data.DownloadURL == "" {
		return "", fmt.Errorf("夸克TV未返回下载地址")
	}
	return out.Data.DownloadURL, nil
}

// StreamingURL 取转码流直链（TV App 同款，弱网更快但清晰度降档）。
// 返回的是 H.264 MP4——浏览器解不开 MKV/H.265 原画（备忘录 §23.30），
// 网页播放就靠它（见 handlePlay 的浏览器分支）。
// video_info 可能带多档分辨率，取像素总数最大的一档。
func (q *QuarkTV) StreamingURL(ctx context.Context, fid string) (string, error) {
	out, err := q.qtvFileLink(ctx, "streaming", fid)
	if err != nil {
		return "", err
	}
	best, bestPx := "", int64(-1)
	for _, vi := range out.Data.VideoInfo {
		if vi.URL == "" {
			continue
		}
		px := int64(vi.Width) * int64(vi.Height)
		if px > bestPx {
			best, bestPx = vi.URL, px
		}
	}
	if best == "" {
		return "", fmt.Errorf("夸克TV未返回转码流地址")
	}
	return best, nil
}
