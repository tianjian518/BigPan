package main

// ---------------------------------------------------------------------------
// 百度网盘（分享 → 转存到临时目录 → DLNA 直链 → 302）
//
// 【2026-10-16 重大修正】新版百度分享解析必须走 share/wxlist，不是旧的 share/list。
// 全部环节已在沙箱用真实分享链接实测确认（见下）。
//
// 完整链路：
//   ① 分享解析（游客态，无需登录）：
//        verify  : POST share/verify?surl=<去1的surl>  body pwd=<pwd>  → randsk(=sekey)
//        wxlist  : POST share/wxlist?channel=weixin&version=2.2.2&clienttype=25&web=1
//                  cookie BDCLND=<randsk 原样透传，切勿 URL-decode>
//                  body: root=1(列分享根,不带dir) / root=0+dir=<path>(列子目录)
//                        shorturl=<带1的完整surl>  pwd=<pwd>
//                  → 返回文件列表 + shareid + uk
//   ② 播放时：share/transfer 把分享文件「转存」到本账号临时目录（占一点空间，播完清）
//   ③ 取流：api/mediainfo?clienttype=80&origin=dlna 取 DLNA 直链（不绑 Cookie、可 302）
//   ④ WebDAV/直链端点发现 Wants302()=true → 直接 302 跳直链，数据不经过本机带宽
//
// 实测关键点（务必保留，别再改错）：
//   - verify 的 surl 参数 = 【去掉前导1】；wxlist 的 shorturl = 【带前导1的完整surl】
//   - 通行证 cookie = BDCLND=<randsk 原样>；不要手设 sekey，也不要 URL-decode
//     （decode 后 transfer 报 errno=200025，详见 baiduBdclnd 注释）
//   - 列子目录的 dir 必须用服务端返回的精确 path（中文/特殊字符不能手打，否则 -130 mis_2）
//   - 取流(mediainfo)必须带登录 Cookie；返回的 dlink 才不绑 Cookie、可 302
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// baiduNetUA 分享解析 / 取流 / 写操作用的安卓端 UA。
	// 【2026-10-16 盒子实测】百度 api/ 系列接口（gettemplatevariable/quota/create/transfer/list/filemanager）
	// 只认这个 netdisk 客户端 UA；用浏览器 UA 会被判定未登录返回 errno -6。
	baiduNetUA = "netdisk;P2SP;2.2.91.136;android-android;"
	// baiduTmpBase 转存临时目录（建不成则降级根目录）。
	// 转存临时目录（百度网盘根目录下）。
	// 2026-10-17 品牌统一：panboost_tmp → bigpan_tmp。
	// 旧目录不删除、仍会被清理（兼容历史残留），空目录留在盘里无害。
	baiduTmpBase = "/bigpan_tmp"
	// baiduTmpBaseLegacy 改名前的旧临时目录，启动清理时一并扫掉。
	baiduTmpBaseLegacy = "/panboost_tmp"
)

// 百度接口地址用 var 而非 const：单测里可以指向 httptest 假服务器
// （baiduWalk 的三层套娃回归测试就靠这个，见 provider_baidu_walk_test.go）。
var (
	// baiduWxListURL 新版分享列表（POST form）。
	baiduWxListURL = "https://pan.baidu.com/share/wxlist?channel=weixin&version=2.2.2&clienttype=25&web=1"
	// baiduShareVerifyURL 提取码校验（POST pwd，surl 用去前导1）。
	baiduShareVerifyURL = "https://pan.baidu.com/share/verify?t=%s&surl=%s"
	// baiduTransferURL 转存（旧版 GET 接口，只需登录 Cookie，无需 bdstoken）。
	baiduTransferURL = "https://pan.baidu.com/share/transfer?from=%s&shareid=%s&ondup=newcopy&path=%s&fsidlist=[%s]"
	// baiduMediainfoURL DLNA 直链（path 需 URL 编码）。
	baiduMediainfoURL = "https://pan.baidu.com/api/mediainfo?type=M3U8_AUTO_264&path=%s&clienttype=80&origin=dlna"
	// baiduTmplVarURL 取 bdstoken（建目录 / 删除要用）。
	baiduTmplVarURL = "https://pan.baidu.com/api/gettemplatevariable?clienttype=0&app_id=250528&web=1&fields=%5B%22bdstoken%22%5D"
	// baiduCreateURL 建目录（带 bdstoken，失败则降级根目录）。
	baiduCreateURL = "https://pan.baidu.com/api/create?a=commit&channel=chunlei&web=1&app_id=250528&bdstoken=%s&clienttype=0&logid=%s"
	// baiduFileManagerURL 删除临时文件（opera=delete，用验证过的完整参数集）。
	baiduFileManagerURL = "https://pan.baidu.com/api/filemanager?opera=delete&bdstoken=%s&onnest=fail&newVerify=1&clienttype=0&app_id=250528&web=1"
)

