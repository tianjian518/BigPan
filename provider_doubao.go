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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
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
		scanKey := "own|" + p.Name() + "|" + nodeID
		// 后台已经在扫这个目录：直接把当前进度交回去，别再开一轮同步扫描
		// （重复扫会双倍打接口，还可能互相抢断点）。
		if t, fs, ok := dbTaskRunningInfo(scanKey); ok {
			return ShareInfo{
				Provider: p.Name(), Title: t,
				ShareID: doubaoNodeIDPrefix + nodeID,
				Files:   fs, Logged: true,
				Pending: true, Scanned: len(fs), ScanKey: scanKey,
				Resume: func() (string, []ShareFile, bool, error) {
					return scanDoubaoOwn(cli, p.Name(), nodeID, name, dbScanBudget)
				},
			}, nil
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
			ScanKey: "own|" + p.Name() + "|" + nodeID,
			Resume: func() (string, []ShareFile, bool, error) {
				return scanDoubaoOwn(cli, p.Name(), nodeID, name, dbScanBudget)
			},
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
		scanKey := "own|" + p.Name() + "|" + tok
		if t, fs, ok := dbTaskRunningInfo(scanKey); ok {
			return ShareInfo{
				Provider: p.Name(), Title: t, ShareID: shareID,
				Files: fs, Logged: true,
				Pending: true, Scanned: len(fs), ScanKey: scanKey,
				Resume: func() (string, []ShareFile, bool, error) {
					return scanDoubaoOwn(cli, p.Name(), tok, t, dbScanBudget)
				},
			}, nil
		}
		title, files, pending, err := scanDoubaoShr(cli, p.Name(), tok, dbScanBudget)
		if err != nil {
			return ShareInfo{}, err
		}
		return ShareInfo{
			Provider: p.Name(), Title: title,
			ShareID: shareID,
			Files:   files, Logged: true,
			Pending: pending, Scanned: len(files),
			ScanKey: "own|" + p.Name() + "|" + tok,
			Resume: func() (string, []ShareFile, bool, error) {
				return scanDoubaoOwn(cli, p.Name(), tok, title, dbScanBudget)
			},
		}, nil
	}
	// 老格式分享（/drive/s/）：几百个目录同样得分段，否则必撞反代超时
	scanKey := "share|" + p.Name() + "|" + shareID
	if t, fs, ok := dbTaskRunningInfo(scanKey); ok {
		return ShareInfo{
			Provider: p.Name(), Title: t, ShareID: shareID,
			Files: fs, Logged: true,
			Pending: true, Scanned: len(fs), ScanKey: scanKey,
			Resume: func() (string, []ShareFile, bool, error) {
				return scanDoubaoShare(cli, p.Name(), shareID, dbScanBudget)
			},
		}, nil
	}
	title, files, pending, err := scanDoubaoShare(cli, p.Name(), shareID, dbScanBudget)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: p.Name(), Title: title, ShareID: shareID,
		Files: files, Logged: true,
		Pending: pending, Scanned: len(files), ScanKey: scanKey,
		Resume: func() (string, []ShareFile, bool, error) {
			return scanDoubaoShare(cli, p.Name(), shareID, dbScanBudget)
		},
	}, nil
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
		title, files, _, err := scanDoubaoShr(cli, p.Name(), tok, 0)
		if err != nil {
			return "", nil, err
		}
		return title, files, nil
	}
	// 追更是后台定时任务，没有反代超时压力：不限时一次扫完
	title, files, _, err := scanDoubaoShare(cli, p.Name(), shareID, 0)
	if err != nil {
		return "", nil, err
	}
	return title, files, nil
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
func scanDoubaoShr(cli *Doubao, slot, token string, budget time.Duration) (string, []ShareFile, bool, error) {
	hint, err := cli.dbnNodeName(token)
	if err != nil {
		log.Printf("[豆包v2] 取分享名失败（不影响导入）：%v", err)
		hint = ""
	}
	return scanDoubaoOwn(cli, slot, token, hint, budget)
}

