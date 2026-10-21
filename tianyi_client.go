package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tclient "bigpan/internal/tianyi/client"
)

// ---------------------------------------------------------------------------
// 天翼云盘（cloud.189.cn）API 封装
//
// 底层库是原样搬进来的 cloudpan189-interface（见 internal/tianyi/doc.go），
// 本文件只做 BigPan 适配：链接解析、凭证管理、分享树扫描、取链、订阅号拉取。
//
// 三个必须记住的坑（详见 internal/tianyi/doc.go 与备忘录）：
//   ① 业务错误是 HTTP 200 + res_code != 0 —— 每个响应都要手检 ResCode；
//   ② token 无刷新，过期只能重扫码；
//   ③ 下载直链会 302 到带 token 的地址，必须不跟随重定向取 Location。
// ---------------------------------------------------------------------------

// 天翼分享链接，两种形态都支持：
//   新：https://cloud.189.cn/web/share?code=XXXXXXXX
//   短：https://cloud.189.cn/t/XXXXXXXX   （302 到 ?code= 形式，code 与短链尾段相同，无需发请求）
// 访问码（提取码）不在链接里，是单独给的 4 位字符；前端可随链接一起粘
// （"链接 访问码"），或在提取码框单独填（apiShareParse 会拼成 ?pwd= 再进来）。
var tianyiShareRe = regexp.MustCompile(`(?i)(?:[a-z0-9-]+\.)?189\.cn/(?:web/share\?code=|t/)([A-Za-z0-9]+)`)

var tianyiPwdRe = regexp.MustCompile(`(?:提取码|访问码|密码|password|pwd)\s*[:：]?\s*([A-Za-z0-9]{4})`)

// parseTianyiLink 从天翼分享文本中提取 shareCode 与访问码。
func parseTianyiLink(raw string) (code, pwd string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("请粘贴分享链接")
	}
	// 访问码：URL 参数优先（apiShareParse 的 mergeSharePwd 拼进来的），
	// 其次文本里的「访问码：xxxx」
	if u, e := url.Parse(raw); e == nil {
		pwd = u.Query().Get("pwd")
		if pwd == "" {
			pwd = u.Query().Get("password")
		}
	}
	if pwd == "" {
		if m := tianyiPwdRe.FindStringSubmatch(raw); m != nil {
			pwd = m[1]
		}
	}
	if m := tianyiShareRe.FindStringSubmatch(raw); m != nil {
		return m[1], pwd, nil
	}
	return "", "", fmt.Errorf("无法识别这个链接，请粘贴完整的天翼分享链接（形如 https://cloud.189.cn/t/xxxx 或 https://cloud.189.cn/web/share?code=xxxx）")
}

// ---------------------------------------------------------------------------
// 凭证管理
// ---------------------------------------------------------------------------

// tianyiCred 天翼登录凭证：扫码拿到的 accessToken + 绝对过期时间（毫秒时间戳）。
// 天翼 token 没有刷新机制，过期只能重扫码。
type tianyiCred struct {
	AccessToken string
	ExpiresMs   int64
}

func (c *tianyiCred) Expired() bool {
	return c == nil || c.AccessToken == "" || time.Now().UnixMilli() >= c.ExpiresMs
}

// tianyiCredOf 从账号库取当前天翼凭证；未登录 / 已过期都返回 nil。
func tianyiCredOf(as *AccountStore) *tianyiCred {
	if as == nil {
		return nil
	}
	acc := as.Get("tianyi")
	if acc == nil || acc.Cookie == "" {
		return nil
	}
	c := &tianyiCred{AccessToken: acc.Cookie, ExpiresMs: acc.TokenExpires}
	if c.Expired() {
		return nil
	}
	return c
}

// tianyiAPI 是天翼 API 的调用门面。
//
// 底层 client.WithToken 是原地修改（换 token 会污染同一实例），所以每个
// 凭证一个实例 + 内部互斥锁串行 —— 播放、订阅拉取都走这里，天然不会串 token。
type tianyiAPI struct {
	mu   sync.Mutex
	cred *tianyiCred
	cl   tclient.Client
}

