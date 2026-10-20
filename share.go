package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 网盘分享链接解析
//
// 目标：把「一个分享链接」变成「一串可播放的直链」。
// 115 的分享解析接口游客即可访问，因此「零占用白嫖」不需要登录账号。
// ---------------------------------------------------------------------------

const ua115Disk = "Mozilla/5.0 115disk/30.1.0"

var shareClient = &http.Client{Timeout: 25 * time.Second}

// 115 分享接口（注意：必须走 115cdn.com 域名，且 Referer 要带提取码）
const (
	api115Snap    = "https://115cdn.com/webapi/share/snap"
	api115DownURL = "https://115cdn.com/webapi/share/downurl"
)

// shareReferer 构造 115 分享的 Referer，形如：
// https://115cdn.com/s/{code}?password={pwd}&
func shareReferer(code, pwd string) string {
	return fmt.Sprintf("https://115cdn.com/s/%s?password=%s&", code, pwd)
}

// newShareReq 构造一个带齐 115 所需请求头的请求；cookie 可为空（游客态）。
func newShareReq(method, rawURL string, code, pwd, cookie string) (*http.Request, error) {
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua115Disk)
	req.Header.Set("Referer", shareReferer(code, pwd))
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return req, nil
}

// ShareFile 是分享里的一个文件或目录。
type ShareFile struct {
	ID    string `json:"id"`   // 文件 ID（目录为 cid）
	Name  string `json:"name"` // 文件名
	Size  int64  `json:"size"` // 字节数
	IsDir bool   `json:"is_dir"`
	Path  string `json:"path"` // 在分享内的路径
	Pwd   string `json:"pwd"`  // 提取码（回传给前端）
	Code  string `json:"code"` // share_code
	// FidToken 夸克分享文件的访问令牌（share_fid_token）——转存用。
	// 其他网盘为空。导入时随文件一起入库，播放转存时不必再扫分享树。
	FidToken string `json:"fid_token,omitempty"`
}

// 分享链接所能识别的域名
var shareHostRe = regexp.MustCompile(
	`(?i)(?:115\.com|115cdn\.com|anxia\.com|115pan\.com)/s/([A-Za-z0-9_-]+)`)

var shareCodeOnlyRe = regexp.MustCompile(`^[A-Za-z0-9]{6,}$`)

// parseShareLink 从分享链接中提取 share_code 与提取码。
// 支持：完整链接、带 password 参数的链接、文本里夹带的提取码、纯 share_code。
func parseShareLink(raw string) (code, pwd string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("请粘贴分享链接")
	}

	// 提取码：优先取 URL 的 password 参数，其次取文本里的「提取码：xxxx」
	if u, e := url.Parse(raw); e == nil {
		pwd = u.Query().Get("password")
		if pwd == "" {
			pwd = u.Query().Get("pwd")
		}
	}
	if pwd == "" {
		re := regexp.MustCompile(`(?:提取码|访问码|密码|password|pwd)\s*[:：]?\s*([A-Za-z0-9]{4})`)
		if m := re.FindStringSubmatch(raw); m != nil {
			pwd = m[1]
		}
	}

	if m := shareHostRe.FindStringSubmatch(raw); m != nil {
		return m[1], pwd, nil
	}
	// 只接受纯 share_code（避免把乱七八糟的文本当成 code）
	if !strings.Contains(raw, "/") && !strings.Contains(raw, " ") && shareCodeOnlyRe.MatchString(raw) {
		return raw, pwd, nil
	}
	return "", "", fmt.Errorf("无法识别这个链接，请粘贴完整的 115 分享链接（形如 https://115.com/s/xxxx?password=abcd）")
}

// ---------------------------------------------------------------------------
// 115 分享接口
// ---------------------------------------------------------------------------

// flexStr 兼容 115 接口中「同一字段有时是字符串、有时是数字」的情况
// （例如 cid 在顶层是 "123"，在子层是 123）。
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	if s == "null" {
		s = ""
	}
	*f = flexStr(s)
	return nil
}

func (f flexStr) String() string { return string(f) }