// scanDoubaoShare 递归扫描别人的分享目录树。规则同自己的盘：失败即停、断点续传。
//
// budget > 0 时分段：到点返回已扫部分 + pending=true，前端/后台任务接着扫。
// 老格式分享（/drive/s/）动辄几百个目录，一口气扫必然撞反向代理 60 秒超时
// —— 这是「内容一多就解析失败」在老分享上的同一个病（2026-10-27 补）。
func scanDoubaoShare(cli *Doubao, slot, shareID string, budget time.Duration) (string, []ShareFile, bool, error) {
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
			return "", nil, false, err
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
		log.Printf("[豆包] 从断点继续（分享）：已扫 %d 个目录 / 已有 %d 个视频 / 还剩 %d 个目录待扫",
			len(seen), len(files), len(queue))
	}

	start := time.Now()
	deadline := start.Add(budget)
	// snapshot 把当前现场拍成可落盘的快照（原来两处落盘各写一遍，容易漏字段）
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

	dirty := 0
	for len(queue) > 0 && len(files) < 20000 && len(seen) < 5000 {
		// 到点交回已扫部分（同自己盘），否则几百个目录必然超时
		if budget > 0 && time.Now().After(deadline) {
			dbScanPut(snapshot())
			log.Printf("[豆包] 分段返回（分享）：已扫 %d 个目录 / %d 个视频，还剩 %d 个目录（会自动续扫）",
				len(seen), len(files), len(queue))
			return title, files, true, nil
		}
		j := queue[0]
		queue = queue[1:]
		list, err := cli.dbShareListDir(shareID, j.ID)
		if err != nil {
			queue = append([]job{j}, queue...) // 同自己盘：正在扫的这个不能丢
			dbScanPut(snapshot())
			return "", nil, false, fmt.Errorf("豆包限流了（请求太密，不是文件出错）。已扫到 %d 个目录 / %d 个视频，进度已保存。\n等 3~5 分钟再点一次「解析」，会从这里接着扫。\n原始信息：%w",
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
			dbScanPut(snapshot())
		}
	}
	dbScanDone(key)
	if len(files) == 0 {
		return "", nil, false, fmt.Errorf("这个分享里没有视频文件")
	}
	return title, files, false, nil
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

// ---------------------------------------------------------------------------
// 按目录导入（老分享）：像天翼订阅号那样 —— 先秒开目录列表，再挑一个导
//
// 老分享（/drive/s/xxx）常是一个大合集：根下是「电视剧」「电影」「动漫」…
// 整条导进去只能落成一个总文件夹。这里改成两步：
//   ① 只列 1~2 层目录（2 个请求，1 秒内出结果）；
//   ② 点哪个目录才扫哪个子树（几十个目录，几十秒），落成 豆包1/电视剧/剧名/…
// ---------------------------------------------------------------------------

// scanDoubaoShareNode 只扫分享里的某个子目录（不扫全树）。
// prefix 是该目录在分享内的完整路径（如「芦苇豆包/电视剧」），拼文件 Path 用。
func scanDoubaoShareNode(cli *Doubao, slot, shareID, nodeID, prefix string, budget time.Duration) ([]ShareFile, bool, error) {
	type job = dbScanJob
	key := "snode|" + slot + "|" + shareID + "|" + nodeID
	st := dbScanTake(key)

	seen := make(map[string]bool, len(st.Seen)+64)
	for _, id := range st.Seen {
		seen[id] = true
	}
	queue := append([]job(nil), st.Queue...)
	if len(queue) == 0 && len(seen) == 0 {
		queue = append(queue, job{ID: nodeID, Prefix: prefix})
		seen[nodeID] = true
	}
	files := st.Files

	snapshot := func() *dbScanState {
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		return &dbScanState{
			Key: key, Title: prefix, Queue: append([]job(nil), queue...),
			Seen: ids, Files: files, Soft: st.Soft,
		}
	}

	start := time.Now()
	deadline := start.Add(budget)
	dirty := 0
	for len(queue) > 0 && len(files) < 20000 && len(seen) < 5000 {
		if budget > 0 && time.Now().After(deadline) {
			dbScanPut(snapshot())
			log.Printf("[豆包] 子目录分段返回：%s 已扫 %d 个目录 / %d 个视频", prefix, len(seen), len(files))
			return files, true, nil
		}
		j := queue[0]
		queue = queue[1:]
		// 撞限流不认输：先等一会儿重试同一个目录（间隔已被自适应放大，
		// 这里再叠一层退避）。限流是暂时的，不该让整趟扫描报销。
		var list []dbNode
		var err error
		for attempt := 0; ; attempt++ {
			list, err = cli.dbShareListDir(shareID, j.ID)
			if err == nil || !dbSoftThrottle(err) || attempt >= 3 {
				break
			}
			wait := time.Duration(4<<attempt) * time.Second // 4s / 8s / 16s
			log.Printf("[豆包] 限流，等 %v 后重试当前目录（第 %d 次）：%s", wait, attempt+1, j.Prefix)
			time.Sleep(wait)
			if budget > 0 && time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			queue = append([]job{j}, queue...) // 正在扫的这个不能丢
			st.Soft++
			dbScanPut(snapshot())
			if dbSoftThrottle(err) && st.Soft < 6 {
				// 软失败：返回「还有活没干完」，任务循环会接着扫，
				// 前端只看到数字在慢慢涨，不会有红色报错。
				log.Printf("[豆包] 仍在限流，稍后自动续扫（%s，已扫 %d 个目录 / %d 个视频，第 %d 次）",
					prefix, len(seen), len(files), st.Soft)
				return files, true, nil
			}
			return files, false, fmt.Errorf("豆包限流了（请求太密，不是文件出错）。已扫到 %d 个目录 / %d 个视频，进度已保存。\n等 3~5 分钟再点一次「导入」，会从这里接着扫。\n原始信息：%w",
				len(seen), len(files), err)
		}
		st.Soft = 0
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
					ID: n.ID, Name: n.Name, Size: n.Size, Path: p, Code: shareID, FidToken: n.Key,
				})
			}
		}
		if dirty++; dirty >= 30 {
			dirty = 0
			dbScanPut(snapshot())
		}
	}
	dbScanDone(key)
	log.Printf("[豆包] 子目录扫描完成：%s，%d 个目录 / %d 个视频 / 耗时 %v",
		prefix, len(seen), len(files), time.Since(start).Round(time.Second))
	return files, false, nil
}