var (
	tianyiAPIMu    sync.Mutex
	tianyiAPICache = map[string]*tianyiAPI{} // key = accessToken
)

// tianyiClientFor 取（或建）一个凭证专属的 API 门面。
func tianyiClientFor(cred *tianyiCred) *tianyiAPI {
	if cred == nil || cred.AccessToken == "" {
		return nil
	}
	tianyiAPIMu.Lock()
	defer tianyiAPIMu.Unlock()
	key := cred.AccessToken
	if c, ok := tianyiAPICache[key]; ok {
		c.cred = cred // 过期时间可能被重新算过，同步最新值
		return c
	}
	c := &tianyiAPI{
		cred: cred,
		cl:   tclient.New().WithToken(tclient.NewAuthToken(cred.AccessToken, cred.ExpiresMs)),
	}
	tianyiAPICache[key] = c
	return c
}

// call 持锁执行一次 API 调用；token 过期直接拒（避免拿过期 token 白打接口）。
func (c *tianyiAPI) call(fn func(cl tclient.Client) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cred.Expired() {
		return fmt.Errorf("天翼登录已过期，请重新扫码登录")
	}
	return fn(c.cl)
}

// tianyiAnon 匿名（无 token）调用的共享客户端。
// 分享解析/列目录本就不需要 token，永远不给它 WithToken，不存在串 token
// 问题；共享出来是为了复用连接（BFS 扫一棵树要打几十个列目录请求）。
var tianyiAnon = tclient.New()

// tianyiDo 统一入口：有凭证走凭证门面（持锁串行），没凭证走匿名客户端。
// 分享解析/列目录不需要 token；订阅号接口按文档也不需要，但带了更稳。
func tianyiDo(cred *tianyiCred, fn func(cl tclient.Client) error) error {
	if api := tianyiClientFor(cred); api != nil {
		return api.call(fn)
	}
	return fn(tianyiAnon)
}

// ---------------------------------------------------------------------------
// 分享解析与目录树扫描（解析/列目录不需要登录，取流才需要）
// ---------------------------------------------------------------------------

// tianyiShareMeta 一次分享解析出来的元信息。
type tianyiShareMeta struct {
	ShareId    int64
	RootFileId string // 分享根节点（目录型分享 = 根目录 fileId；单文件分享 = 文件 fileId）
	FileName   string
	FileSize   int // 根节点大小（单文件分享时是文件大小）
	IsFolder   bool
	AccessCode string
	ShareMode  int
}

