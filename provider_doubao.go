package main

// ---------------------------------------------------------------------------
// 豆包云盘 provider（多账号 = 多网盘）
//
// 设计：**一个账号就是一个网盘**。doubao1 / doubao2 / doubao3 / doubao4
// 各是一张独立卡片、一份独立 Cookie、一套独立文件树。这样条目、STRM、
// 302、巡检、统计全部按 provider 名天然隔离 —— 不用给 Item 加账号字段，
// 也不用改 PanProvider 接口。
//
// 两种导入来源：
//   1) 分享链接 https://www.doubao.com/drive/s/{share_id}
//      老格式，公开分享任何已登录账号都能解析。
//   2) 自己的盘 doubao://doubao2/{node_id}
//      伪链接，直接浏览自己网盘里的目录。用户是文件所有者：
//      不需要分享、不需要转存、不怕分享被删。
//
// 取链：media/get_play_info → 最高分辨率档位 main_url → 302。
// 实测 1080p h264、无 Cookie 可直连、58 Mbps。
// ---------------------------------------------------------------------------

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// doubaoSlots 预留的账号槽位数。加账号就改这个数（UI 会同步多一张卡片）。
const doubaoSlots = 4

// doubaoNodeIDPrefix 标记「自己盘里的目录」。
//
// 它不是分享 ID，但必须占住 ShareID 这个字段，否则分享表的键会退化成
// "doubao1|"（空分享 ID），用户从自己盘导入第二个目录时直接覆盖第一个 ——
// 表现就是首页列表里豆包永远只剩一条、名字还是最后导的那个。
//
// 同时巡检靠这个前缀认出「这不是分享」，跳过探活：自己盘条目要扫目录才
// 知道还在不在，几千个条目轮着扫必然触发风控。
const doubaoNodeIDPrefix = "node:"

// dbScanBudget 单次「解析」最多扫多久。
//
// 豆包自家盘全局限速 doubaoGap（0.6 秒/请求起步，撞风控自动放大），
// 一个目录一次请求 —— 几百个目录同步扫下来必然超过反向代理的 60 秒超时，
// 用户看到的就是「内容一多就解析失败」。
//
// 所以每段扫 45 秒就返回：已扫到的文件先给前端预览，pending=true 让前端
// 自动再发一次接着扫（断点已落盘，不会重复也不会漏）。
const dbScanBudget = 45 * time.Second

// doubaoIsNodeID 判断某个 ShareID 是不是「自己盘的目录」。
func doubaoIsNodeID(sid string) bool {
	return strings.HasPrefix(sid, doubaoNodeIDPrefix)
}

// doubaoTag 取一个能区分账号的短标识。
//
// 豆包没有任何公开的用户信息接口（/samantha/user/info、/alice/user/info 等
// 8 个路径实测全 404），JWT 里也只有 uid 没有昵称。能稳定拿到的只有
// Cookie 里的 uid_tt：32 位哈希、每个账号不同、不会随 7 天的 token 过期。
// 4 个槽位靠这个短码至少能分清「是不是同一个号」。
func doubaoTag(cookie string) string {
	for _, kv := range strings.Split(cookie, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "uid_tt" && len(v) >= 6 {
			return v[:6]
		}
	}
	// 没有 uid_tt（只粘了 sessionid）：用整段 Cookie 的短哈希兜底
	sum := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(sum[:])[:6]
}

// doubaoShareRe 老格式分享：/drive/s/{16位hex}
var doubaoShareRe = regexp.MustCompile(`(?i)doubao\.com/drive/s/([A-Za-z0-9]{8,40})`)

// doubaoShrRe 新格式分享：/drive/shr/{27位token}（飞书体系）。
// 2026-10-24 实测：shr 的 token 就是飞书 node_token，分享页走
// permission/setting.v2 + children/list?token=xxx 解析，播放同新链路 302。
var doubaoShrRe = regexp.MustCompile(`(?i)doubao\.com/drive/shr/([A-Za-z0-9]+)`)

// doubaoShrPrefix 标记「新格式分享」。占住 ShareID 字段当唯一键，
// shareURL / RefreshShare 据此区分新旧两种分享。
const doubaoShrPrefix = "shr:"

// parseDoubaoLink 解析豆包分享链接，返回 share_id。
// 老格式（/drive/s/）返回 16 位 hex；新格式（/drive/shr/）返回 "shr:"+token。
func parseDoubaoLink(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请粘贴分享链接")
	}
	if m := doubaoShrRe.FindStringSubmatch(raw); m != nil {
		return doubaoShrPrefix + m[1], nil
	}
	if m := doubaoShareRe.FindStringSubmatch(raw); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("这不是豆包分享链接")
}