// doubaoShareItem 浏览别人分享时的一行（目录或文件）。
type doubaoShareItem struct {
	Name     string `json:"name"`
	ID       string `json:"node_id"`
	Path     string `json:"path"` // 分享内的完整路径（含容器层）
	IsDir    bool   `json:"is_dir"`
	Video    bool   `json:"video"`
	Size     int64  `json:"size"`
	Imported bool   `json:"imported,omitempty"` // 媒体库里已经有这个目录的条目（前台不显示，仅供排查）
}

// apiDoubaoShareLS GET /api/doubao/share/ls?link=&node_id=&path=
// 浏览别人的老分享（/drive/s/），跟浏览自己盘一样逐层点。
// node_id 为空 = 看分享根（只有一层容器时自动下探，省一次点击）。
func apiDoubaoShareLS(store *Store, as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		link := strings.TrimSpace(r.URL.Query().Get("link"))
		nodeID := strings.TrimSpace(r.URL.Query().Get("node_id"))
		path := strings.Trim(filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path"))), "/")

		p := findProvider(link)
		dp, ok := p.(doubaoProvider)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "这不是豆包分享链接"})
			return
		}
		shareID, err := parseDoubaoLink(link)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		if strings.HasPrefix(shareID, doubaoShrPrefix) {
			writeJSON(w, map[string]interface{}{
				"ok": false,
				"error": "这是新格式分享（/drive/shr/），直接粘到「分享导入」页就行；" +
					"这里浏览的是老的 /drive/s/ 链接",
			})
			return
		}
		cli := dp.client(as)
		if cli == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": providerCN(dp.Name()) + " 还没粘 Cookie"})
			return
		}
		if nodeID != "" {
			if _, e := strconv.ParseInt(nodeID, 10, 64); e != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "目录标识无效，请点「回根目录」重新进入"})
				return
			}
		}

		title := path
		var nodes []dbNode
		if nodeID == "" {
			var e error
			title, nodes, e = cli.dbShareOverview(shareID)
			if e != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": e.Error()})
				return
			}
			// 分享根常是一层容器（如「芦苇豆包」）：只有一个目录时直接下探，
			// 用户点进来就能看见 电视剧/电影/动漫。
			for i := 0; i < 2; i++ {
				dirs, videos := 0, 0
				var only dbNode
				for _, n := range nodes {
					if n.IsDir() {
						dirs++
						only = n
					} else if isVideo(n.Name) {
						videos++
					}
				}
				if dirs != 1 || videos != 0 {
					break
				}
				kids, e := cli.dbShareListDir(shareID, only.ID)
				if e != nil {
					break
				}
				if path == "" {
					path = only.Name
				} else {
					path += "/" + only.Name
				}
				nodes = kids
			}
		} else {
			nodes, err = cli.dbShareListDir(shareID, nodeID)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
		}

		base := providerCN(dp.Name())
		items := make([]doubaoShareItem, 0, len(nodes))
		for _, n := range nodes {
			pth := n.Name
			if path != "" {
				pth = path + "/" + n.Name
			}
			it := doubaoShareItem{
				Name: n.Name, ID: n.ID, Path: pth,
				IsDir: n.IsDir(), Size: n.Size,
			}
			// 【不要预取子项数】每个目录多打一次接口，9 个目录就是 20 秒+，
			// 反代 20 秒就断，用户看到的是「点了进不去、转圈回到根目录」。
			// 列表只要名字就够，点进去再列。
			if !n.IsDir() {
				it.Video = isVideo(n.Name)
			} else {
				it.Imported = libHasDir(store, base, n.Name)
			}
			items = append(items, it)
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "provider": dp.Name(), "share_id": shareID,
			"title": title, "path": path, "node_id": nodeID, "items": items,
		})
	}
}