// tianyiGetShare 解析分享链接拿元信息（GetShareInfo 不需要登录）。
func tianyiGetShare(shareCode, accessCode string) (*tianyiShareMeta, error) {
	var resp *tclient.GetShareInfoResponse
	err := tianyiDo(nil, func(cl tclient.Client) error {
		r, e := cl.GetShareInfo(context.Background(), shareCode, func(req *tclient.GetShareInfoRequest) {
			req.AccessCode = accessCode
		})
		resp = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("分享解析失败：%w", err)
	}
	// 坑①：HTTP 200 + res_code != 0 是业务错误，必须手检
	if resp == nil || resp.ResCode != 0 {
		msg := ""
		if resp != nil {
			msg = resp.ResMessage
		}
		if accessCode != "" && (strings.Contains(msg, "码") || strings.Contains(msg, "code") || strings.Contains(msg, "Code")) {
			return nil, fmt.Errorf("访问码可能不对（%s），请检查后重试", msg)
		}
		return nil, fmt.Errorf("分享解析失败（res_code=%v）：%s", respCodeOf(resp), msg)
	}
	// 坑④（2026-10 实测）：getShareInfoByCode 已经不再返回 shareId 了，
	// 而 listShareDir 必须用 shareId 才能列目录。真值在 checkAccessCode 的响应里，
	// 这里补一步换 shareId；没有访问码的分享走空 accessCode 一并换。
	shareID := resp.ShareId
	if shareID == 0 {
		var id int64
		codeErr := tianyiDo(nil, func(cl tclient.Client) error {
			v, e := cl.CheckAccessCode(context.Background(), shareCode, accessCode)
			id = v
			return e
		})
		if codeErr != nil {
			if accessCode != "" {
				return nil, fmt.Errorf("分享解析失败：%w", codeErr)
			}
			return nil, fmt.Errorf("分享解析失败：%w（该分享可能需要访问码）", codeErr)
		}
		shareID = id
	}

	acc := accessCode
	if acc == "" {
		acc = resp.AccessCode
	}
	return &tianyiShareMeta{
		ShareId:    shareID,
		RootFileId: string(resp.FileId),
		FileName:   resp.FileName,
		FileSize:   resp.FileSize,
		IsFolder:   resp.IsFolder,
		AccessCode: acc,
		ShareMode:  resp.ShareMode,
	}, nil
}

// respCodeOf 容错取 res_code（响应为 nil 时返回 "?"）。
func respCodeOf(resp *tclient.GetShareInfoResponse) any {
	if resp == nil {
		return "?"
	}
	return resp.ResCode
}

// tianyiDirEntry 分享目录里的一项。
type tianyiDirEntry struct {
	ID    string
	Name  string
	Size  int64
	IsDir bool
}

// tianyiListShareDir 列一页分享目录（内部自动翻页取全，单目录上限 50 页防御死循环）。
func tianyiListShareDir(cred *tianyiCred, shareId int64, fileId, accessCode string, shareMode, isFolder int) ([]tianyiDirEntry, error) {
	var out []tianyiDirEntry
	page := 1
	for page <= 50 {
		var resp *tclient.ListFilesResponse
		err := tianyiDo(cred, func(cl tclient.Client) error {
			r, e := cl.ListShareDir(context.Background(), shareId, tclient.String(fileId), func(req *tclient.ListShareFileRequest) {
				req.PageNum = page
				req.PageSize = 100
				req.AccessCode = accessCode
				req.ShareMode = shareMode
				req.IconOption = 5
				if isFolder == 1 {
					req.IsFolder = true
				} else if isFolder == 2 {
					req.IsFolder = false
				}
			})
			resp = r
			return e
		})
		if err != nil {
			return nil, fmt.Errorf("列目录失败：%w", err)
		}
		if resp == nil || resp.ResCode != 0 {
			msg := ""
			if resp != nil {
				msg = resp.ResMessage
			}
			return nil, fmt.Errorf("列目录失败（res_code=%v）：%s", respListCode(resp), msg)
		}
		for _, f := range resp.FileListAO.FolderList {
			out = append(out, tianyiDirEntry{ID: string(f.Id), Name: f.Name, IsDir: true})
		}
		for _, f := range resp.FileListAO.FileList {
			out = append(out, tianyiDirEntry{ID: string(f.Id), Name: f.Name, Size: f.Size})
		}
		got := int64(len(resp.FileListAO.FileList) + len(resp.FileListAO.FolderList))
		if got == 0 || int64(len(out)) >= resp.FileListAO.Count || got < 100 {
			break
		}
		page++
	}
	return out, nil
}

func respListCode(resp *tclient.ListFilesResponse) any {
	if resp == nil {
		return "?"
	}
	return resp.ResCode
}

// tianyiWalkRoot 一次目录树扫描的根节点（普通分享来自 GetShareInfo，
// 订阅号分享来自 GetUpResourceShare，两个来源在这层统一）。
type tianyiWalkRoot struct {
	ShareId    int64
	RootFileId string // 目录型分享 = 根目录 fileId；单文件分享 = 文件 fileId
	RootName   string // 根节点名称（单文件分享时就是文件名）
	RootSize   int64  // 根节点大小（单文件分享时是文件大小）
	IsFolder   bool
	AccessCode string
	ShareMode  int
	ShareCode  string // 分享 code（订阅号分享可能没有，传空）
}

// tianyiWalkShare 从根节点开始 BFS 展开整棵分享目录树，只收视频文件。
// 普通分享与订阅号分享共用（区别只在根节点怎么来）。
// 上限：目录 200 个 / 文件 500 个，与夸克扫描一致，防超大分享拖死。
func tianyiWalkShare(cred *tianyiCred, root tianyiWalkRoot) ([]ShareFile, error) {
	var files []ShareFile
	numID := strconv.FormatInt(root.ShareId, 10)

	// 单文件分享：根本身就是文件（名字/大小根节点自带，不必再查）
	if !root.IsFolder {
		if root.RootFileId == "" {
			return nil, fmt.Errorf("分享信息不完整（缺文件 ID）")
		}
		if !isVideo(root.RootName) {
			return nil, fmt.Errorf("这个分享里没有找到视频文件")
		}
		return []ShareFile{{
			ID: root.RootFileId, Name: root.RootName, Size: root.RootSize, Path: root.RootName,
			Code: root.ShareCode, Pwd: root.AccessCode, ShareNumID: numID,
		}}, nil
	}

	type dirJob struct {
		fid, prefix string
	}
	queue := []dirJob{{fid: root.RootFileId}}
	seenDirs := map[string]bool{root.RootFileId: true}
	for len(queue) > 0 && len(files) < 500 {
		job := queue[0]
		queue = queue[1:]
		list, err := tianyiListShareDir(cred, root.ShareId, job.fid, root.AccessCode, root.ShareMode, 1)
		if err != nil {
			continue // 单层列失败不中断整棵树（与其他网盘扫描行为一致）
		}
		for _, e := range list {
			p := e.Name
			if job.prefix != "" {
				p = job.prefix + "/" + e.Name
			}
			if e.IsDir {
				if !seenDirs[e.ID] && len(seenDirs) < 200 {
					seenDirs[e.ID] = true
					queue = append(queue, dirJob{fid: e.ID, prefix: p})
				}
				continue
			}
			if !isVideo(e.Name) {
				continue
			}
			files = append(files, ShareFile{
				ID: e.ID, Name: e.Name, Size: e.Size, Path: p,
				Code: root.ShareCode, Pwd: root.AccessCode, ShareNumID: numID,
			})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("这个分享里没有找到视频文件")
	}
	return files, nil
}

// scanTianyiShare 解析天翼分享链接，展开整棵目录树，返回视频文件。
// 首次解析与追更刷新共用（分享标识 = shareCode + 访问码）。
func scanTianyiShare(shareCode, accessCode string) (title string, files []ShareFile, err error) {
	meta, err := tianyiGetShare(shareCode, accessCode)
	if err != nil {
		return "", nil, err
	}
	title = meta.FileName
	if title == "" {
		title = shareCode
	}
	files, err = tianyiWalkShare(nil, tianyiWalkRoot{
		ShareId:    meta.ShareId,
		RootFileId: meta.RootFileId,
		RootName:   meta.FileName,
		RootSize:   int64(meta.FileSize),
		IsFolder:   meta.IsFolder,
		AccessCode: accessCode,
		ShareMode:  meta.ShareMode,
		ShareCode:  shareCode,
	})
	if err != nil {
		return title, nil, err
	}
	return title, files, nil
}

// ---------------------------------------------------------------------------
// 播放取链
// ---------------------------------------------------------------------------

// tianyiNoFollowClient 不跟随重定向的 HTTP 客户端：
// 天翼下载链接会 302 到带 token 的真实地址，把原始 URL 给客户端等于泄露
// token；取 Location 头里的直链才安全（与 cloudpan189-share 同一做法）。
var tianyiNoFollowClient = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		req.Header.Set("User-Agent", tclient.UserAgent)
		return http.ErrUseLastResponse
	},
}

