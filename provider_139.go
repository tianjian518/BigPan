package main

// ---------------------------------------------------------------------------
// 移动云盘（139）分享支持
//
// 接口链（从 OpenList 139 驱动 + 巨库二进制互证 + 真机实测得出）：
//   ① 列目录  POST /yun-share/richlifeApp/devapp/IOutLink/getOutLinkInfo     (pCaID=root)
//             POST /yun-share/richlifeApp/devapp/IOutLink/getOutLinkInfoV6   (子目录)
//   ② 取流    POST /yun-share/richlifeApp/devapp/IOutLink/getContentInfoFromOutLink
//             → data.contentInfo.presentURL = m3u8 HLS（4 档清晰度自适应）
//
// 关键事实（实测 2026-10-14）：
//   - 请求/响应均为 AES-128-CBC 加密，key 固定 "PVGDwmcvfs1uV3d1"，
//     请求体 = base64(随机IV + 密文)，响应 data 字段同结构（可能 gzip）。
//   - 个人分享（/w/i/）拿不到原文件直链（dlFromOutLinkV3 报"暂无权限"），
//     但 presentURL 的 m3u8 完全可用：不绑 Cookie、不绑 UA，
//     自带 AWS 签名 8 小时有效 —— 302 直连可行，这正是本网盘的价值。
//   - 列目录 / 取流均需要登录态（Authorization: Basic <token>）。
//   - caID/coID 带随机后缀且随会话变化，一次调用链内必须用同批返回的 ID。
// ---------------------------------------------------------------------------

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	m139API  = "https://share-kd-njs.yun.139.com/yun-share"
	m139Key  = "PVGDwmcvfs1uV3d1"
	m139Acct = "mobile139"
	m139TTL  = 6 * time.Hour // 签名 8 小时，留余量
)

// m139LinkRe 识别移动云盘分享链接：yun.139.com/shareweb/#/w/i/{key}?pwd=xxx
// 注意 pwd 跟在 `#` 之后的 fragment 里，必须单独抓，否则 ListShareFiles 传空串
// 会被百度...（移动）判「提取码非法 errno=9188」而解析失败。
var m139LinkRe = regexp.MustCompile(`139\.com/shareweb/#/[a-z]/[a-z]/([0-9A-Za-z_-]+)(?:\?([0-9A-Za-z_=&-]*))?`)

func parse139Link(link string) (key string, passcode string, err error) {
	m := m139LinkRe.FindStringSubmatch(link)
	if m == nil {
		return "", "", fmt.Errorf("不是移动云盘分享链接")
	}
	// 从 query 段（?pwd=xxx 或 ?password=xxx）里抠提取码
	if m[2] != "" {
		q, qerr := url.ParseQuery(m[2])
		if qerr == nil {
			for _, k := range []string{"pwd", "password", "pwd_id", "code"} {
				if v := q.Get(k); v != "" {
					return m[1], v, nil
				}
			}
		}
	}
	return m[1], "", nil
}

// m139Client 一个登录态的移动云盘客户端。
type m139Client struct {
	account string // 手机号
	auth    string // "Basic xxx" 完整头
	http    *http.Client
}

// newM139 从账号库取凭证。token 两种形态都收：
//   - 完整 "Basic xxx"
//   - 裸 token（自动补 Basic）
//   - base64(手机号:密码)（旧式，解码取手机号）
func newM139(as *AccountStore) *m139Client {
	a := as.Get(m139Acct)
	if a == nil || a.Cookie == "" {
		return nil
	}
	auth := strings.TrimSpace(a.Cookie)
	if !strings.HasPrefix(strings.ToLower(auth), "basic ") {
		auth = "Basic " + auth
	}
	// 从 token 里解手机号
	account := ""
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(auth, "Basic "))); err == nil {
		// "pc:13800000000:..." / "13800000000:密码"
		parts := strings.Split(string(raw), ":")
		for _, p := range parts {
			if len(p) == 11 && strings.HasPrefix(p, "1") {
				account = p
				break
			}
		}
	}
	if account == "" {
		// Label 兜底存了手机号
		if len(a.Label) == 11 && strings.HasPrefix(a.Label, "1") {
			account = a.Label
		}
	}
	if account == "" {
		return nil
	}
	return &m139Client{account: account, auth: auth, http: &http.Client{Timeout: 30 * time.Second}}
}