// libHasDir 媒体库里是否已有「网盘/这个目录名」下的条目。
// 只用来给目录行打标记，前台不显示；补齐逻辑在入库时按 fileID 走。
func libHasDir(store *Store, providerName, dir string) bool {
	if store == nil {
		return false
	}
	want := providerName + "/" + safeFilename(dir) + "/"
	for _, it := range store.List() {
		if strings.HasPrefix(it.Dir+"/", want) {
			return true
		}
	}
	return false
}

// apiDoubaoShareScan POST {link,node_id,path} → **只扫不入库**，给导入页预览用。
//
// 关键：扫描放后台跑，接口立刻返回 job，前端轮询 /api/share/parse/status。
// 为什么不能同步扫：instabox 网关 20 秒就断（实测 502），一个目录动辄扫 1~5 分钟，
// 同步接口必挂。
func apiDoubaoShareScan(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Link   string `json:"link"`
			NodeID string `json:"node_id"`
			Path   string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		dp, ok := findProvider(strings.TrimSpace(req.Link)).(doubaoProvider)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "这不是豆包分享链接"})
			return
		}
		shareID, err := parseDoubaoLink(strings.TrimSpace(req.Link))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		if _, e := strconv.ParseInt(req.NodeID, 10, 64); e != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "目录标识无效，请回根目录重新进入"})
			return
		}
		cli := dp.client(as)
		if cli == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": providerCN(dp.Name()) + " 还没粘 Cookie"})
			return
		}
		prefix := strings.Trim(filepath.ToSlash(strings.TrimSpace(req.Path)), "/")
		name := prefix
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		title := safeFilename(stripScrape(cleanShareTitle(name)))
		if title == "" {
			title = "未命名"
		}
		key := "snode|" + dp.Name() + "|" + shareID + "|" + req.NodeID
		if dbTaskGet(key) == nil {
			slot, sid, pfx := dp.Name(), shareID, prefix
			dbTaskStart(key, slot, sid, title, nil, func() (string, []ShareFile, bool, error) {
				files, pending, e := scanDoubaoShareNode(cli, slot, sid, req.NodeID, pfx, 45*time.Second)
				return title, files, pending, e
			})
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "job": key, "provider": dp.Name(), "share_id": shareID, "title": title,
		})
	}
}

