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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	doubaoBase = "https://www.doubao.com"
	doubaoUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	doubaoQs   = "version_code=20800&device_platform=web&aid=497858"

	// doubaoGap 全局限速间隔。实测 0.35s 间隔连打 8 次全过，取 350ms；
	// 真触发 429 还有退避重试兜底。大库扫描（几千文件）靠它压时间。
	doubaoGap = 350 * time.Millisecond
)

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
}

func newDoubao(cookie string) *Doubao {
	return &Doubao{cookie: strings.TrimSpace(cookie), hc: &http.Client{Timeout: 30 * time.Second}}
}

// ---- 全局限速 ----

var (
	dbGateMu   sync.Mutex
	dbLastCall time.Time
)

// dbThrottle 保证任意两次豆包请求至少间隔 doubaoGap。
func dbThrottle() {
	dbGateMu.Lock()
	defer dbGateMu.Unlock()
	if d := doubaoGap - time.Since(dbLastCall); d > 0 {
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

// post 调一个豆包接口，返回 data 原始 JSON。
// 命中限流（HTTP 429 或返回体含频繁提示）时退避重试，最多 3 次。
func (d *Doubao) post(path string, body any) (json.RawMessage, error) {
	if d.cookie == "" {
		return nil, fmt.Errorf("豆包未登录（请先粘贴 Cookie）")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second) // 2s / 4s
		}
		dbThrottle()
		req, err := http.NewRequest(http.MethodPost, doubaoBase+path+"?"+doubaoQs, bytes.NewReader(payload))
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
			lastErr = fmt.Errorf("豆包接口限流，稍后重试")
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("豆包返回 HTTP %d：%s", resp.StatusCode, dbSnippet(raw))
		}
		var out struct {
			Code    int             `json:"code"`
			Msg     string          `json:"msg"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("豆包返回无法识别：%s", dbSnippet(raw))
		}
		if out.Code != 0 {
			msg := out.Msg
			if msg == "" {
				msg = out.Message
			}
			low := strings.ToLower(msg)
			if strings.Contains(low, "too many") || strings.Contains(msg, "频繁") || out.Code == 429 {
				lastErr = fmt.Errorf("豆包接口限流：%s", msg)
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
		return out.Data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("豆包接口请求失败")
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

// ---- 自己的盘 ----

// dbHome 列根目录，返回根节点 ID 与顶层条目。
func (d *Doubao) dbHome() (rootID string, children []dbNode, err error) {
	data, err := d.post("/samantha/aispace/homepage", map[string]any{})
	if err != nil {
		return "", nil, err
	}
	var out struct {
		RootID   string   `json:"root_id"`
		Children []dbNode `json:"children"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", nil, fmt.Errorf("豆包根目录结构异常：%s", dbSnippet(data))
	}
	return out.RootID, out.Children, nil
}

// dbPage 是目录接口的一页。
type dbPage struct {
	Children   []dbNode `json:"children"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

// dbListDir 列自己盘某个目录（单页）。
func (d *Doubao) dbListDir(nodeID string) ([]dbNode, error) {
	return d.dbListDirPage(nodeID, "")
}

func (d *Doubao) dbListDirPage(nodeID, cursor string) ([]dbNode, error) {
	body := map[string]any{"node_id": nodeID}
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
	return out.Children, nil
}

// dbListDirAll 列完整个目录（自动翻页）。
// 豆包单个目录超过一页时会给 has_more + next_cursor，不翻页就会漏文件。
func (d *Doubao) dbListDirAll(nodeID string) ([]dbNode, error) {
	var all []dbNode
	cursor := ""
	for page := 0; page < 200; page++ {
		body := map[string]any{"node_id": nodeID}
		if cursor != "" {
			body["cursor"] = cursor
		}
		data, err := d.post("/samantha/aispace/node_info", body)
		if err != nil {
			if page == 0 {
				return nil, err
			}
			break // 翻页中途失败就用手上这批，不要整单失败
		}
		var out dbPage
		if err := json.Unmarshal(data, &out); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("豆包目录结构异常：%s", dbSnippet(data))
			}
			break
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

// dbShareListDir 列分享内某个目录（自动翻页）。
func (d *Doubao) dbShareListDir(shareID, nodeID string) ([]dbNode, error) {
	var all []dbNode
	cursor := ""
	for page := 0; page < 200; page++ {
		body := map[string]any{"share_id": shareID, "node_id": nodeID, "need_full_path": false}
		if cursor != "" {
			body["cursor"] = cursor
		}
		data, err := d.post("/samantha/aispace/share/node_info", body)
		if err != nil {
			if page == 0 {
				return nil, err
			}
			break
		}
		var out dbPage
		if err := json.Unmarshal(data, &out); err != nil {
			if page == 0 {
				return nil, fmt.Errorf("豆包分享目录结构异常：%s", dbSnippet(data))
			}
			break
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