// ---- AES-CBC 加解密 ----

func m139Pad(b []byte) []byte {
	pad := aes.BlockSize - len(b)%aes.BlockSize
	return append(b, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func (c *m139Client) enc(v interface{}) string {
	raw, _ := json.Marshal(v)
	iv := make([]byte, 16)
	_, _ = rand.Read(iv)
	block, _ := aes.NewCipher([]byte(m139Key))
	out := make([]byte, aes.BlockSize+len(m139Pad(raw)))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[aes.BlockSize:], m139Pad(raw))
	return base64.StdEncoding.EncodeToString(out)
}

func m139Dec(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) < 32 {
		return nil, fmt.Errorf("139 响应不是加密串")
	}
	block, _ := aes.NewCipher([]byte(m139Key))
	pt := make([]byte, len(raw)-16)
	cipher.NewCBCDecrypter(block, raw[:16]).CryptBlocks(pt, raw[16:])
	if n := len(pt); n > 0 {
		pad := int(pt[n-1])
		if pad > 0 && pad <= 16 {
			pt = pt[:n-pad]
		}
	}
	if len(pt) > 2 && pt[0] == 0x1f && pt[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(pt))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	}
	return pt, nil
}

// m139Resp 统一响应（解密后）。
type m139Resp struct {
	ResultCode string          `json:"resultCode"`
	Desc       string          `json:"desc"`
	Data       json.RawMessage `json:"data"`
}

func (r *m139Resp) OK() bool { return r.ResultCode == "0" }

// post 加密请求 → 解密响应。data 的 data 字段为加密串或已明文。
func (c *m139Client) post(path string, payload interface{}) (*m139Resp, error) {
	body := strings.NewReader(c.enc(payload))
	req, err := http.NewRequest(http.MethodPost, m139API+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("X-Deviceinfo", "||9|12.27.0|firefox|140.0|12b780037221ab547c682223327dc9cd||linux unknow|1920X526|zh-CN|||")
	req.Header.Set("hcy-cool-flag", "1")
	req.Header.Set("CMS-DEVICE", "default")
	req.Header.Set("x-m4c-caller", "PC")
	req.Header.Set("X-Yun-Api-Version", "v1")
	req.Header.Set("Origin", "https://yun.139.com")
	req.Header.Set("Referer", "https://yun.139.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接移动云盘失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)

	// 实测：响应整体是一个加密串 base64(IV+密文)，解密后才是 JSON。
	// 兜底：若不是加密串，按明文 JSON 处理。
	var outer m139Resp
	pt, derr := m139Dec(text)
	if derr == nil {
		if err := json.Unmarshal(pt, &outer); err != nil {
			return nil, fmt.Errorf("139 响应解密后无法解析: %.120s", pt)
		}
	} else if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("139 响应异常(HTTP %d): %.120s", resp.StatusCode, text)
	}
	if !outer.OK() {
		return &outer, fmt.Errorf("移动云盘: %s（%s）", outer.Desc, outer.ResultCode)
	}
	return &outer, nil
}

// ---- 目录结构 ----

type m139Catalog struct {
	CaID   string `json:"caID"`
	CaName string `json:"caName"`
}
type m139Content struct {
	CoID          string `json:"coID"`
	CoName        string `json:"coName"`
	CoSize        int64  `json:"coSize"`
	ContentSuffix string `json:"contentSuffix"`
}
type m139DirData struct {
	CaLst []m139Catalog `json:"caLst"`
	CoLst []m139Content `json:"coLst"`
	CaLen json.Number   `json:"caLength"` // 服务端返回字符串型数字
	CoLen json.Number   `json:"coLength"`
}

// listDir 列某个目录（pCaID 传 "root" 表示分享根）。
func (c *m139Client) listDir(linkID, pCaID, passcode string) (*m139DirData, error) {
	req := map[string]interface{}{
		"account": c.account,
		"linkID":  linkID,
		"pCaID":   pCaID,
	}
	if passcode != "" {
		req["passCode"] = passcode
		req["passwd"] = passcode
	}
	var path string
	if pCaID == "root" {
		path = "/richlifeApp/devapp/IOutLink/getOutLinkInfo"
	} else {
		path = "/richlifeApp/devapp/IOutLink/getOutLinkInfoV6"
	}
	r, err := c.post(path, map[string]interface{}{"getOutLinkInfoReq": req})
	if err != nil {
		return nil, err
	}
	var d m139DirData
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return nil, fmt.Errorf("139 目录响应解析失败")
	}
	return &d, nil
}