// apiDoubaoShareImport POST {link,node_id,path,category?} → 只导这一个子目录。
// 分段扫：本次扫 45 秒就返回 pending，前端再点/自动续，进度已落盘。
func apiDoubaoShareImport(store *Store, as *AccountStore, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Link     string `json:"link"`
			NodeID   string `json:"node_id"`
			Path     string `json:"path"`
			Category string `json:"category"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		p := findProvider(strings.TrimSpace(req.Link))
		dp, ok := p.(doubaoProvider)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "这不是豆包分享链接"})
			return
		}
		shareID, err := parseDoubaoLink(strings.TrimSpace(req.Link))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		cli := dp.client(as)
		if cli == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": providerCN(dp.Name()) + " 还没粘 Cookie"})
			return
		}
		if _, e := strconv.ParseInt(req.NodeID, 10, 64); e != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "目录标识无效，请回根目录重新进入"})
			return
		}
		cat, catErr := normalizeCategory(req.Category)
		if catErr != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": catErr.Error()})
			return
		}
		prefix := strings.Trim(filepath.ToSlash(strings.TrimSpace(req.Path)), "/")
		files, pending, err := scanDoubaoShareNode(cli, dp.Name(), shareID, req.NodeID, prefix, 45*time.Second)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "pending": false})
			return
		}
		name := prefix
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		title := safeFilename(stripScrape(cleanShareTitle(name)))
		if title == "" {
			title = "未命名"
		}
		if len(files) == 0 {
			writeJSON(w, map[string]interface{}{
				"ok": false, "pending": pending, "total": 0,
				"error": "这个目录里还没扫到视频" + map[bool]string{true: "（后台还在扫，再点一次继续）", false: ""}[pending],
			})
			return
		}
		info := ShareInfo{Provider: dp.Name(), ShareID: shareID, Title: title, Files: files}
		root := shareRootOf(files)
		// 已导入过这个分享：只补缺的集（同一目录再点一次不会重复）
		var have map[string]bool
		if shareExists(store, dp.Name(), shareID) {
			have = map[string]bool{}
			for _, it := range store.List() {
				if strings.EqualFold(it.Provider, dp.Name()) && it.ShareID == shareID && it.FileID != "" {
					have[it.FileID] = true
				}
			}
		}
		batch := buildShareItems(info, cat, title, nil, have, root)
		added := store.AddBatch(batch)
		if globalShares != nil {
			globalShares.Put(ShareMeta{
				Provider: dp.Name(), ShareID: shareID, Passcode: "", Category: cat, Show: title,
				Node: req.NodeID, Prefix: prefix,
				// 不自动追更（定时全量重扫没必要）；手动点「刷新」时按 Node 只扫这棵子树。
				AutoRefresh: false,
			})
		}
		log.Printf("[按目录导入] %s/%s ← %s：本次 %d 个视频，入库 %d 条%s",
			dp.Name(), title, prefix, len(files), added, map[bool]string{true: "（还在扫）", false: ""}[pending])
		writeJSON(w, map[string]interface{}{
			"ok": true, "added": added, "total": len(files),
			"pending": pending, "title": title,
		})
		// 分段扫时前端会自动续调（可能十几轮），每轮都重建 = 反复写盘。
		// 只在最后一段扫完时重建一次；中途关页面的话，下次导入/手动重建会补上。
		if added > 0 && !pending {
			rebuildSTRMAsync(store, self, "按目录导入")
		}
	}
}