// parseDoubaoOwnLink 解析自己盘的伪链接：
//
//	doubao://<provider>/<node_id>[/<目录名>]
//
// 第三段（目录名）是可选的、URL 编码的，只用来给导入的剧起标题 ——
// 不带名字的话，扫一个多子目录的容器就只能叫「豆包文件」，首页列表里认不出来。
// 老链接（不带名字）仍然可用。
func parseDoubaoOwnLink(raw string) (provider, nodeID, name string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "doubao://") {
		return "", "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, "doubao://"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	if len(parts) == 3 {
		if n, err := url.PathUnescape(parts[2]); err == nil {
			name = strings.TrimSpace(n)
		}
	}
	return parts[0], parts[1], name, true
}

// ---- provider ----

type doubaoProvider struct{ slot int }

func init() {
	for i := 1; i <= doubaoSlots; i++ {
		registerProvider(doubaoProvider{slot: i})
	}
}

func (p doubaoProvider) Name() string { return "doubao" + strconv.Itoa(p.slot) }

func (p doubaoProvider) MatchLink(link string) bool {
	link = strings.TrimSpace(link)
	// 自己的盘伪链接：槽位对上才认，否则 doubao1 会抢走 doubao3 的链接
	if pv, _, _, ok := parseDoubaoOwnLink(link); ok {
		return pv == p.Name()
	}
	return doubaoShareRe.MatchString(link) || doubaoShrRe.MatchString(link)
}

// Wants302 豆包直链不绑 Cookie（实测无 Cookie、换 IP 均可拉），走 302。
func (p doubaoProvider) Wants302() bool { return true }

func (p doubaoProvider) ListShareFiles(link string) (ShareInfo, error) {
	cli := p.client(globalAccounts)
	if cli == nil {
		return ShareInfo{}, fmt.Errorf("%s 还没粘 Cookie", providerCN(p.Name()))
	}

	// A. 自己的盘
	if pv, nodeID, name, ok := parseDoubaoOwnLink(link); ok {
		if pv != p.Name() {
			return ShareInfo{}, fmt.Errorf("这个链接属于 %s，不是 %s", pv, p.Name())
		}
		title, files, pending, err := scanDoubaoOwn(cli, p.Name(), nodeID, name, dbScanBudget)
		if err != nil {
			return ShareInfo{}, err
		}
		return ShareInfo{
			Provider: p.Name(), Title: title,
			ShareID: doubaoNodeIDPrefix + nodeID, // 用 node_id 当唯一键，避免多个目录互相覆盖
			Files:   files, Logged: true,
			Pending: pending, Scanned: len(files),
		}, nil
	}

	// B. 别人的分享
	shareID, err := parseDoubaoLink(link)
	if err != nil {
		return ShareInfo{}, err
	}
	// B2. 新格式分享（/drive/shr/）：走 v2 链路，children/list?token= 直接列
	if strings.HasPrefix(shareID, doubaoShrPrefix) {
		tok := strings.TrimPrefix(shareID, doubaoShrPrefix)
		title, files, pending, err := scanDoubaoShr(cli, p.Name(), tok)
		if err != nil {
			return ShareInfo{}, err
		}
		return ShareInfo{
			Provider: p.Name(), Title: title,
			ShareID: shareID,
			Files:   files, Logged: true,
			Pending: pending, Scanned: len(files),
		}, nil
	}
	title, files, err := scanDoubaoShare(cli, p.Name(), shareID)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{Provider: p.Name(), Title: title, ShareID: shareID, Files: files, Logged: true}, nil
}

func (p doubaoProvider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	cli := p.client(globalAccounts)
	if cli == nil {
		return "", nil, fmt.Errorf("%s 还没粘 Cookie", providerCN(p.Name()))
	}
	if shareID == "" || doubaoIsNodeID(shareID) {
		return "", nil, fmt.Errorf("自己盘里的条目不参与追更，重新导入即可")
	}
	if strings.HasPrefix(shareID, doubaoShrPrefix) {
		tok := strings.TrimPrefix(shareID, doubaoShrPrefix)
		// 追更是后台定时任务，没有反代超时压力：不限时一次扫完。
		title, files, _, err := scanDoubaoShr(cli, p.Name(), tok)
		if err != nil {
			return "", nil, err
		}
		return title, files, nil
	}
	return scanDoubaoShare(cli, p.Name(), shareID)
}