// presentURL 取某文件的 m3u8 播放地址。
func (c *m139Client) presentURL(linkID, coID, passcode string) (string, error) {
	req := map[string]interface{}{
		"contentId": coID,
		"linkID":    linkID,
		"account":   c.account,
	}
	if passcode != "" {
		req["passwd"] = passcode
		req["passCode"] = passcode
	}
	r, err := c.post("/richlifeApp/devapp/IOutLink/getContentInfoFromOutLink",
		map[string]interface{}{"getContentInfoFromOutLinkReq": req})
	if err != nil {
		return "", err
	}
	var d struct {
		ContentInfo struct {
			PresentURL string `json:"presentURL"`
		} `json:"contentInfo"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return "", fmt.Errorf("139 取流响应解析失败")
	}
	if d.ContentInfo.PresentURL == "" {
		return "", fmt.Errorf("该文件没有可用的转码播放流")
	}
	return d.ContentInfo.PresentURL, nil
}

// shareTitle 分享标题（getOutLinkInfo 根响应 lkName）。
func (c *m139Client) shareTitle(linkID, passcode string) string {
	req := map[string]interface{}{"account": c.account, "linkID": linkID, "pCaID": "root"}
	if passcode != "" {
		req["passCode"] = passcode
	}
	r, err := c.post("/richlifeApp/devapp/IOutLink/getOutLinkInfo",
		map[string]interface{}{"getOutLinkInfoReq": req})
	if err != nil {
		return ""
	}
	var d struct {
		LkName string `json:"lkName"`
	}
	_ = json.Unmarshal(r.Data, &d)
	return d.LkName
}

// scan139Share BFS 递归扫整棵分享目录树（与夸克 scanQuarkShare 同款）。
// 上限：目录 200 / 文件 500。coID 用入库时的值跨会话取流（实测有效）。
func scan139Share(c *m139Client, linkID, passcode string) (title string, files []ShareFile, err error) {
	// totalVideo 统计「视频扩展名的文件数」，包含被跳过的 ISO。
	// 用来区分两种「files 为空」：真空分享（该判死清理）vs 只有 ISO（保留）。
	totalVideo := 0
	type job struct{ caID, prefix string }
	queue := []job{{caID: "root"}}
	seen := 0
	for len(queue) > 0 && len(files) < 500 {
		j := queue[0]
		queue = queue[1:]
		seen++
		if seen > 200 {
			break
		}
		d, err := c.listDir(linkID, j.caID, passcode)
		if err != nil {
			if j.caID == "root" {
				return "", nil, err // 根都进不去，直接报错
			}
			continue
		}
		for _, dir := range d.CaLst {
			p := dir.CaName
			if j.prefix != "" {
				p = j.prefix + "/" + dir.CaName
			}
			queue = append(queue, job{caID: dir.CaID, prefix: p})
		}
		for _, f := range d.CoLst {
			if !isVideo(f.CoName) {
				continue
			}
			totalVideo++
			// 139 分享盘对 .iso 不提供转码流（presentURL 为空），分享场景也拿不到
			// 原文件下载直链 —— 收录了也无流可播，纯粹占列表，扫描时直接跳过。
			if strings.HasSuffix(strings.ToLower(f.CoName), ".iso") {
				continue
			}
			p := f.CoName
			if j.prefix != "" {
				p = j.prefix + "/" + f.CoName
			}
			files = append(files, ShareFile{
				ID: f.CoID, Name: f.CoName, Size: f.CoSize, Path: p,
				Code: linkID, Pwd: passcode,
			})
		}
	}
	if len(files) == 0 {
		if totalVideo == 0 {
			// 分享里一个视频文件都没有（连 ISO 都没有）——这是真空分享，
			// 巡检应当判死并清理。与「只有 ISO」必须区分开，否则失效分享永远留着。
			return "", nil, fmt.Errorf("分享里没有任何文件")
		}
		return "", nil, fmt.Errorf("分享里只有 ISO 镜像（139 不提供播放流）")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return title, files, nil
}

// ---- PanProvider 实现 ----

type m139Provider struct{}

func init() { registerProvider(m139Provider{}) }

func (m139Provider) Name() string { return m139Acct }

func (m139Provider) MatchLink(link string) bool {
	return strings.Contains(link, "139.com") || strings.Contains(link, "yun.139.com")
}

func (m139Provider) client() *m139Client {
	if globalAccounts == nil {
		return nil
	}
	return newM139(globalAccounts)
}

func (m139Provider) ListShareFiles(link string) (ShareInfo, error) {
	c := m139Provider{}.client()
	if c == nil {
		return ShareInfo{}, fmt.Errorf("还没有导入移动云盘凭证（139 解析/取流都需要登录），请到「账号与接入」导入")
	}
	// 【2026-10-17 修复「带提取码的 139 分享解析失败」】
	// 原来这里把提取码硬编码成 ""，移动云盘对带码分享一律返回
	// 「提取码非法 errno=9188」→ 前端只看到「解析失败」，看不出原因。
	// 现在从链接里把 pwd 抠出来，链接里没有时再用前端「提取码」输入框的值
	// （share.go 的 mergeSharePwd 已把它拼到链接末尾）。
	key, passcode, err := parse139Link(link)
	if err != nil {
		return ShareInfo{}, err
	}
	title := c.shareTitle(key, passcode)
	if title == "" {
		title = key
	}
	_, files, err := scan139Share(c, key, passcode)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: m139Acct,
		Title:    title,
		ShareID:  key,
		Passcode: passcode,
		Files:    files,
		Logged:   true,
	}, nil
}

func (m139Provider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	c := m139Provider{}.client()
	if c == nil {
		return "", nil, fmt.Errorf("还没有导入移动云盘凭证")
	}
	title := c.shareTitle(shareID, passcode)
	if title == "" {
		title = shareID
	}
	_, files, err := scan139Share(c, shareID, passcode)
	return title, files, err
}

// Wants302 m3u8 不绑会话，客户端直连 —— 这正是接 139 的核心价值。
func (m139Provider) Wants302() bool { return true }

func (m139Provider) ResolvePlay(item *Item) (PlayTarget, error) {
	c := m139Provider{}.client()
	if c == nil {
		return PlayTarget{}, fmt.Errorf("还没有导入移动云盘凭证")
	}
	u, err := c.presentURL(item.ShareID, item.FileID, item.Passcode)
	if err != nil {
		// coID 失效兜底：重扫分享按文件名匹配
		log.Printf("[139] 直链失败（%v），重扫分享匹配文件", err)
		_, files, serr := scan139Share(c, item.ShareID, item.Passcode)
		if serr != nil {
			return PlayTarget{}, err
		}
		for _, f := range files {
			if f.Name == item.Title+item.Ext || f.Name == strings.TrimSuffix(item.FileName(), item.Ext)+item.Ext {
				if u2, e2 := c.presentURL(item.ShareID, f.ID, item.Passcode); e2 == nil {
					return PlayTarget{URL: u2}, nil
				}
			}
		}
		return PlayTarget{}, err
	}
	// m3u8 由播放器自己解析，无需 UA/Cookie
	return PlayTarget{URL: u, UA: ""}, nil
}