// baiduShareRe 识别百度分享链接（两种形态：/share/init?surl= 与 /s/1xxxx）。
var baiduShareRe = regexp.MustCompile(`(?i)pan\.baidu\.com/(?:share/(?:init)?|s/)`)

// baiduBdstokenRe 从 gettemplatevariable 响应里抠 bdstoken（兼容 data 为对象或数组）。
var baiduBdstokenRe = regexp.MustCompile(`(?i)"bdstoken"\s*:\s*"([^"]+)"`)

// baiduPwdRe 从文本里夹带的提取码。
var baiduPwdRe = regexp.MustCompile(`(?:提取码|访问码|密码|pwd)\s*[:：]?\s*([A-Za-z0-9]{4})`)

// ---- 新版 wxlist 响应 ----

type baiduWxFile struct {
	FsID           flexStr `json:"fs_id"` // ⚠️ 根目录项是字符串、子目录文件项是数字，必须柔性类型
	Path           string  `json:"path"`
	ServerFilename string  `json:"server_filename"`
	Size           flexStr `json:"size"`        // 同样可能数字/字符串混用
	IsDir          flexStr `json:"isdir"`       // "1"=目录 "0"=文件
	ServerMtime    flexStr `json:"server_mtime"` // 可能数字
}

type baiduWxListResp struct {
	Errno   int64           `json:"errno"`
	ErrType json.RawMessage `json:"errtype"` // 有时是数字 0、有时是字符串 "mis_2"，用 RawMessage 兼容
	Data    struct {
		Title    string        `json:"title"`
		FileNums int           `json:"fileNums"`
		HasMore  bool          `json:"has_more"`
		List     []baiduWxFile `json:"list"`
		UK       int64         `json:"uk"`
		ShareID  int64         `json:"shareid"`
	} `json:"data"`
}

// ---- 转存响应 ----