// tianyiDownloadURL 拿分享文件的真实下载直链（需要登录）。
// 优先 GetFileDownload → 不跟随重定向取 Location；拿不到再退 VLC 播放地址。
// tianyiRawDownloadURL 只取 GetFileDownload 返回的原始地址（还没跟 302）。
func tianyiRawDownloadURL(cred *tianyiCred, shareId int64, fileId string) (string, error) {
	if cred.Expired() {
		return "", fmt.Errorf("还没有导入天翼凭证，或登录已过期（播放需要登录，解析分享不需要）")
	}
	var raw string
	err := tianyiDo(cred, func(cl tclient.Client) error {
		resp, e := cl.GetFileDownload(context.Background(), tclient.String(fileId), func(req *tclient.GetFileDownloadRequest) {
			req.ShareId = shareId
		})
		if e != nil {
			return e
		}
		if resp == nil || resp.ResCode != 0 {
			msg := ""
			if resp != nil {
				msg = resp.ResMessage
			}
			return fmt.Errorf("获取下载地址失败（res_code=%v）：%s", respDownloadCode(resp), msg)
		}
		raw = resp.FileDownloadUrl
		return nil
	})
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", fmt.Errorf("下载地址为空")
	}
	return raw, nil
}

func tianyiDownloadURL(cred *tianyiCred, shareId int64, fileId string) (string, error) {
	raw, err := tianyiRawDownloadURL(cred, shareId, fileId)
	if err != nil {
		return "", err
	}
	// 不跟随重定向，取 Location 头的真实直链
	resp, err := tianyiNoFollowClient.Get(raw)
	if err != nil {
		return "", fmt.Errorf("请求下载地址失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("重定向响应但没有 Location 头")
		}
		return loc, nil
	}
	if resp.StatusCode == http.StatusOK {
		// 少数情况直接 200（原始 URL 即直链）——它带 token，只作兜底，
		// 由调用方决定是否使用；正常路径不该到这里。
		return raw, nil
	}
	// 天翼常见拦截：内容审核（InfoSecurityCheck）。
	// 原文形如 <error><code>InfoSecurityCheck</code><message>resources is illegal…</message></error>
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	b := strings.TrimSpace(string(body))
	if strings.Contains(b, "InfoSecurityCheck") {
		return "", fmt.Errorf("这个文件被天翼内容审核拦截了（InfoSecurityCheck），换片源吧")
	}
	if b != "" {
		return "", fmt.Errorf("下载地址异常（HTTP %d）：%s", resp.StatusCode, b)
	}
	return "", fmt.Errorf("下载地址异常（HTTP %d）", resp.StatusCode)
}