type snapItem struct {
	FID flexStr `json:"fid"` // 文件 ID（目录此项为空）
	CID flexStr `json:"cid"` // 目录 ID
	PC  flexStr `json:"pc"`  // pickcode
	N   string  `json:"n"`   // 名称
	S   int64   `json:"s"`   // 大小
}

type snapResp struct {
	State bool   `json:"state"`
	Error string `json:"error"`
	Data  struct {
		Count     int        `json:"count"`
		List      []snapItem `json:"list"`
		ShareInfo struct {
			Title string `json:"share_title"`
		} `json:"shareinfo"`
	} `json:"data"`
}

// list115Share 列出 115 分享中某一目录下的内容（游客即可）。
func list115Share(code, pwd, cid, cookie string) (*snapResp, error) {
	q := url.Values{}
	q.Set("share_code", code)
	q.Set("receive_code", pwd)
	q.Set("offset", "0")
	q.Set("limit", "200")
	if cid != "" {
		q.Set("cid", cid)
	}
	req, err := newShareReq(http.MethodGet, api115Snap+"?"+q.Encode(), code, pwd, cookie)
	if err != nil {
		return nil, err
	}

	resp, err := shareClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 115 失败：%w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out snapResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("115 返回了无法识别的数据（HTTP %d）: %.1600s", resp.StatusCode, string(body))
	}
	if !out.State {
		msg := out.Error
		if msg == "" {
			msg = "解析失败（提取码可能不正确）"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return &out, nil
}

// shareFileURL 取分享内某个文件的下载直链。
// 注意：115 的 downurl 接口要求登录态，cookie 为空时会返回「登录超时」。
func shareFileURL(code, pwd, fileID, cookie string) (string, error) {
	q := url.Values{}
	q.Set("share_code", code)
	q.Set("receive_code", pwd)
	q.Set("file_id", fileID)
	q.Set("dl", "1") // 关键参数：缺了它接口行为不同

	req, err := newShareReq(http.MethodGet, api115DownURL+"?"+q.Encode(), code, pwd, cookie)
	if err != nil {
		return "", err
	}

	resp, err := shareClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接 115 失败：%w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		State bool   `json:"state"`
		Error string `json:"error"`
		Errno int    `json:"errno"`
		Data  struct {
			URL struct {
				URL string `json:"url"`
			} `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("115 返回了无法识别的数据")
	}
	if !out.State {
		msg := out.Error
		switch {
		case out.Errno == 990001:
			msg = "115 要求登录后才能播放分享文件，请先在下方扫码登录"
		case msg == "":
			msg = "取直链失败"
		}
		return "", fmt.Errorf("%s", msg)
	}
	if out.Data.URL.URL == "" {
		return "", fmt.Errorf("115 未返回可用的播放地址")
	}
	return out.Data.URL.URL, nil
}

// ---------------------------------------------------------------------------
// 对外：解析分享链接（含一层目录展开）
// ---------------------------------------------------------------------------

var videoExts = []string{
	".mp4", ".mkv", ".avi", ".ts", ".mov", ".m4v", ".flv", ".wmv",
	".rmvb", ".rm", ".iso", ".mpg", ".mpeg", ".webm", ".m2ts", ".vob",
}

func isVideo(name string) bool {
	l := strings.ToLower(name)
	for _, e := range videoExts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

// resolveShare 解析分享链接，返回其中的视频文件列表。
// 目录会向下展开一层，方便「整个文件夹」的分享也能直接用。
// cookie 为空时为游客态（仅能列文件，播放时仍需登录）。
func resolveShare(rawLink, cookie string) (title string, files []ShareFile, err error) {
	code, pwd, err := parseShareLink(rawLink)
	if err != nil {
		return "", nil, err
	}
	return resolveShareCode(code, pwd, cookie)
}

// resolveShareCode 按分享码扫描整棵 115 分享目录树 —— 首次解析与追更刷新共用。
func resolveShareCode(code, pwd, cookie string) (title string, files []ShareFile, err error) {
	root, err := list115Share(code, pwd, "", cookie)
	if err != nil {
		return "", nil, err
	}
	title = root.Data.ShareInfo.Title
	if title == "" {
		title = code
	}

	var out []ShareFile    // 视频（优先）
	var others []ShareFile // 其它文件（视频为空时兜底）
	var dirs []snapItem

	collect := func(it snapItem, prefix string) {
		path := it.N
		if prefix != "" {
			path = prefix + "/" + it.N
		}
		f := ShareFile{ID: string(it.FID), Name: it.N, Size: it.S, Path: path, Pwd: pwd, Code: code}
		if isVideo(it.N) {
			out = append(out, f)
		} else {
			others = append(others, f)
		}
	}

	for _, it := range root.Data.List {
		switch {
		case it.FID != "":
			collect(it, "")
		case it.FID == "" && it.CID != "":
			dirs = append(dirs, it)
		}
	}

	// 展开目录（限制数量，避免一次拉太多）
	for i, d := range dirs {
		if i >= 10 || len(out)+len(others) >= 200 {
			break
		}
		sub, err := list115Share(code, pwd, string(d.CID), cookie)
		if err != nil {
			continue
		}
		for _, it := range sub.Data.List {
			if it.FID != "" {
				collect(it, d.N)
			}
		}
	}

	if len(out) == 0 {
		if len(others) == 0 {
			return title, nil, fmt.Errorf("这个分享里没有找到可播放的文件")
		}
		// 没有视频时，把全部文件列出来（可能是音乐等）
		return title, others, nil
	}
	return title, out, nil
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// mergeSharePwd 把用户在「提取码」输入框单独填的提取码合并进链接。
// 百度/115/夸克的链接解析都从 URL query 里找 pwd/password，链接里已带则不重复拼。
func mergeSharePwd(link, pwd string) string {
	pwd = strings.TrimSpace(pwd)
	if pwd == "" {
		return link
	}
	low := strings.ToLower(link)
	if strings.Contains(low, "pwd=") || strings.Contains(low, "password=") {
		return link
	}
	sep := "?"
	if strings.Contains(link, "?") {
		sep = "&"
	}
	return link + sep + "pwd=" + pwd
}

// apiShareParse 解析分享链接（自动识别网盘）：POST /api/share/parse {"link":"..."}
func apiShareParse(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Link string `json:"link"`
			Pwd  string `json:"pwd"` // 前端「提取码(选填)」输入框的值；单独填时链接里不带 pwd
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
			return
		}
		// 【2026-10-17 修复「解析失败」】前端一直有发 pwd 字段（提取码单独填的
		// 场景），但这里只读了 link —— 用户按输入框提示把提取码填在右边小框、
		// 链接框只粘裸链时，pwd 被整段丢弃，verify 跳过 → wxlist 无通行证 →
		// 9019「需要提取码」→ 前端显示"解析失败"。百度/115/夸克的链接解析都认
		// ?pwd= 参数，链接里没带就拼上去。
		link := mergeSharePwd(strings.TrimSpace(req.Link), strings.TrimSpace(req.Pwd))
		p := findProvider(link)
		if p == nil {
			writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": "暂不支持这个网盘（当前支持：" + strings.Join(providerList(), "、") + "）",
			})
			return
		}
		info, err := p.ListShareFiles(link)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":       true,
			"provider": info.Provider,
			"title":    info.Title,
			"pwd_id":   info.ShareID,
			"passcode": info.Passcode,
			"files":    info.Files,
			"logged":   info.Logged,
		})
	}
}

// apiShareAdd 把解析出的文件加入虚拟盘：POST /api/share/add
// scrapeSuffixRe 匹配尾部的 TMDB 刮削标记：[tmdbid=123] / {tmdb-123}（可多个叠加）。
// 这类标记是用户网盘目录名自带的（早年刮削工具写的），ExampleShow {tmdb-85271}
// 与 示例剧（2026）[tmdbid=279923] 是同一路子。
var scrapeSuffixRe = regexp.MustCompile(`(?:\s*(?:\[tmdbid=\d+\]|\{tmdb-\d+\}))+\s*$`)

// tmdbidRe 从任意位置提取 TMDB ID，两种格式都认；提取不到返回 ""。
var tmdbidRe = regexp.MustCompile(`tmdbid=(\d+)|tmdb-(\d+)`)

// stripScrape 剥掉名字尾部的刮削标记。
// 用途一（2026-10-17，备忘录 §22.6/§22.7 实锤）：网易爆米花深度接 TMDB，
// 见到目录名带 [tmdbid=xxx]/{tmdb-xxx} 就把当前目录渲染成剧集条目，列表里
// 多出一个「目录自己」—— 用户看到的目录套目录。虚拟目录名一律剥标记，
// 爆米花就平铺展示了。前端 cleanShow 展示时本来也剥，Show 字段保留原值，
// 追更/分享列表展示不受影响。
// 用途二：剧名与分享目录名做同名折叠判断前，先剥标记再比。
func stripScrape(s string) string {
	return strings.TrimSpace(scrapeSuffixRe.ReplaceAllString(strings.TrimSpace(s), ""))
}

// tmdbidOf 提取刮削标记里的 TMDB ID，没有返回 ""。
func tmdbidOf(s string) string {
	m := tmdbidRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// buildItemDir 构造条目的虚拟目录：{网盘}[/{分类}]/{剧名}[/{分享内子目录}]。
//
// 导入与追更刷新共用 —— 目录规则只写这一处，绝不允许两边各写一份
// （上次就是导入漏了"同名嵌套折叠"，刷新修了导入没修，结果又套了一层娃）。
//
// 折叠规则（2026-10-17 升级，备忘录 §22.7）：分享内路径的第一段若与剧名
// 「剥刮削标记后同名」或「同 tmdbid」，判定为同一部剧的两种命名（用户在
// bigpan 起规范名 + 网盘里的原名/变形名），整段去掉。
// 病历：用户把「Z==剧（2026）」改名成规范名「示例剧（2026）」，两边字符串
// 不相等老规则折不掉 → 规范名下面套着变形名，两层目录，共 7 部 50 个文件。
// 目录名所有段一律剥尾部刮削标记（stripScrape 用途一）。
func buildItemDir(provider, category, showName, filePath string) string {
	dir := providerCN(provider)
	// 【2026-10-17 修潜伏 bug】safeFilename("") 返回「未命名」而非空串，
	// 老的 `if c := safeFilename(...); c != ""` 对空 category 恒真 ——
	// 用户导入不选分类就会莫名多出一层「未命名」目录。先判空再净化。
	// 【2026-10-20 两级分类】category 可能是「父/子」，按段拼出多层目录。
	if cat := strings.TrimSpace(category); cat != "" {
		for _, seg := range strings.Split(cat, "/") {
			if seg = strings.TrimSpace(seg); seg != "" {
				dir += "/" + safeFilename(seg)
			}
		}
	}
	show := safeFilename(stripScrape(strings.TrimSpace(showName)))
	if show == "" {
		show = "未命名"
	}
	dir += "/" + show

	rel := strings.Trim(filepath.ToSlash(filepath.Dir(filePath)), "/")
	if rel == "." || rel == "" {
		return dir
	}
	segs := strings.Split(rel, "/")
	if len(segs) > 0 {
		seg0 := segs[0]
		if safeFilename(stripScrape(seg0)) == show ||
			(tmdbidOf(seg0) != "" && tmdbidOf(seg0) == tmdbidOf(showName)) {
			segs = segs[1:]
		}
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		dir += "/" + safeFilename(stripScrape(seg))
	}
	return dir
}

func apiShareAdd(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			Title    string `json:"title"`    // 分享（剧）名，入库的剧名目录
			Category    string `json:"category"` // 分类（可选，可新建；空 = 不分类）
			AutoRefresh *bool  `json:"auto_refresh"` // 导入后是否开启追更（默认开）
			PwdID       string `json:"pwd_id"`
			Passcode string `json:"passcode"`
			Files    []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Path string `json:"path"`
				Size int64  `json:"size"`
				// FidToken 夸克分享令牌（share_fid_token）——前端原样带回，
				// 仅供夸克 TV 302 播放时转存用，其他网盘为空。
				FidToken string `json:"fid_token"`
			} `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Files) == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "没有要添加的文件"})
			return
		}
		// 虚拟盘目录统一由 buildItemDir 构造（分类/剧名/嵌套折叠都在里面）
		// 分类支持两级「父/子」（如 电视剧/国产剧），在这里统一规范化
		category, catErr := normalizeCategory(req.Category)
		if catErr != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": catErr.Error()})
			return
		}
		showNameRaw := strings.TrimSpace(req.Title)
		showFinal := safeFilename(showNameRaw)
		if showFinal == "" {
			showFinal = "未命名"
		}
		batch := make([]Item, 0, len(req.Files))
		for _, f := range req.Files {
			if f.ID == "" {
				continue
			}
			dir := buildItemDir(req.Provider, category, req.Title, f.Path)
			// 保留真实扩展名 —— 这是播放器选择解析器的唯一依据。
			// 曾因强行改成 .mp4 导致 MKV 文件全部无法播放。
			ext := strings.ToLower(filepath.Ext(f.Name))
			if ext == "" {
				ext = strings.ToLower(filepath.Ext(f.Path))
			}
			title := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
			}
			batch = append(batch, Item{
				Title:    title,
				Dir:      dir,
				Category: category,
				Show:     showFinal,
				Provider: req.Provider,
				ShareID:  req.PwdID,
				Passcode: req.Passcode,
				FileID:   f.ID,
				FidToken: f.FidToken, // 夸克 TV 302 转存用（其他网盘为空）
				Ext:      ext,
				Size:     f.Size,
			})
		}
		// 一次落盘：导入几百集不再逐条重写 items.json
		added := store.AddBatch(batch)
		// 登记追更（默认开启；完结剧可在导入时取消，或之后在列表里切换/移除）
		if globalShares != nil {
			ar := true
			if req.AutoRefresh != nil {
				ar = *req.AutoRefresh
			}
			show := safeFilename(strings.TrimSpace(req.Title))
			if show == "" {
				show = "未命名"
			}
			globalShares.Put(ShareMeta{
				Provider: req.Provider, ShareID: req.PwdID, Passcode: req.Passcode,
				Category: category,
				Show:     show,
				AutoRefresh: ar,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "added": added})
		// 强同步：导入成功后立刻在后台重建 STRM，新剧集马上出现在媒体库目录里。
		rebuildSTRMAsync(store, self, "导入分享")
	}
}