type baiduTransferResp struct {
	Errno   int64  `json:"errno"`
	ErrMsg  string `json:"err_msg"`
	ShowMsg string `json:"show_msg"` // 200025 等的说明文字在这里（err_msg 常为空）
	TaskID  int64  `json:"task_id"`
	Info   []struct {
		FsID           flexStr `json:"fs_id"` // 不同版本接口可能返回字符串或数字，柔性兼容
		Path           string  `json:"path"`
		ServerFilename string  `json:"server_filename"`
	} `json:"info"`
	// extra.list[0].to 才是转存落地的真实路径（info[0].path 是分享内源路径，别用错）。
	Extra struct {
		List []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"list"`
	} `json:"extra"`
}

// ---- mediainfo 响应 ----

type baiduMediainfoResp struct {
	Errno  int64           `json:"errno"`
	ErrMsg string          `json:"err_msg"`
	Info   json.RawMessage `json:"info"`
}

// ===========================================================================
// Provider 实现
// ===========================================================================

type baiduProvider struct{}

func init() { registerProvider(baiduProvider{}) }

func (baiduProvider) Name() string { return "baidu" }

func (baiduProvider) MatchLink(link string) bool {
	return baiduShareRe.MatchString(link)
}

// Wants302 百度 DLNA 直链不绑 Cookie、支持 Range、可 302 —— 数据直连，省本机带宽。
func (baiduProvider) Wants302() bool { return true }

// baiduAccountCookie 取已导入的百度登录 Cookie（裸 BDUSS 或整段均可）。
func baiduAccountCookie() string {
	if globalAccounts == nil {
		return ""
	}
	if acc := globalAccounts.Get("baidu"); acc != nil {
		return baiduCookie(acc.Cookie)
	}
	return ""
}

// baiduSurlNo1 去掉 surl 前导的 1（verify 用）：1KUz... → KUz...
func baiduSurlNo1(surl string) string { return strings.TrimPrefix(surl, "1") }

// baiduBdclnd 把 verify 返回的 randsk 原样作为 BDCLND cookie 值。
// 【2026-10-17 盒子实测·重大翻车教训】百度 transfer 只认 verify 返回的
// 【原始编码串】。randsk 形如 "xxx%2Byyy%3D"（base64 的 + / = 被百分号编码），
// 曾依"URL-decode 后放 cookie"的注释做解码，结果 transfer 返回 errno=200025
// 「提取码输入错误」——而 wxlist 两种形态都认（pwd 在 body 里，校验宽松），
// 所以该 bug 只在 randsk 恰好含 %2B/%3D/%2F 时炸（概率约 87%），
// 表现为"时间性抽风：昨天能转存、今天不能"，极易误判成网盘侧变更。
// 浏览器行为即原样透传（document.cookie 存什么就发什么），勿再"好心"解码。
func baiduBdclnd(randsk string) string {
	return randsk
}

// baiduParseShareLink 从链接里抠出「带前导1的完整 surl」与提取码。
// 完整 surl 直接用作 wxlist 的 shorturl；verify 时再 baiduSurlNo1 去1。
func baiduParseShareLink(raw string) (surFull, pwd string, err error) {
	raw = strings.TrimSpace(raw)
	u, e := url.Parse(raw)
	if e != nil {
		return "", "", fmt.Errorf("链接无法解析")
	}
	if p := u.Query().Get("pwd"); p != "" {
		pwd = p
	}
	if pwd == "" {
		if m := baiduPwdRe.FindStringSubmatch(raw); m != nil {
			pwd = m[1]
		}
	}
	if s := u.Query().Get("surl"); s != "" {
		surFull = s
	} else {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != "" {
				surFull = parts[i]
				break
			}
		}
	}
	if surFull == "" {
		return "", "", fmt.Errorf("无法识别百度分享链接（应形如 https://pan.baidu.com/s/1xxxx 或 .../share/init?surl=xxxx）")
	}
	return surFull, pwd, nil
}

// baiduVerify 校验提取码，返回 randsk（新版 sekey）。无提取码时返回 ""。
// 注意：verify 的 surl 参数必须用【去前导1】的 surl。
func baiduVerify(surFull, pwd string) (randsk string, err error) {
	if pwd == "" {
		return "", nil
	}
	ts := fmt.Sprintf("%d", time.Now().UnixNano()/int64(time.Millisecond))
	u := fmt.Sprintf(baiduShareVerifyURL, ts, url.QueryEscape(baiduSurlNo1(surFull)))
	form := url.Values{}
	form.Set("pwd", pwd)
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := httpClient.Do(req)
	if e != nil {
		return "", fmt.Errorf("百度校验提取码失败：%w", e)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Errno  int64  `json:"errno"`
		ErrMsg string `json:"err_msg"`
		Randsk string `json:"randsk"` // 新版返回字段叫 randsk（不是 data.sekey）
	}
	if json.Unmarshal(body, &out) != nil {
		return "", fmt.Errorf("百度校验返回无法解析")
	}
	switch out.Errno {
	case 0:
		if out.Randsk == "" {
			return "", fmt.Errorf("校验通过但未拿到 randsk")
		}
		return out.Randsk, nil
	case -9:
		return "", fmt.Errorf("提取码错误，请检查后重试")
	default:
		return "", fmt.Errorf("校验失败（errno=%d）", out.Errno)
	}
}

// baiduWxList 调新版分享列表接口。
//   randsk：verify 拿到的 randsk（为空表示无提取码分享）
//   surFull：带前导1的完整 surl（shorturl 参数）
//   root=1 列分享根（dir 忽略）；root=0 需传精确 dir
func baiduWxList(randsk, surFull, pwd, dir string, root string) (*baiduWxListResp, error) {
	form := url.Values{}
	form.Set("shorturl", surFull)
	form.Set("pwd", pwd)
	form.Set("root", root)
	form.Set("num", "100")
	form.Set("order", "time")
	form.Set("page", "1")
	if root == "0" && dir != "" {
		form.Set("dir", dir)
	}
	req, _ := http.NewRequest(http.MethodPost, baiduWxListURL, strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if randsk != "" {
		// 通行证：randsk 原样放 BDCLND cookie（URL-decode 会导致 transfer 200025）。
		req.Header.Set("Cookie", "BDCLND="+baiduBdclnd(randsk))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接百度分享失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out baiduWxListResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("百度分享返回无法识别（HTTP %d）: %.300s", resp.StatusCode, string(body))
	}
	switch out.Errno {
	case 0:
		return &out, nil
	case 9019:
		return nil, fmt.Errorf("该分享需要提取码或校验未通过（errno=9019）")
	case -130:
		// -130 = 分享已被取消/删除。文案必须含「分享已失效」：
		// 巡检器（health.go）靠关键词区分「真失效」和「网络抽风」。
		return nil, fmt.Errorf("分享已失效（原分享被取消或删除，errno=-130 %s）", out.ErrType)
	case 105:
		return nil, fmt.Errorf("分享已失效（链接不存在，errno=105）")
	case -9:
		return nil, fmt.Errorf("提取码错误（errno=-9）")
	default:
		return nil, fmt.Errorf("解析分享失败（errno=%d %s）", out.Errno, out.ErrType)
	}
}

// baiduWalk 递归扫描整棵分享目录树，收集视频文件（含 shareid/uk 供转存）。
// 【2026-10-17 修复「三层套娃分享解析失败」】原来是「只展开一层」（照抄的 115
// 老逻辑），而百度新剧分享常见三层结构：分享根 / 剧名目录 / 【X】剧名目录 /
// 视频文件。第二层仍是目录，一层展开什么都收不到，直接报「这个分享里没有
// 找到可播放的视频文件」——用户实踩两个：1AbCdEfGhIjKlMnOpQrStUv（示例剧A）、
// 1XyZ0123456789abcdefghij（示例剧B），盒子上 Python 逐层复刻确认第三层才有视频。
// 现改为 BFS 递归整棵树（与夸克 scanQuarkShare 同模式）：目录 200 个 /
// 文件 500 个上限，防超大分享把解析拖死；dir 参数必须用服务端返回的精确 path。
func baiduWalk(surFull, randsk, pwd string) (shareid, uk int64, shareRoot, title string, files []ShareFile, err error) {
	root, e := baiduWxList(randsk, surFull, pwd, "", "1")
	if e != nil {
		return 0, 0, "", "", nil, e
	}
	shareid, uk = root.Data.ShareID, root.Data.UK
	title = root.Data.Title
	if title == "" {
		title = surFull
	}
	// 分享根目录名 = title 的父目录（如 "/【影视Cut】/Z..." → "/【影视Cut】"）。
	shareRoot = path.Dir(title)
	if shareRoot == "." || shareRoot == "/" {
		shareRoot = ""
	}

	collect := func(f baiduWxFile, prefix string) {
		name := f.ServerFilename
		rel := strings.TrimPrefix(f.Path, shareRoot)
		rel = strings.TrimPrefix(rel, "/")
		if prefix != "" {
			rel = prefix + "/" + name
		}
		files = append(files, ShareFile{
			ID:   f.FsID.String(),
			Name: name,
			Size: parseSize(f.Size.String()),
			Path: rel,
		})
	}
	// relOf 把服务端返回的完整 path 转成相对 shareRoot 的路径（拼 prefix 用）。
	// 用服务端 path 裁剪而不是手工拼名：中文/特殊字符（丨/-/【】/空格）绝不会错。
	relOf := func(p string) string {
		return strings.TrimPrefix(strings.TrimPrefix(p, shareRoot), "/")
	}

	var dirs []baiduWxFile
	for _, f := range root.Data.List {
		if f.IsDir == "1" {
			dirs = append(dirs, f)
		} else if isVideo(f.ServerFilename) {
			collect(f, "")
		}
	}
	log.Printf("[baidu] 分享 %q 根目录 %d 项（目录 %d），shareid=%d uk=%d",
		title, len(root.Data.List), len(dirs), shareid, uk)
	// 展开子目录：BFS 递归整棵分享目录树（对齐夸克 scanQuarkShare 的模式）。
	type dirJob struct {
		dirPath, prefix string // dirPath=服务端精确 path；prefix=该目录的相对路径
	}
	queue := make([]dirJob, 0, len(dirs))
	seen := map[string]bool{}
	for _, d := range dirs {
		if !seen[d.Path] && len(seen) < 200 {
			seen[d.Path] = true
			queue = append(queue, dirJob{dirPath: d.Path, prefix: relOf(d.Path)})
		}
	}
	for len(queue) > 0 && len(files) < 500 && len(seen) < 200 {
		job := queue[0]
		queue = queue[1:]
		sub, e2 := baiduWxList(randsk, surFull, pwd, job.dirPath, "0")
		if e2 != nil {
			log.Printf("[baidu] 子目录 %q 列取失败: %v", job.dirPath, e2)
			continue
		}
		for _, f := range sub.Data.List {
			if f.IsDir == "1" {
				if !seen[f.Path] && len(seen) < 200 {
					seen[f.Path] = true
					queue = append(queue, dirJob{dirPath: f.Path, prefix: relOf(f.Path)})
				}
				continue
			}
			if isVideo(f.ServerFilename) {
				collect(f, job.prefix)
			}
		}
	}
	if len(files) == 0 {
		return shareid, uk, shareRoot, title, nil, fmt.Errorf("这个分享里没有找到可播放的视频文件")
	}
	return shareid, uk, shareRoot, title, files, nil
}

func parseSize(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// ListShareFiles 解析分享链接（无需登录）：列出其中的视频文件。
func (baiduProvider) ListShareFiles(link string) (ShareInfo, error) {
	surFull, pwd, err := baiduParseShareLink(link)
	if err != nil {
		return ShareInfo{}, err
	}
	randsk, err := baiduVerify(surFull, pwd)
	if err != nil {
		return ShareInfo{}, err
	}
	_, _, _, title, files, err := baiduWalk(surFull, randsk, pwd)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: "baidu",
		Title:    title,
		ShareID:  surFull, // 存带1的完整 surl（ResolvePlay 里据此派生）
		Passcode: pwd,
		Files:    files,
		Logged:   baiduAccountCookie() != "",
	}, nil
}

// RefreshShare 追更刷新：凭完整 surl + 提取码重扫整棵分享目录树。
func (baiduProvider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	randsk, err := baiduVerify(shareID, passcode)
	if err != nil {
		return "", nil, err
	}
	_, _, _, title, files, err := baiduWalk(shareID, randsk, passcode)
	if err != nil {
		return "", nil, err
	}
	return title, files, nil
}

// ===========================================================================
// 播放：转存 → DLNA 直链（需登录 Cookie；盒子侧实测）
// ===========================================================================

// baiduEnsureTmpDir 确保临时目录 /bigpan_tmp 存在；失败返回 ""（调用方降级根目录）。
// 【盒子实测】必须用长期存在的平级目录：新建子目录后立即 transfer+mediainfo 会因
// 后端同步延迟报 errno=31023（文件未就绪）；/bigpan_tmp 一直存在，无此问题。
// 重名问题靠「查重复用 + ondup=newcopy 的 (1) 后缀」解决，不再用 per-fs_id 子目录。
func baiduEnsureTmpDir(cookie string) string {
	token, err := baiduBdstoken(cookie)
	if err != nil || token == "" {
		log.Printf("[baidu] 取 bdstoken 失败，转存降级根目录：%v", err)
		return ""
	}
	logid := strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 16)
	u := fmt.Sprintf(baiduCreateURL, token, logid)
	form := url.Values{}
	form.Set("path", baiduTmpBase)
	form.Set("isdir", "1")
	form.Set("block_list", "[]")
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[baidu] 建临时目录失败，降级根目录：%v", err)
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var out struct {
		Errno int64 `json:"errno"`
	}
	json.Unmarshal(body, &out)
	if out.Errno != 0 && out.Errno != -8 { // -8 = 已存在
		log.Printf("[baidu] 建临时目录返回 errno=%d，降级根目录", out.Errno)
		return ""
	}
	return baiduTmpBase
}

// baiduFindExisting 在临时目录里找「同名同尺寸」的已转存文件，找到则复用其路径。
// 好处：① 省一次转存和空间；② 绕开新目录同步延迟导致的 mediainfo 31023。
func baiduFindExisting(cookie, dir, name string, size int64) string {
	q := url.Values{}
	q.Set("dir", dir)
	q.Set("num", "100")
	q.Set("clienttype", "0")
	q.Set("web", "1")
	q.Set("app_id", "250528")
	req, _ := http.NewRequest(http.MethodGet, "https://pan.baidu.com/api/list?"+q.Encode(), nil)
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var r struct {
		Errno int64 `json:"errno"`
		List  []struct {
			Path     string `json:"path"`
			IsDir    int    `json:"isdir"`
			ServerFN string `json:"server_filename"`
			Size     int64  `json:"size"`
		} `json:"list"`
	}
	if json.Unmarshal(body, &r) != nil || r.Errno != 0 {
		return ""
	}
	for _, it := range r.List {
		if it.IsDir == 0 && it.Size == size && strings.EqualFold(it.ServerFN, name) {
			return it.Path
		}
	}
	return ""
}

// baiduBdstoken 取 bdstoken（写操作需要）。
func baiduBdstoken(cookie string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, baiduTmplVarURL, nil)
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if m := baiduBdstokenRe.FindSubmatch(body); m != nil {
		return string(m[1]), nil
	}
	return "", fmt.Errorf("未从响应中找到 bdstoken")
}

// baiduTransfer 把分享文件转存到本账号临时目录，返回转存后在「自己盘上」的路径。
// shareFsID 是分享内的 fs_id；uk/shareid 来自 wxlist 响应。
// bdclnd 是 verify 拿到的 randsk（BDCLND 通行证）——【盒子实测】转存分享文件必须带它，
// 否则百度返回 errno=200025「提取码输入错误」（即使分享无提取码也要）。
func baiduTransfer(cookie string, uk, shareid int64, shareFsID string, destDir string, bdclnd string) (string, error) {
	if destDir == "" {
		destDir = "/" // 降级：根目录
	}
	ck := cookie
	if bdclnd != "" {
		ck = cookie + "; BDCLND=" + bdclnd
	}
	u := fmt.Sprintf(baiduTransferURL,
		strconv.FormatInt(uk, 10),
		strconv.FormatInt(shareid, 10),
		url.QueryEscape(destDir),
		shareFsID)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Cookie", ck)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("百度转存失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out baiduTransferResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("转存返回无法识别: %.300s", string(body))
	}
	if out.Errno != 0 {
		// errno:2 = 文件已存在（判重）：视为成功，转去目标目录找最新文件。
		if out.Errno == 2 {
			return baiduPollTransferred(cookie, destDir)
		}
		log.Printf("[baidu][debug] transfer 失败 errno=%d show=%q url=%s", out.Errno, out.ShowMsg, u)
		return "", fmt.Errorf("转存失败（errno=%d）: %s %s", out.Errno, out.ErrMsg, out.ShowMsg)
	}
	// extra.list[0].to 是转存落地的真实路径；info[0].path 是分享内源路径（不可用于取流）。
	if len(out.Extra.List) > 0 && out.Extra.List[0].To != "" {
		return out.Extra.List[0].To, nil
	}
	// 异步任务模式：轮询目标目录找最新文件。
	return baiduPollTransferred(cookie, destDir)
}

// baiduPollTransferred 转存为异步任务时，轮询目标目录取最新落地的文件。
func baiduPollTransferred(cookie, destDir string) (string, error) {
	for i := 0; i < 10; i++ {
		time.Sleep(800 * time.Millisecond)
		q := url.Values{}
		q.Set("dir", destDir)
		q.Set("num", "20")
		q.Set("order", "time")
		q.Set("desc", "1")
		q.Set("clienttype", "0")
		q.Set("web", "1")
		q.Set("app_id", "250528")
		u := "https://pan.baidu.com/api/list?" + q.Encode()
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", baiduNetUA)
		req.Header.Set("Referer", "https://pan.baidu.com/")
		req.Header.Set("Cookie", cookie)
		resp, err := httpClient.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var r struct {
			Errno int64 `json:"errno"`
			List  []struct {
				Path     string `json:"path"`
				IsDir    int    `json:"isdir"`
				ServerFN string `json:"server_filename"`
			} `json:"list"`
		}
		if json.Unmarshal(b, &r) == nil && r.Errno == 0 {
			for _, it := range r.List {
				if it.IsDir == 0 && isVideo(it.ServerFN) {
					return it.Path, nil
				}
			}
		}
	}
	return "", fmt.Errorf("转存任务未完成（超时），请稍后重试")
}

// baiduMediainfo 取 DLNA 直链（path 为自己盘上的路径，需登录 Cookie 识别盘）。
// 【盒子实测】转存后文件元数据有同步延迟，立即取流可能报 errno=31023，需重试。
func baiduMediainfo(cookie, path string) (string, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * time.Second)
		}
		dlink, err := baiduMediainfoOnce(cookie, path)
		if err == nil {
			return dlink, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func baiduMediainfoOnce(cookie, path string) (string, error) {
	u := fmt.Sprintf(baiduMediainfoURL, url.QueryEscape(path))
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("百度取流失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out baiduMediainfoResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("mediainfo 返回无法识别: %.200s", string(body))
	}
	// 【盒子实测】dlink 优先：M3U8_AUTO_264 是按需转码，转码没完成时百度返
	// HTTP 400 + errno=31023，但 info 里仍带可用的原画直链（rt=pr，不绑 Cookie、
	// 支持 Range、8 小时有效）——直接用它，不为等转码反复删转文件。
	var dlink string
	var asObj struct {
		Dlink string `json:"dlink"`
	}
	if json.Unmarshal(out.Info, &asObj) == nil && asObj.Dlink != "" {
		dlink = asObj.Dlink
	} else {
		var asArr []struct {
			Dlink string `json:"dlink"`
		}
		if json.Unmarshal(out.Info, &asArr) == nil && len(asArr) > 0 {
			dlink = asArr[0].Dlink
		}
	}
	if dlink != "" {
		return dlink, nil
	}
	if out.Errno != 0 {
		return "", fmt.Errorf("mediainfo 失败（errno=%d）: %s", out.Errno, out.ErrMsg)
	}
	return "", fmt.Errorf("mediainfo 未返回直链")
}

// ---- 临时文件清理：精确路径记录 + 定时清扫（绝不扫根目录） ----

var (
	baiduTempMu    sync.Mutex
	baiduTempPaths = map[string]time.Time{} // 精确路径 → 落地时间
)

func baiduRecordTemp(p string) {
	if p == "" || p == "/" {
		return
	}
	baiduTempMu.Lock()
	baiduTempPaths[p] = time.Now()
	baiduTempMu.Unlock()
}

// baiduStartSweeper 启动后台清扫：删除 30 分钟前的临时文件。
//
// 【2026-10-17 修复孤儿堆积】清扫清单 baiduTempPaths 只在内存里，容器一重启就清空，
// 之前转存的文件从此没人认领、永远不会被删 —— 实测堆到 44.8GB，把 121GB 的盘
// 挤到只剩 68GB，飞牛影视全库扫描时直接 errno=12 全线 502。
// 所以启动时必须先去盘上扫一遍，把遗留文件纳入清理范围。
func baiduStartSweeper() {
	go func() {
		// 启动自愈：清掉盘上所有 30 分钟前的遗留文件（容器刚起，这段时间内不会有
		// 正在进行的播放；再近的留给下面常规清扫，避免打断刚拿到直链的 302 播放）。
		//
		// 【注意】本函数在 init() 里调用，那时 globalAccounts 还没初始化，
		// baiduAccountCookie() 会是空串 —— 所以这里要等账号库就绪再清，
		// 否则启动清理永远静默跳过（实测踩过）。
		for i := 0; i < 60; i++ {
			if cookie := baiduAccountCookie(); cookie != "" {
				baiduPurgeOrphans(cookie, 30*time.Minute)
				break
			}
			time.Sleep(2 * time.Second)
		}
		for {
			time.Sleep(10 * time.Minute)
			cookie := baiduAccountCookie()
			if cookie == "" {
				continue
			}
			baiduTempMu.Lock()
			expired := make([]string, 0)
			for p, t := range baiduTempPaths {
				if time.Since(t) > 30*time.Minute {
					expired = append(expired, p)
				}
			}
			baiduTempMu.Unlock()
			for _, p := range expired {
				if err := baiduDeleteFile(cookie, p); err != nil {
					log.Printf("[baidu] 清扫临时文件失败 %s: %v", p, err)
				} else {
					baiduTempMu.Lock()
					delete(baiduTempPaths, p)
					baiduTempMu.Unlock()
					log.Printf("[baidu] 已清扫临时文件 %s", p)
				}
			}
		}
	}()
}

// baiduPurgeOrphans 扫描临时目录（新 /bigpan_tmp + 旧 /panboost_tmp）的实际内容，
// 删除超过 minAge 的文件。
// 只认「文件名 + 修改时间」，不依赖内存记录 —— 这是重启后唯一可靠的信息来源。
func baiduPurgeOrphans(cookie string, minAge time.Duration) {
	for _, dir := range []string{baiduTmpBase, baiduTmpBaseLegacy} {
		baiduPurgeDir(cookie, dir, minAge)
	}
}

// baiduPurgeDir 清理单个临时目录。
func baiduPurgeDir(cookie, dir string, minAge time.Duration) {
	q := url.Values{}
	q.Set("dir", dir)
	q.Set("num", "200")
	q.Set("order", "time")
	q.Set("desc", "1")
	q.Set("clienttype", "0")
	q.Set("web", "1")
	q.Set("app_id", "250528")
	req, _ := http.NewRequest(http.MethodGet, "https://pan.baidu.com/api/list?"+q.Encode(), nil)
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[baidu] 启动清理：列临时目录 %s 失败: %v", dir, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var r struct {
		Errno int64 `json:"errno"`
		List  []struct {
			Path     string `json:"path"`
			IsDir    int    `json:"isdir"`
			MTime    int64  `json:"server_mtime"`
			Size     int64  `json:"size"`
			ServerFN string `json:"server_filename"`
		} `json:"list"`
	}
	if json.Unmarshal(body, &r) != nil || r.Errno != 0 {
		// errno=-9 = 目录不存在：首次运行还没转过存，属于正常情况，不报错。
		if r.Errno != -9 {
			log.Printf("[baidu] 启动清理：临时目录 %s 列表异常 errno=%d", dir, r.Errno)
		}
		return
	}
	var freed int64
	n := 0
	for _, it := range r.List {
		if it.IsDir != 0 || it.Path == "" {
			continue
		}
		if time.Since(time.Unix(it.MTime, 0)) < minAge {
			continue
		}
		if err := baiduDeleteFile(cookie, it.Path); err != nil {
			log.Printf("[baidu] 启动清理失败 %s: %v", it.ServerFN, err)
			continue
		}
		freed += it.Size
		n++
		baiduTempMu.Lock()
		delete(baiduTempPaths, it.Path)
		baiduTempMu.Unlock()
	}
	if n > 0 {
		log.Printf("[baidu] 启动清理：%s 删除 %d 个遗留临时文件，释放 %.2f GB", dir, n, float64(freed)/(1<<30))
	}
}

// baiduDeleteFile 删除指定路径（精确，安全）。需 bdstoken。
func baiduDeleteFile(cookie, p string) error {
	token, err := baiduBdstoken(cookie)
	if err != nil || token == "" {
		return fmt.Errorf("取 bdstoken 失败：%w", err)
	}
	u := fmt.Sprintf(baiduFileManagerURL, token)
	payload, _ := json.Marshal([]string{p})
	// 【盒子实测】filemanager 删除只认 form 格式：filelist=<url-encoded JSON 数组字符串>；
	// 直接 POST JSON body 会返回 errno=2 删不掉。
	form := url.Values{}
	form.Set("filelist", string(payload))
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", baiduNetUA)
	req.Header.Set("Referer", "https://pan.baidu.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Errno int64 `json:"errno"`
	}
	json.Unmarshal(body, &out)
	if out.Errno != 0 {
		return fmt.Errorf("删除返回 errno=%d", out.Errno)
	}
	return nil
}

// ---- 播放解析互斥锁：同一分享文件的多跳解析（302 试跳 + 中转 + 回环）串行化，
// 避免重复转存与"刚落盘文件的 mediainfo 同步延迟」互相踩踏。 ----

var (
	baiduPlayMu   sync.Mutex
	baiduPlayLock = map[string]*sync.Mutex{}
)

func baiduPlayMutex(key string) func() {
	baiduPlayMu.Lock()
	mu, ok := baiduPlayLock[key]
	if !ok {
		mu = &sync.Mutex{}
		baiduPlayLock[key] = mu
	}
	baiduPlayMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// ResolvePlay 取播放直链：转存到临时目录 → 取 DLNA 直链（302 友好）。
func (baiduProvider) ResolvePlay(item *Item) (PlayTarget, error) {
	cookie := baiduAccountCookie()
	if cookie == "" {
		return PlayTarget{}, fmt.Errorf("还没有导入百度网盘登录（取流需要登录，解析分享不需要）")
	}
	defer baiduPlayMutex(item.ShareID + "|" + item.FileID)()
	surFull := item.ShareID
	// ① 校验提取码拿 randsk（游客态）
	randsk, err := baiduVerify(surFull, item.Passcode)
	if err != nil {
		return PlayTarget{}, err
	}
	// ② 列分享根拿 shareid / uk（转存要用）
	root, err := baiduWxList(randsk, surFull, item.Passcode, "", "1")
	if err != nil {
		return PlayTarget{}, err
	}
	if root.Data.ShareID == 0 || root.Data.UK == 0 {
		return PlayTarget{}, fmt.Errorf("分享信息不完整（缺少 shareid/uk）")
	}

	// ③ 确保临时目录（失败降级根目录）→ 查重复用 → 没有才转存（带 BDCLND 通行证，否则 errno=200025）
	destDir := baiduEnsureTmpDir(cookie)
	fileName := item.Title + item.Ext
	mypath := baiduFindExisting(cookie, destDir, fileName, item.Size)
	if mypath != "" {
		baiduRecordTemp(mypath) // 复用也续期，避免被 sweeper 清掉
		log.Printf("[baidu] 复用已转存文件 %s", mypath)
	} else {
		mypath, err = baiduTransfer(cookie, root.Data.UK, root.Data.ShareID, item.FileID, destDir, baiduBdclnd(randsk))
		if err != nil {
			return PlayTarget{}, err
		}
		baiduRecordTemp(mypath)
	}

	// ④ 取 DLNA 直链。失败自愈：复用的文件可能个体抽风（实测遇到过转存出的文件
	// mediainfo 持续 31023），删掉它重新转存一份新的再取。
	// 删除用同步执行：异步删会和紧随其后的 transfer 新建同名文件互相踩。
	dlink, err := baiduMediainfo(cookie, mypath)
	if err != nil {
		log.Printf("[baidu] 取流失败（%v），自愈：删除 %s 并重新转存", err, mypath)
		baiduDeleteFile(cookie, mypath)
		var mypath2 string
		mypath2, err2 := baiduTransfer(cookie, root.Data.UK, root.Data.ShareID, item.FileID, destDir, baiduBdclnd(randsk))
		if err2 == nil {
			baiduRecordTemp(mypath2)
			mypath = mypath2
			dlink, err = baiduMediainfo(cookie, mypath)
		} else {
			err = err2
		}
	}
	if err != nil {
		baiduDeleteFile(cookie, mypath) // 取流失败也尽量清掉临时文件
		return PlayTarget{}, err
	}
	// Cookie 留空：DLNA 直链不绑 Cookie，客户端直连即可（支持 302）。
	return PlayTarget{URL: dlink, UA: baiduNetUA, Cookie: ""}, nil
}

func init() {
	baiduStartSweeper() // 启动临时文件清扫（只清理记录的精确路径，不扫根目录）
}