func (p doubaoProvider) ResolvePlay(item *Item) (PlayTarget, error) {
	cli := p.client(globalAccounts)
	if cli == nil {
		return PlayTarget{}, fmt.Errorf("%s 还没粘 Cookie", providerCN(p.Name()))
	}
	// 新链路条目：FileID 是飞书 node_token（字母串），FidToken 存 obj_token。
	// 播放直链把鉴权放在 query 里，302 后播放器无需任何特殊头。
	if !dbIsOldNodeID(item.FileID) && cli.v2Key != nil && item.FidToken != "" {
		// 新链路 302 直链：鉴权在 query 里（token 用 biz_auth 刷新后的 + DPoP 前缀）
		u, err := cli.dbnDownloadURL(item.FidToken)
		if err == nil && u != "" {
			return PlayTarget{URL: u}, nil
		}
		log.Printf("[豆包v2] 新链路取链失败，降级老链路：%v", err)
	}
	// 老链路条目：FileID 是纯数字 node_id；item.ShareID 非空且不是 node: 前缀
	// 才表示来自分享，取链要带 share_id。
	sid := item.ShareID
	if doubaoIsNodeID(sid) {
		sid = "" // 自己盘的文件不能带 share_id，否则接口报参数错误
	}
	list, err := cli.dbPlayInfo(item.FileID, item.FidToken, sid)
	if err != nil {
		return PlayTarget{}, err
	}
	u := dbPickURL(list)
	if u == "" {
		return PlayTarget{}, fmt.Errorf("豆包未返回可用播放地址")
	}
	return PlayTarget{URL: u, UA: doubaoUA}, nil
}

// ---- 客户端（按槽位缓存，互不干扰）----

var (
	dbMu      sync.Mutex
	dbClients = map[string]*Doubao{}
	dbKeys    = map[string]string{}
)

func (p doubaoProvider) client(as *AccountStore) *Doubao {
	if as == nil {
		return nil
	}
	acc := as.Get(p.Name())
	if acc == nil || strings.TrimSpace(acc.Cookie) == "" {
		return nil
	}
	name := p.Name()
	dbMu.Lock()
	defer dbMu.Unlock()
	if c, ok := dbClients[name]; ok && dbKeys[name] == acc.Cookie {
		return c
	}
	c := newDoubao(acc.Cookie)
	dbClients[name] = c
	dbKeys[name] = acc.Cookie
	return c
}

// resetDoubaoClients 换号时清缓存（旧 Cookie 的客户端不能再复用）。
func resetDoubaoClients() {
	dbMu.Lock()
	dbClients = map[string]*Doubao{}
	dbKeys = map[string]string{}
	dbMu.Unlock()
}

// ---- 扫描 ----