// ---------------------------------------------------------------------------
// 追更刷新
// ---------------------------------------------------------------------------

// apiShares 列出已导入的分享（聚合自条目，合并追更登记）。
//
// 支持分页：?page=1&size=50。不传 size 时返回全部（兼容脚本与老前端）。
// 排序：最近有新增的排在前面。
func apiShares(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			ShareSummary
			AutoRefresh bool `json:"auto_refresh"`
		}
		list := store.Shares()
		out := make([]row, 0, len(list))
		for _, s := range list {
			if m := globalShares.Get(s.Provider, s.ShareID); m != nil && m.Hidden {
				continue // 用户移除过的分享不再显示（条目仍在媒体库）
			}
			it := row{ShareSummary: s, AutoRefresh: true}
			if m := globalShares.Get(s.Provider, s.ShareID); m != nil {
				it.AutoRefresh = m.AutoRefresh
				if m.Show != "" {
					it.ShowName = m.Show
				}
				if m.Category != "" {
					it.Category = m.Category
				}
			}
			out = append(out, it)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].LastAdded > out[j].LastAdded })

		// 全部分类（改分类弹窗的下拉选项）：分页前聚合，不受 page/size 影响
		catSet := map[string]bool{}
		for _, s := range out {
			if s.Category != "" {
				catSet[s.Category] = true
			}
		}
		cats := make([]string, 0, len(catSet))
		for c := range catSet {
			cats = append(cats, c)
		}
		sort.Strings(cats)

		total := len(out)
		page, size := 1, 0 // size=0 → 不分页，返回全部
		if v := r.URL.Query().Get("page"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				page = n
			}
		}
		if v := r.URL.Query().Get("size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				size = n
				if size > 500 {
					size = 500
				}
			}
		}
		if size > 0 {
			start := (page - 1) * size
			if start > total {
				start = total
			}
			end := start + size
			if end > total {
				end = total
			}
			out = out[start:end]
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "shares": out, "total": total, "page": page, "size": size,
			"categories": cats,
		})
	}
}