func respDownloadCode(resp *tclient.GetFileDownloadResponse) any {
	if resp == nil {
		return "?"
	}
	return resp.ResCode
}

// tianyiVLCPlayURL 备用播放地址（m3u8 转码流，原画限速时用它）。
func tianyiVLCPlayURL(cred *tianyiCred, shareId int64, fileId string) (string, error) {
	if cred.Expired() {
		return "", fmt.Errorf("天翼登录已过期，请重新扫码登录")
	}
	var u string
	err := tianyiDo(cred, func(cl tclient.Client) error {
		resp, e := cl.GetNewVLCVideoPlayURL(context.Background(), tclient.String(fileId), func(req *tclient.GetNewVLCVideoPlayURLRequest) {
			req.ShareId = strconv.FormatInt(shareId, 10)
		})
		if e != nil {
			return e
		}
		if resp == nil || resp.ResCode != 0 || resp.Normal.Code != 0 {
			return fmt.Errorf("获取播放地址失败（res_code=%v）", respVLCCode(resp))
		}
		u = resp.Normal.URL
		return nil
	})
	if err != nil {
		return "", err
	}
	if u == "" {
		return "", fmt.Errorf("播放地址为空")
	}
	return u, nil
}

func respVLCCode(resp *tclient.GetNewVLCVideoPlayURLResponse) any {
	if resp == nil {
		return "?"
	}
	if resp.Normal.Code != 0 {
		return resp.Normal.Code
	}
	return resp.ResCode
}

// ---------------------------------------------------------------------------
// 订阅号（资源号）拉取
// ---------------------------------------------------------------------------