// scanDoubaoOwn 从自己盘的某个目录起，递归收集视频文件。
// ShareFile.ID 存 node_id，FidToken 存 key，Code 留空（不是分享）。
//
// 上限只做「防跑飞」保护（2 万文件 / 5 千目录），不是业务限制：
// 之前写的 800 文件 / 300 目录会静默漏文件，用户的库有几千个视频。
//
// 两个硬要求：
//  1. 任何目录读失败都**立刻中止**，不能 continue 跳过 —— 跳过就是静默漏文件；
//  2. 中止前把进度落盘，用户过几分钟再点一次导入就从断点接着扫。
func scanDoubaoOwn(cli *Doubao, slot, nodeID, hint string, budget time.Duration) (string, []ShareFile, bool, error) {
	const maxFiles, maxDirs = 20000, 5000
	type job = dbScanJob

	key := "own|" + slot + "|" + nodeID
	st := dbScanTake(key)

	seen := make(map[string]bool, len(st.Seen)+64)
	for _, id := range st.Seen {
		seen[id] = true
	}
	queue := append([]job(nil), st.Queue...)
	if len(queue) == 0 && len(seen) == 0 {
		queue = append(queue, job{ID: nodeID})
		seen[nodeID] = true
	}
	files := st.Files
	title := st.Title
	if title == "" {
		// 优先用伪链接里带的目录名（浏览面板生成的都有），
		// 没有才退到通用名 —— 否则首页列表里会堆一排「豆包文件」。
		title = strings.TrimSpace(hint)
	}
	if title == "" {
		title = "豆包文件"
	}
	if len(st.Seen) > 0 && len(queue) > 0 {
		log.Printf("[豆包] 从断点继续：已扫 %d 个目录 / 已有 %d 个视频 / 还剩 %d 个目录待扫",
			len(seen), len(files), len(queue))
	}

	start := time.Now()
	deadline := start.Add(budget)
	dirty := 0
	// snapshot 把当前现场拍成可落盘的快照。
	snapshot := func() *dbScanState {
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		return &dbScanState{
			Key: key, Title: title, Queue: append([]job(nil), queue...),
			Seen: ids, Files: files,
		}
	}

	for len(queue) > 0 && len(files) < maxFiles && len(seen) < maxDirs {
		// 分段扫描：到点就交回已扫的部分，前端自动再调一次接着扫。
		// 断点已经落盘（dbScanPut），进程重启也不会丢。
		if budget > 0 && time.Now().After(deadline) {
			dbScanPut(snapshot())
			log.Printf("[豆包] 分段返回：已扫 %d 个目录 / %d 个视频，还剩 %d 个目录（前端会自动续扫）",
				len(seen), len(files), len(queue))
			return title, files, true, nil
		}
		j := queue[0]
		queue = queue[1:]
		list, err := cli.dbListDirAll(j.ID)
		if err != nil {
			// 这个目录还没扫完，必须放回队首再存盘 ——
			// 否则它已经在 seen 里、又不在队列里，续扫时会被永久跳过，
			// 连带它下面的所有视频一起消失（实测少 3 个文件就是这么来的）。
			queue = append([]job{j}, queue...)
			dbScanPut(snapshot())
			return "", nil, false, fmt.Errorf("豆包限流了（请求太密，不是文件出错）。已扫到 %d 个目录 / %d 个视频，进度已保存。\n等 3~5 分钟再点一次「解析」，会从这里接着扫，不用从头开始。\n原始信息：%w",
				len(seen), len(files), err)
		}
		// 单层且只有一个目录时，用它的名字当剧名（和 UC 的折叠规则一致）
		if j.ID == nodeID && len(list) == 1 && list[0].IsDir() {
			title = list[0].Name
		}
		for _, n := range list {
			p := n.Name
			if j.Prefix != "" {
				p = j.Prefix + "/" + n.Name
			}
			if n.IsDir() {
				if !seen[n.ID] {
					seen[n.ID] = true
					queue = append(queue, job{ID: n.ID, Prefix: p})
				}
				continue
			}
			if isVideo(n.Name) {
				files = append(files, ShareFile{
					ID: n.ID, Name: n.Name, Size: n.Size, Path: p, FidToken: n.Key,
				})
			}
		}
		// 每 30 个目录存一次盘：崩溃/重启也不丢太多进度
		if dirty++; dirty >= 30 {
			dirty = 0
			dbScanPut(snapshot())
		}
		if len(files) > 0 && len(files)/200 > (len(files)-len(list))/200 {
			log.Printf("[豆包] 扫描中：%d 个目录 / %d 个视频 / 已用 %v", len(seen), len(files), time.Since(start).Round(time.Second))
		}
	}
	dbScanDone(key)
	log.Printf("[豆包] 扫描完成：%d 个目录 / %d 个视频 / 耗时 %v", len(seen), len(files), time.Since(start).Round(time.Second))
	if len(files) == 0 {
		return "", nil, false, fmt.Errorf("这个目录里没有视频文件")
	}
	return title, files, false, nil
}

// scanDoubaoShr 扫描新格式分享（/drive/shr/）。
//
// 2026-10-24 实测：shr 的 27 位 token 就是飞书 node_token，分享内容的
// 列取和自己盘完全同一个接口（children/list?token=xxx），权限由服务端
// 按「链接可见」判定 —— 所以扫描逻辑直接复用 scanDoubaoOwn，
// 只有标题来源不同：用 obj/path 接口拿分享根的名字。
func scanDoubaoShr(cli *Doubao, slot, token string) (string, []ShareFile, bool, error) {
	hint, err := cli.dbnNodeName(token)
	if err != nil {
		log.Printf("[豆包v2] 取分享名失败（不影响导入）：%v", err)
		hint = ""
	}
	return scanDoubaoOwn(cli, slot, token, hint, dbScanBudget)
}