// apiShareToggle 切换某个分享的追更开关（追更中 ↔ 已完结）。
func apiShareToggle(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider    string `json:"provider"`
			ShareID     string `json:"share_id"`
			AutoRefresh bool   `json:"auto_refresh"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ShareID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		m := globalShares.Get(req.Provider, req.ShareID)
		if m == nil {
			// 没有登记的老分享：从条目聚合补一份
			m = &ShareMeta{Provider: req.Provider, ShareID: req.ShareID}
			for _, it := range store.List() {
				if it.Provider == req.Provider && it.ShareID == req.ShareID {
					m.Passcode = it.Passcode
					m.Category = it.Category
					m.Show = it.Show
					if m.Show == "" && it.Dir != "" {
						parts := strings.Split(it.Dir, "/")
						if len(parts) >= 2 {
							m.Show = parts[len(parts)-2]
							if len(parts) >= 3 && m.Category == "" {
								m.Category = parts[1]
							}
						}
					}
					break
				}
			}
		}
		m.AutoRefresh = req.AutoRefresh
		globalShares.Put(*m)
		writeJSON(w, map[string]interface{}{"ok": true, "auto_refresh": m.AutoRefresh})
	}
}

// apiShareDelete 把分享从追更列表移除（用户说的"清除"）。
// 实现是打 Hidden 标记而不是删登记：追更列表按条目聚合，只删登记的话行还在
// （用户点了没反应就是这个坑）。条目本身保留——还能在媒体库里播放，
// 想彻底删剧请去媒体库整剧删除。重新导入同一分享会复活该行。
func apiShareDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 路径：/api/shares/{provider}/{share_id}
		rest := strings.TrimPrefix(r.URL.Path, "/api/shares/")
		kv := strings.SplitN(rest, "/", 2)
		if len(kv) != 2 || kv[1] == "" {
			http.Error(w, "参数错误", http.StatusBadRequest)
			return
		}
		provider, shareID := kv[0], kv[1]
		m := globalShares.Get(provider, shareID)
		if m == nil {
			// 没有登记的老分享：从条目聚合补一份，顺便带上剧名/分类/提取码，
			// 记录更完整（将来重新导入时 Put 会整体覆盖，无残留影响）。
			m = &ShareMeta{Provider: provider, ShareID: shareID}
			for _, it := range store.List() {
				if it.Provider == provider && it.ShareID == shareID {
					m.Passcode = it.Passcode
					m.Category = it.Category
					m.Show = it.Show
					break
				}
			}
		}
		m.Hidden = true
		globalShares.Put(*m)
		writeJSON(w, map[string]interface{}{"ok": true})
	}
}

// ---- 追更刷新：手动（UI 按钮）与定时自动共用同一套逻辑 ----

// refreshMu 让所有追更刷新全局串行。
// 分享重扫要打好几下网盘接口（verify/wxlist/逐层列目录），并发跑既容易被
// 网盘限流，又可能让 store 的"集名防重复"判断看串行。分享就十几个，
// 逐个刷一轮也就一两分钟，串行完全够快，还顺带杜绝了同一分享被并发重扫。
var refreshMu sync.Mutex

// refreshShareOnce 重扫一个分享，把新出的集数增量入库。
// 原则：只增不删 —— 分享里删掉的旧文件不动，已入库条目路径绝不变化。
// 返回剧名、新增集数、扫描到的视频总数。
// 分类/剧名沿用已入库条目的值（用户手工改过名、设过分类都保留）；
// 显式传入的 category 优先。passcode 为空表示无提取码分享。
func refreshShareOnce(store *Store, provider, shareID, passcode, category string) (show string, added, total int, err error) {
	p := providerByName(provider)
	if p == nil {
		return "", 0, 0, fmt.Errorf("不支持的网盘: %s", provider)
	}
	title, files, err := p.RefreshShare(shareID, passcode)
	if err != nil {
		return "", 0, 0, err
	}
	// 分类与剧名沿用已入库条目记录的值 —— 用户手工改过名、设过分类都保持不变，
	// 路径稳定，播放器缓存才不会失效。显式传入的 category 优先。
	cat := strings.TrimSpace(category)
	existTitles := map[string]bool{} // 同剧下已有的「集名」，用于防重复
	for _, it := range store.List() {
		if it.Provider == provider && it.ShareID == shareID && it.Dir != "" {
			if it.Show != "" && show == "" {
				show = it.Show
				if cat == "" {
					cat = it.Category
				}
			}
			existTitles[it.Title] = true
		}
	}
	if show == "" {
		show = safeFilename(title)
	}
	for _, f := range files {
		if f.ID == "" {
			continue
		}
		total++
		// 集名去重：同一部剧下，集名相同就视为同一集 ——
		// 分享主常在根目录和子文件夹里各放一份，路径不同但内容重复。
		name := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		if name == "" {
			continue
		}
		if existTitles[name] {
			continue
		}
		// 目录构造统一走 buildItemDir（分类/剧名/同名嵌套折叠都在里面）
		dir := buildItemDir(provider, cat, show, f.Path)
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext == "" {
			ext = strings.ToLower(filepath.Ext(f.Path))
		}
		full := dir + "/" + safeFilename(name) + ext
		if store.FindByPath(full) != nil {
			continue // 已在库里
		}
		store.Add(Item{
			Title: name, Dir: dir,
			Category: cat, Show: show,
			Provider: provider, ShareID: shareID, Passcode: passcode,
			FileID: f.ID, Ext: ext, Size: f.Size,
		})
		existTitles[name] = true
		added++
	}
	log.Printf("[追更] %s/%s 重扫 %d 个视频，新增 %d 个", provider, show, total, added)
	return show, added, total, nil
}

// apiShareRefresh 追更刷新（手动）：重扫分享目录树，增量加入新文件。
func apiShareRefresh(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			ShareID  string `json:"share_id"`
			Passcode string `json:"passcode"`
			Category string `json:"category"` // 可选：显式指定分类（不传则沿用已入库的）
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ShareID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		refreshMu.Lock()
		show, added, total, err := refreshShareOnce(store, req.Provider, req.ShareID, req.Passcode, req.Category)
		refreshMu.Unlock()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		// 有新集才同步 STRM，规矩与定时刷新一致（见 autoRefreshCycle）：
		// 没新集就别折腾磁盘（增量同步虽不重写旧文件，走一遍也要几秒）。
		// 【2026-10-18 修缺口】此前手动刷新只入库不重建——新集进了数据库却
		// 没有 .strm 文件，飞牛影视根本看不到；而下一轮定时刷新这部剧已
		// 没有新集（added=0），照样不重建，新集就一直缺着。
		if added > 0 {
			rebuildSTRMAsync(store, self, "手动刷新")
		}
		writeJSON(w, map[string]interface{}{"ok": true, "total": total, "added": added, "show": show})
	}
}

// ---- 定时自动刷新（追更定时器） ----

// startShareRefresher 启动追更定时器：每隔 every 把所有「追更中」的分享自动重扫一遍。
// every <= 0 表示关闭（保持以前的手动刷新行为）。
// 首轮延迟 5 分钟再跑（容器刚启动，网盘接口/账号态都还没稳定，且避免重启惊跳）；
// 之后严格按间隔到点就刷。单轮内分享逐个串行；某个分享失败只记日志，
// 不影响同轮其他分享，也不影响后续轮次。
func startShareRefresher(store *Store, every time.Duration, self string) {
	if every <= 0 || globalShares == nil {
		return
	}
	go func() {
		first := 5 * time.Minute
		if every < first {
			first = every
		}
		log.Printf("[追更] 定时自动刷新已开启：%v 后首轮，之后每 %v 刷一遍全部追更中的分享（只增不删，不动老条目）", first, every)
		time.Sleep(first)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			autoRefreshCycle(store, self)
			<-t.C
		}
	}()
}

// autoRefreshCycle 跑一轮定时刷新：遍历全部登记，只刷 auto_refresh=true 的。
func autoRefreshCycle(store *Store, self string) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	metas := globalShares.List()
	on, done, newEp := 0, 0, 0
	for _, m := range metas {
		if !m.AutoRefresh || m.Hidden {
			continue // 没开追更的、已从列表移除的，都不刷
		}
		on++
		if _, added, _, err := refreshShareOnce(store, m.Provider, m.ShareID, m.Passcode, m.Category); err != nil {
			// 单个分享失败（网盘抽风/登录过期）不拖垮整轮，下轮再试
			log.Printf("[追更] %s/%s 自动刷新失败（下轮重试）: %v", m.Provider, m.Show, err)
			continue
		} else {
			done++
			newEp += added
		}
	}
	if on > 0 {
		log.Printf("[追更] 本轮定时刷新完成：追更中 %d 个，成功 %d 个，新增 %d 集", on, done, newEp)
	}
	// 有新集才重建 STRM（没变化就不折腾磁盘，N1 上几百个文件也要一两秒）
	// force=false：地址变过时只删孤儿、写新文件，存量等用户手动全量重建
	if newEp > 0 && self != "" && strmCfg.AutoRebuild {
		strmMu.Lock()
		defer strmMu.Unlock()
		if n, err := buildSTRM(store, self, false); err != nil {
			log.Printf("[strm] 追更后重建失败: %v", err)
		} else {
			log.Printf("[strm] 追更后已重建：%d 个", n)
		}
	}
}

// apiSharePurge 人工确认某分享已失效：立即删条目 + 登记 + 重建 STRM。
//
// 与巡检的区别：巡检要连续两轮确认（防误删），这里是用户自己拍板，立即执行。
// body: {"provider":"baidu","share_id":"xxx"}
func apiSharePurge(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			ShareID  string `json:"share_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" || req.ShareID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		m := shareMetaFor(store, req.Provider, req.ShareID)
		eps := purgeShare(store, m)
		log.Printf("[手动清理] 用户确认 %s/%s 已失效，删除 %d 个条目", m.Provider, m.Show, eps)
		writeJSON(w, map[string]interface{}{"ok": true, "deleted": eps, "show": m.Show})
		rebuildSTRMAsync(store, self, "手动清理失效分享")
	}
}