// tianyiSubShare 订阅号下的一条分享。
type tianyiSubShare struct {
	ShareId    int64  `json:"share_id"`
	RootFileId string `json:"root_file_id"` // 目录型分享 = 根目录 fileId；单文件分享 = 文件 fileId
	Name       string `json:"name"`
	ShareCode  string `json:"share_code"` // 从 AccessURL 提取的分享 code（可能为空）
	IsFolder   bool   `json:"is_folder"`
	Size       int64  `json:"size"`
	ShareDate  string `json:"share_date"` // "2006-01-02 15:04:05"
}

// tianyiSubShareAt 解析分享时间为 unix 秒（水位比较用）；失败返回 0。
func (s tianyiSubShare) ShareAt() int64 {
	t, err := time.Parse(time.DateTime, s.ShareDate)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// tianyiSubUser 验证订阅号存在并拿昵称。
// 注意：订阅号接口的 Code 字段语义不确定（参考项目根本不检它，只用 Data），
// 这里只认「Data.UserId 为空 = 不存在」，避免误杀有效订阅号。
func tianyiSubUser(cred *tianyiCred, userId string) (name string, err error) {
	var resp *tclient.SubscribeGetUserResponse
	err = tianyiDo(cred, func(cl tclient.Client) error {
		r, e := cl.SubscribeGetUser(context.Background(), userId)
		resp = r
		return e
	})
	if err != nil {
		return "", fmt.Errorf("查询订阅号失败：%w", err)
	}
	if resp == nil || resp.Data.UserId == "" {
		msg := ""
		if resp != nil {
			msg = resp.Msg
		}
		return "", fmt.Errorf("订阅号不存在或已失效（%s）——请确认 userId 复制对了", msg)
	}
	if resp.Data.Name != "" {
		return resp.Data.Name, nil
	}
	return userId, nil
}

// tianyiUpShares 拉一页订阅号分享（按最后操作时间倒序）。
func tianyiUpShares(cred *tianyiCred, userId string, pageNum, pageSize int64) (list []tianyiSubShare, total int64, err error) {
	var resp *tclient.GetUpResourceShareResponse
	err = tianyiDo(cred, func(cl tclient.Client) error {
		r, e := cl.GetUpResourceShare(context.Background(), userId, pageNum, pageSize)
		resp = r
		return e
	})
	if err != nil {
		return nil, 0, fmt.Errorf("拉取订阅号分享失败：%w", err)
	}
	if resp == nil || resp.Data == nil {
		return nil, 0, fmt.Errorf("拉取订阅号分享返回为空")
	}
	for _, item := range resp.Data.FileList {
		list = append(list, tianyiSubShare{
			ShareId:    item.ShareId,
			RootFileId: string(item.Id),
			Name:       item.Name,
			ShareCode:  tianyiCodeFromURL(item.AccessURL),
			IsFolder:   item.Folder == 1,
			Size:       item.Size,
			ShareDate:  item.ShareDate,
		})
	}
	return list, resp.Data.Count, nil
}

// tianyiCodeFromURL 从 AccessURL 里提取分享 code。
// 订阅号资源的 AccessURL 可能是完整分享链接（https://cloud.189.cn/web/share?code=xxx），
// 也可能不是 —— 提不出来就返回空，调用方按「无 code 分享」处理。
func tianyiCodeFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if m := tianyiShareRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	// 有些形态是裸 code（6 位以上字母数字）
	if !strings.Contains(raw, "/") && !strings.Contains(raw, " ") && len(raw) >= 6 {
		return raw
	}
	return ""
}

// tianyiSubWalk 订阅号里一条分享的完整扫描（拿视频文件清单）。
// 订阅号分享无访问码；ShareMode 用库默认值 5（cloudpan189-share 同款）。
func tianyiSubWalk(cred *tianyiCred, s tianyiSubShare) (title string, files []ShareFile, err error) {
	title = s.Name
	files, err = tianyiWalkShare(cred, tianyiWalkRoot{
		ShareId:    s.ShareId,
		RootFileId: s.RootFileId,
		RootName:   s.Name,
		RootSize:   s.Size,
		IsFolder:   s.IsFolder,
		ShareCode:  s.ShareCode,
		ShareMode:  5,
	})
	if err != nil {
		return title, nil, err
	}
	return title, files, nil
}