// scanDoubaoShare 递归扫描别人的分享目录树。规则同自己的盘：失败即停、断点续传。
func scanDoubaoShare(cli *Doubao, slot, shareID string) (string, []ShareFile, error) {
	type job = dbScanJob
	key := "share|" + slot + "|" + shareID
	st := dbScanTake(key)

	seen := make(map[string]bool, len(st.Seen)+64)
	for _, id := range st.Seen {
		seen[id] = true
	}
	queue := append([]job(nil), st.Queue...)
	files := st.Files
	title := st.Title

	if len(seen) == 0 {
		var nodes []dbNode
		var err error
		title, nodes, err = cli.dbShareOverview(shareID)
		if err != nil {
			return "", nil, err
		}
		for _, n := range nodes {
			if n.IsDir() {
				if !seen[n.ID] {
					seen[n.ID] = true
					queue = append(queue, job{ID: n.ID, Prefix: n.Name})
				}
				continue
			}
			if isVideo(n.Name) {
				files = append(files, ShareFile{
					ID: n.ID, Name: n.Name, Size: n.Size, Path: n.Name, Code: shareID, FidToken: n.Key,
				})
			}
		}
	} else {
		log.Printf("[豆包] 从断点继续（分享）：已扫 %d 个目录 / 已有 %d 个视频", len(seen), len(files))
	}

	dirty := 0
	for len(queue) > 0 && len(files) < 20000 && len(seen) < 5000 {
		j := queue[0]
		queue = queue[1:]
		list, err := cli.dbShareListDir(shareID, j.ID)
		if err != nil {
			queue = append([]job{j}, queue...) // 同自己盘：正在扫的这个不能丢
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			dbScanPut(&dbScanState{Key: key, Title: title, Queue: append([]job(nil), queue...), Seen: ids, Files: files})
			return "", nil, fmt.Errorf("豆包限流了（请求太密，不是文件出错）。已扫到 %d 个目录 / %d 个视频，进度已保存。\n等 3~5 分钟再点一次「解析」，会从这里接着扫。\n原始信息：%w",
				len(seen), len(files), err)
		}
		for _, n := range list {
			p := j.Prefix + "/" + n.Name
			if n.IsDir() {
				if !seen[n.ID] {
					seen[n.ID] = true
					queue = append(queue, job{ID: n.ID, Prefix: p})
				}
				continue
			}
			if isVideo(n.Name) {
				files = append(files, ShareFile{
					ID: n.ID, Name: n.Name, Size: n.Size, Path: p, Code: shareID, FidToken: n.Key,
				})
			}
		}
		if dirty++; dirty >= 30 {
			dirty = 0
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			dbScanPut(&dbScanState{Key: key, Title: title, Queue: append([]job(nil), queue...), Seen: ids, Files: files})
		}
	}
	dbScanDone(key)
	if len(files) == 0 {
		return "", nil, fmt.Errorf("这个分享里没有视频文件")
	}
	return title, files, nil
}

// ---- 浏览自己的盘（给界面用）----

// doubaoBrowseItem 是浏览接口返回的一行。
type doubaoBrowseItem struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Video bool   `json:"video"`
	Link  string `json:"link"` // 目录才有：可直接粘贴导入的伪链接
}

// apiDoubaoBrowse GET /api/doubao/browse?provider=doubao1[&node_id=xxx]
// 不传 node_id 时列根目录。返回该层的目录/文件，目录附带可直接导入的伪链接。
func apiDoubaoBrowse(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pv := r.URL.Query().Get("provider")
		nodeID := r.URL.Query().Get("node_id")
		if pv == "" {
			pv = "doubao1"
		}
		dp, ok := providerByName(pv).(doubaoProvider)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "不是豆包账号：" + pv})
			return
		}
		cli := dp.client(as)
		if cli == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": providerCN(pv) + " 还没粘 Cookie"})
			return
		}

		var nodes []dbNode
		var rootID string
		var err error
		if nodeID == "" {
			rootID, nodes, err = cli.dbHome()
		} else {
			nodes, err = cli.dbListDir(nodeID)
		}
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}

		items := make([]doubaoBrowseItem, 0, len(nodes))
		for _, n := range nodes {
			it := doubaoBrowseItem{Name: n.Name, ID: n.ID, IsDir: n.IsDir(), Size: n.Size}
			if n.IsDir() {
				it.Link = "doubao://" + pv + "/" + n.ID + "/" + url.PathEscape(n.Name)
			} else {
				it.Video = isVideo(n.Name)
				it.Link = "doubao://" + pv + "/" + n.ID + "/" + url.PathEscape(n.Name)
			}
			items = append(items, it)
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "provider": pv, "root_id": rootID, "node_id": nodeID, "items": items,
		})
	}
}
