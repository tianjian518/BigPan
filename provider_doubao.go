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
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// doubaoSlots 预留的账号槽位数。加账号就改这个数（UI 会同步多一张卡片）。
const doubaoSlots = 4

// doubaoShareRe 老格式分享：/drive/s/{16位hex}
var doubaoShareRe = regexp.MustCompile(`(?i)doubao\.com/drive/s/([A-Za-z0-9]{8,40})`)

// doubaoShrRe 新格式分享：/drive/shr/{27位}，属飞书体系，暂不支持。
var doubaoShrRe = regexp.MustCompile(`(?i)doubao\.com/drive/shr/([A-Za-z0-9]+)`)

// parseDoubaoLink 解析豆包分享链接，返回 share_id。
func parseDoubaoLink(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请粘贴分享链接")
	}
	if doubaoShrRe.MatchString(raw) {
		return "", fmt.Errorf("这是豆包新格式分享（/drive/shr/），暂不支持；自己的文件请用 doubao:// 方式浏览")
	}
	if m := doubaoShareRe.FindStringSubmatch(raw); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("这不是豆包分享链接")
}

// parseDoubaoOwnLink 解析自己盘的伪链接 doubao://<provider>/<node_id>。
func parseDoubaoOwnLink(raw string) (provider, nodeID string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "doubao://") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, "doubao://"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
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
	if pv, _, ok := parseDoubaoOwnLink(link); ok {
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
	if pv, nodeID, ok := parseDoubaoOwnLink(link); ok {
		if pv != p.Name() {
			return ShareInfo{}, fmt.Errorf("这个链接属于 %s，不是 %s", pv, p.Name())
		}
		title, files, err := scanDoubaoOwn(cli, nodeID)
		if err != nil {
			return ShareInfo{}, err
		}
		return ShareInfo{Provider: p.Name(), Title: title, Files: files, Logged: true}, nil
	}

	// B. 别人的分享
	shareID, err := parseDoubaoLink(link)
	if err != nil {
		return ShareInfo{}, err
	}
	title, files, err := scanDoubaoShare(cli, shareID)
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
	if shareID == "" {
		return "", nil, fmt.Errorf("自己盘里的条目不参与追更，重新导入即可")
	}
	return scanDoubaoShare(cli, shareID)
}

func (p doubaoProvider) ResolvePlay(item *Item) (PlayTarget, error) {
	cli := p.client(globalAccounts)
	if cli == nil {
		return PlayTarget{}, fmt.Errorf("%s 还没粘 Cookie", providerCN(p.Name()))
	}
	// item.FileID 存 node_id；item.FidToken 存豆包的 key（多数文件为空串）；
	// item.ShareID 非空表示来自分享，取链要带 share_id。
	list, err := cli.dbPlayInfo(item.FileID, item.FidToken, item.ShareID)
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
func scanDoubaoOwn(cli *Doubao, nodeID string) (string, []ShareFile, error) {
	const maxFiles, maxDirs = 20000, 5000
	type job struct {
		id, prefix string
	}
	queue := []job{{id: nodeID}}
	seen := map[string]bool{nodeID: true}
	title := "豆包文件"
	var files []ShareFile
	first := true
	start := time.Now()
	for len(queue) > 0 && len(files) < maxFiles && len(seen) < maxDirs {
		j := queue[0]
		queue = queue[1:]
		list, err := cli.dbListDirAll(j.id)
		if err != nil {
			if first {
				return "", nil, fmt.Errorf("读取豆包目录失败：%w", err)
			}
			continue
		}
		first = false
		// 单层且只有一个目录时，用它的名字当剧名（和 UC 的折叠规则一致）
		if j.id == nodeID && len(list) == 1 && list[0].IsDir() {
			title = list[0].Name
		}
		for _, n := range list {
			p := n.Name
			if j.prefix != "" {
				p = j.prefix + "/" + n.Name
			}
			if n.IsDir() {
				if !seen[n.ID] {
					seen[n.ID] = true
					queue = append(queue, job{id: n.ID, prefix: p})
				}
				continue
			}
			if isVideo(n.Name) {
				files = append(files, ShareFile{
					ID: n.ID, Name: n.Name, Size: n.Size, Path: p, FidToken: n.Key,
				})
			}
		}
		if len(files) > 0 && len(files)/200 > (len(files)-len(list))/200 {
			log.Printf("[豆包] 扫描中：%d 个目录 / %d 个视频 / 已用 %v", len(seen), len(files), time.Since(start).Round(time.Second))
		}
	}
	log.Printf("[豆包] 扫描完成：%d 个目录 / %d 个视频 / 耗时 %v", len(seen), len(files), time.Since(start).Round(time.Second))
	if len(files) == 0 {
		return "", nil, fmt.Errorf("这个目录里没有视频文件")
	}
	return title, files, nil
}

// scanDoubaoShare 递归扫描别人的分享目录树。
func scanDoubaoShare(cli *Doubao, shareID string) (string, []ShareFile, error) {
	title, nodes, err := cli.dbShareOverview(shareID)
	if err != nil {
		return "", nil, err
	}
	type job struct {
		id, prefix string
	}
	seen := map[string]bool{}
	var queue []job
	var files []ShareFile

	for _, n := range nodes {
		if n.IsDir() {
			if !seen[n.ID] {
				seen[n.ID] = true
				queue = append(queue, job{id: n.ID, prefix: n.Name})
			}
			continue
		}
		if isVideo(n.Name) {
			files = append(files, ShareFile{
				ID: n.ID, Name: n.Name, Size: n.Size, Path: n.Name, Code: shareID, FidToken: n.Key,
			})
		}
	}
	for len(queue) > 0 && len(files) < 20000 && len(seen) < 5000 {
		j := queue[0]
		queue = queue[1:]
		list, err := cli.dbShareListDir(shareID, j.id)
		if err != nil {
			continue
		}
		for _, n := range list {
			p := j.prefix + "/" + n.Name
			if n.IsDir() {
				if !seen[n.ID] {
					seen[n.ID] = true
					queue = append(queue, job{id: n.ID, prefix: p})
				}
				continue
			}
			if isVideo(n.Name) {
				files = append(files, ShareFile{
					ID: n.ID, Name: n.Name, Size: n.Size, Path: p, Code: shareID, FidToken: n.Key,
				})
			}
		}
	}
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
				it.Link = "doubao://" + pv + "/" + n.ID
			} else {
				it.Video = isVideo(n.Name)
				it.Link = "doubao://" + pv + "/" + n.ID
			}
			items = append(items, it)
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "provider": pv, "root_id": rootID, "node_id": nodeID, "items": items,
		})
	}
}