// shareMetaFor 取登记；没有登记（老数据）时从条目里补剧名，方便日志与清理。
func shareMetaFor(store *Store, provider, shareID string) ShareMeta {
	if globalShares != nil {
		if m := globalShares.Get(provider, shareID); m != nil {
			return *m
		}
	}
	m := ShareMeta{Provider: provider, ShareID: shareID}
	for _, it := range store.List() {
		if it.Provider == provider && it.ShareID == shareID {
			m.Show = it.Show
			m.Category = it.Category
			m.Passcode = it.Passcode
			break
		}
	}
	return m
}

// apiShowDelete 按条目 ID 批量删除（媒体库里"删除整个剧/目录"用）。
// 前端传 ID 列表而不是路径，避免路径转义不一致的坑。
// 已入库条目删除后，涉及分享若一条不剩，其追更登记也一并清理。
func apiShowDelete(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		n := 0
		for _, id := range req.IDs {
			if store.Delete(id) {
				n++
			}
		}
		// 清理空壳的追更登记
		if globalShares != nil {
			for _, m := range globalShares.List() {
				cnt := 0
				for _, it := range store.List() {
					if it.Provider == m.Provider && it.ShareID == m.ShareID {
						cnt++
					}
				}
				if cnt == 0 {
					globalShares.Delete(m.Provider, m.ShareID)
				}
			}
		}
		log.Printf("[删除] 批量移除 %d 个条目", n)
		writeJSON(w, map[string]interface{}{"ok": true, "deleted": n})
		// 强同步：条目删了，对应的 .strm 也要马上消失。
		if n > 0 {
			rebuildSTRMAsync(store, self, "删除条目")
		}
	}
}
