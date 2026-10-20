package main

// ---------------------------------------------------------------------------
// 夸克播放时转存 + TV 直链（「一个开关」设计里开关打开时的取流链路）
//
// 为什么需要转存：夸克普通版分享直链绑登录会话（UA+Cookie），客户端直连
// 会被 412 拒绝，只能本机中转；夸克 TV 版接口能取「免认证直链」（可 302），
// 但只认「自己盘上」的文件 fid。于是开关打开时的链路变成：
//
//	PC Cookie 调 share/save 把分享文件转存到自己盘 /bigpan_tmp/quark
//	→ 异步 task 轮询，拿到转存后的 fid（记入 Item.TVFID，播过直接复用）
//	→ 夸克 TV 接口对这个 fid 取免认证直链
//	→ 302 跳播放器，数据网盘 CDN 直拉，不过本机
//
// 模式照搬 provider_baidu.go（百度已验证的成熟方案）：固定名临时目录 +
// 按文件名/大小查重复用 + sweeper 定时清（见 quark_sweep.go）。
// 接口形态与 quark-auto-save 等成熟实现一致（2026-10 查证）：
//
//	转存  POST /1/clouddrive/share/sharepage/save
//	      {fid_list, fid_token_list, to_pdir_fid, pwd_id, stoken, pdir_fid:"0", scene:"link"}
//	      → {code:0, data:{task_id}}
//	轮询  GET /1/clouddrive/task?task_id=xxx&retry_index=N（500ms 一轮）
//	      → data.status==2 时 data.save_as.save_as_top_fids[0] 就是新 fid
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// quarkTmpDir 自己盘上的临时目录（固定名，sweeper 只扫这里）。
const quarkTmpDir = "/bigpan_tmp/quark"

// 临时文件保留时长：百度是 30 分钟；夸克 302 后客户端直连、不耗本机流量，
// 连看场景多，保守取 2 小时（§23.47 决策点②）。连看续期由 sweeper 负责。
const quarkTmpTTL = 2 * time.Hour

// 转存异步任务轮询上限（分享转存是元数据操作，正常几秒内完成；
// 60 秒足够覆盖慢账期，超时报错让用户重试）。
const quarkTaskTimeout = 60 * time.Second

// ---- 播放解析互斥锁：同一分享文件的多跳解析串行化，避免重复转存 ----

var (
	quarkPlayMu   sync.Mutex
	quarkPlayLock = map[string]*sync.Mutex{}
)

func quarkPlayMutex(key string) func() {
	quarkPlayMu.Lock()
	mu, ok := quarkPlayLock[key]
	if !ok {
		mu = &sync.Mutex{}
		quarkPlayLock[key] = mu
	}
	quarkPlayMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// ---- 临时目录 ----

var (
	quarkTmpMu  sync.Mutex
	quarkTmpFID string // 进程内缓存：临时目录 fid，避免每次播放都调接口
)

// quarkTmpDirFID 确保 /bigpan_tmp/quark 存在，返回其 fid（失败返回 ""）。
// 逐级创建（bigpan_tmp → quark）：夸克单层建目录接口是
// POST /1/clouddrive/file {pdir_fid, file_name, dir_path:"", dir_init_lock:false}。
// 账号切换后缓存要失效（新号的盘里没有这个目录）——由 quarkTmpDirReset 负责。
func quarkTmpDirFID(q *Quark) string {
	quarkTmpMu.Lock()
	defer quarkTmpMu.Unlock()
	if quarkTmpFID != "" {
		return quarkTmpFID
	}
	fid := quarkRootFID
	for _, name := range []string{"bigpan_tmp", "quark"} {
		sub, err := quarkFindDirFID(q, fid, name)
		if err != nil || sub == "" {
			sub, err = quarkMkdir(q, fid, name)
			if err != nil {
				log.Printf("[quark-tv] 创建临时目录 %s 失败: %v", name, err)
				return ""
			}
		}
		fid = sub
	}
	quarkTmpFID = fid
	log.Printf("[quark-tv] 临时目录就绪 %s (fid=%s)", quarkTmpDir, fid)
	return fid
}

// quarkTmpDirReset 丢弃临时目录 fid 缓存（账号切换/目录被清时调）。
func quarkTmpDirReset() {
	quarkTmpMu.Lock()
	quarkTmpFID = ""
	quarkTmpMu.Unlock()
}

// quarkFindDirFID 在父目录下按名字找子目录，返回 fid（找不到返回 ""）。
func quarkFindDirFID(q *Quark, pdirFID, name string) (string, error) {
	list, err := q.ListDir(pdirFID)
	if err != nil {
		return "", err
	}
	for _, f := range list {
		if f.IsDir && f.Name == name {
			return f.FID, nil
		}
	}
	return "", nil
}

// quarkMkdir 在父目录下建一层目录，返回新目录 fid。
func quarkMkdir(q *Quark, pdirFID, name string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FID string `json:"fid"`
		} `json:"data"`
	}
	err := q.api(http.MethodPost, "/file", nil, map[string]interface{}{
		"pdir_fid":      pdirFID,
		"file_name":     name,
		"dir_path":      "",
		"dir_init_lock": false,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("夸克建目录：%s（code %d）", out.Message, out.Code)
	}
	return out.Data.FID, nil
}

// ---- 转存 ----

// quarkFindOwnFile 在临时目录里按文件名+大小找已转存的文件（复用，不重复转存）。
// 大小只在 item.Size>0 时校验（导入时可能没记录大小）。
func quarkFindOwnFile(q *Quark, pdirFID, name string, size int64) string {
	list, err := q.ListDir(pdirFID)
	if err != nil {
		return ""
	}
	for _, f := range list {
		if !f.IsDir && f.Name == name && (size == 0 || f.Size == size) {
			return f.FID
		}
	}
	return ""
}

// quarkSaveFile 把分享文件转存到临时目录，返回转存后的 fid。
// share/save 是异步任务：先拿 task_id，再轮询 /task 到 status==2，
// 结果里的 save_as_top_fids[0] 就是转存后新文件的 fid。
func quarkSaveFile(q *Quark, pwdID, stoken, shareFID, shareFidToken, toPdirFID string) (string, error) {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	// __dt/__t 是夸克的风控参数（成熟实现都带），随机化更像真人操作。
	// __t 必须是「浮点秒」（与网页端 datetime.now().timestamp() 同格式），
	// 毫秒整数会被风控判定异常——格式照 Cp0204/quark-auto-save 复刻。
	__dt := int64(rand.Intn(4*60*1000) + 60*1000)
	__t := strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 3, 64)
	params := map[string]string{
		"pr": "ucpro", "fr": "pc", "uc_param_str": "",
		"app":  "clouddrive",
		"__dt": fmt.Sprintf("%d", __dt),
		"__t":  __t,
	}
	err := q.api(http.MethodPost, "/share/sharepage/save", params, map[string]interface{}{
		"fid_list":       []string{shareFID},
		"fid_token_list": []string{shareFidToken},
		"to_pdir_fid":    toPdirFID,
		"pwd_id":         pwdID,
		"stoken":         stoken,
		"pdir_fid":       "0",
		"scene":          "link",
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.TaskID == "" {
		msg := out.Message
		if msg == "" {
			msg = "转存失败（分享可能已失效）"
		}
		return "", fmt.Errorf("夸克转存：%s（code %d）", msg, out.Code)
	}
	return quarkPollTask(q, out.Data.TaskID)
}

// quarkPollTask 轮询转存任务直到完成，返回转存后文件的 fid。
func quarkPollTask(q *Quark, taskID string) (string, error) {
	deadline := time.Now().Add(quarkTaskTimeout)
	retry := 0
	for {
		var out struct {
			Status int `json:"status"`
			Code   int `json:"code"`
			Data   struct {
				Status    int    `json:"status"` // 2 = 完成
				TaskTitle string `json:"task_title"`
				SaveAs    struct {
					SaveAsTopFids []string `json:"save_as_top_fids"`
				} `json:"save_as"`
			} `json:"data"`
		}
		__dt := int64(rand.Intn(4*60*1000) + 60*1000)
		__t := strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 3, 64)
		params := map[string]string{
			"pr": "ucpro", "fr": "pc", "uc_param_str": "",
			"task_id":     taskID,
			"retry_index": fmt.Sprintf("%d", retry),
			"__dt":        fmt.Sprintf("%d", __dt),
			"__t":         __t,
		}
		if err := q.api(http.MethodGet, "/task", params, nil, &out); err != nil {
			return "", err
		}
		if out.Data.Status == 2 {
			if len(out.Data.SaveAs.SaveAsTopFids) == 0 {
				return "", fmt.Errorf("转存任务完成了但没拿到新文件 ID")
			}
			return out.Data.SaveAs.SaveAsTopFids[0], nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("转存任务超时（%s），稍后再试", out.Data.TaskTitle)
		}
		retry++
		time.Sleep(500 * time.Millisecond)
	}
}

// quarkRefreshFidToken 重扫分享树找文件的 share_fid_token（转存令牌过期时刷新用）。
// BFS 找到即返回，不展开全集；上限 200 个目录，防超大分享拖死。
// quarkRefreshFidToken 重扫分享树补 share_fid_token（BFS，200 目录上限）。
// st 由调用方传入（与转存同一个 stoken 会话）：share_fid_token 与 stoken
// 是绑定的，混用不同会话的令牌会被夸克以 41020「token校验异常」拒掉。
func quarkRefreshFidToken(q *Quark, item *Item, st string) string {
	queue := []string{""}
	seen := map[string]bool{"": true}
	for len(queue) > 0 && len(seen) < 200 {
		cur := queue[0]
		queue = queue[1:]
		list, err := q.ShareList(item.ShareID, st, cur)
		if err != nil {
			continue
		}
		for _, f := range list {
			if f.IsDir {
				if !seen[f.FID] {
					seen[f.FID] = true
					queue = append(queue, f.FID)
				}
				continue
			}
			if f.FID == item.FileID {
				return f.ShareFidToken
			}
		}
	}
	return ""
}

// ---- 删除（sweeper 用）----

// quarkDeleteOwn 删除自己盘上的文件（转存回收站，sweeper 用）。
// POST /1/clouddrive/file/delete {action_type:2, filelist:[fid], exclude_fids:[]}
func quarkDeleteOwn(q *Quark, fid string) error {
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	err := q.api(http.MethodPost, "/file/delete", nil, map[string]interface{}{
		"action_type":  2,
		"filelist":     []string{fid},
		"exclude_fids": []string{},
	}, &out)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("删除返回 code=%d（%s）", out.Code, out.Message)
	}
	return nil
}

// ---- 播放链路入口 ----

// quarkTVPlayTarget 开关打开时的夸克取链：转存（或复用）→ TV 免认证直链。
// 返回的 PlayTarget UA/Cookie 全空——TV 直链不绑会话，客户端直连 CDN 即可。
//
// 清晰度策略（2026-10-20 定稿，§23.59）：**只走 TV 转码流**，拿不到才回落原画。
// 曾经有个「画质优先（原画）」选项，已下架——原画直链单连接被 CDN 限速到
// ~100KB/s（沙箱 + 用户家宽双端实锤），而 302 模式下连接归播放器管，
// 爆米花/飞牛饿起来只开 1 个连接（日志实锤：转码流模式 1 分钟开 12+ 个，
// 原画模式 6 分钟开 1 个），原画 302 是死路，按钮纯属误导。
func quarkTVPlayTarget(item *Item) (PlayTarget, error) {
	// ⓪ 302 直链缓存：播放器每开一个连接就来要一次链（实测 1 分钟 12+ 次），
	// 逐个打夸克 TV 接口既没必要也拉高风控面。链本身几分钟内有效，
	// 缓存 60 秒，API 调用量立刻降到约十分之一。
	if u, ok := quarkLinkCached(item.ID); ok {
		return PlayTarget{URL: u, UA: "", Cookie: ""}, nil
	}
	// ① TV 扫码凭证（取免认证直链用）
	qtv := quarkTVClient(globalAccounts)
	if qtv == nil {
		return PlayTarget{}, fmt.Errorf("夸克 TV（302 直链）还没扫码登录：去「设置 → 网盘账号 → 夸克网盘 → 扫码登录」扫一下码")
	}
	// ② PC Cookie（转存要用；分享解析本身不需要登录）
	q := quarkClient(globalAccounts)
	if q == nil {
		return PlayTarget{}, fmt.Errorf("还没有导入夸克 Cookie（转存要用它）：去「设置 → 网盘账号 → 夸克网盘 → 粘贴 Cookie」")
	}
	// ③ 同一分享文件串行化：连播/多设备同时打同一集会并发解析，
	// 不串行会重复转存（浪费空间）还可能踩夸克的后端同步延迟。
	defer quarkPlayMutex(item.ShareID + "|" + item.FileID)()

	// ④ 拿转存后的 fid：TVFID 有记录直接复用；没有 → 转存
	fid := item.TVFID
	if fid == "" {
		var err error
		fid, err = quarkTransferForPlay(q, item)
		if err != nil {
			return PlayTarget{}, err
		}
		quarkRememberTVFID(item.ID, fid)
	}
	// ⑤ TV 接口取免认证直链（token 失效自动刷新重试在 qtvFileLink 内处理）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	u, err := qtvPickURL(ctx, qtv, fid)
	cancel()
	if err != nil {
		// 自愈：转存文件可能已被 sweeper 清掉（TVFID 失效）→ 丢掉记录重转存一次。
		// 这是设计内容错（不报错给用户），只在重转存也失败时才把错误抛出去。
		log.Printf("[quark-tv] 取链失败（%v），自愈：重转存 %s", err, item.Title+item.Ext)
		quarkForgetTVFID(item.ID)
		fid2, err2 := quarkTransferForPlay(q, item)
		if err2 != nil {
			return PlayTarget{}, err2
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		u, err = qtvPickURL(ctx2, qtv, fid2)
		cancel2()
		if err != nil {
			return PlayTarget{}, err
		}
		quarkRememberTVFID(item.ID, fid2)
	}
	quarkLinkRemember(item.ID, u)
	return PlayTarget{URL: u, UA: "", Cookie: ""}, nil
}

// qtvPickURL 取 TV 直链：优先转码流（H.264 MP4，各播放器都能解，且该 CDN
// 不限单连接——用户家宽实测多连接叠加 16MB/s，§23.58），失败回落原画
// （万一片源本身就是 MP4/H.264 或转码任务没排上）。
func qtvPickURL(ctx context.Context, qtv *QuarkTV, fid string) (string, error) {
	u, err := qtv.StreamingURL(ctx, fid)
	if err == nil {
		return u, nil
	}
	log.Printf("[quark-tv] 转码流不可用（%v），回落原画直链", err)
	return qtv.DownloadURL(ctx, fid)
}

// ---- 302 直链短缓存（降 API 调用频率，顺带降风控面）----
//
// 只缓存「最终 URL」，TTL 60s：TV 直链几分钟内有效，缓存远小于其寿命；
// 真过期也是播放器吃一个 403 再要一次新链的事，无感。
var (
	quarkLinkMu    sync.Mutex
	quarkLinkCache = map[string]quarkLinkEntry{}
	quarkLinkTTL   = 60 * time.Second
)

type quarkLinkEntry struct {
	url string
	at  time.Time
}

func quarkLinkCached(id string) (string, bool) {
	quarkLinkMu.Lock()
	defer quarkLinkMu.Unlock()
	e, ok := quarkLinkCache[id]
	if !ok {
		return "", false
	}
	if time.Since(e.at) > quarkLinkTTL {
		delete(quarkLinkCache, id)
		return "", false
	}
	return e.url, true
}

func quarkLinkRemember(id, url string) {
	quarkLinkMu.Lock()
	defer quarkLinkMu.Unlock()
	quarkLinkCache[id] = quarkLinkEntry{url: url, at: time.Now()}
}

// quarkLinkReset 清空全部缓存（账号切换时调：新号的直链/转存目录都变了）。
func quarkLinkReset() {
	quarkLinkMu.Lock()
	defer quarkLinkMu.Unlock()
	quarkLinkCache = map[string]quarkLinkEntry{}
}

// ---- 分享级失败缓存（防播放器无限重试风暴）----
//
// 爆米花等播放器对失败条目会疯狂重试（2026-10-20 实测 2 分钟 88 次），
// 每次都全量走转存流程：查重 → ShareToken → BFS 整棵分享树。分享级失败
// （分享失效 / 被审核屏蔽）短时间内不会自愈，缓存 TTL 内直接返回上次的
// 错误：既止住日志风暴，也避免高频失败请求触发夸克风控。
//
// 只缓存「分享级」失败；转存/取链的临时性错误（限流、任务超时等）不缓存，
// 恢复后要能立刻重试。条目转存成功即清缓存。
var (
	quarkFailMu    sync.Mutex
	quarkFailCache = map[string]quarkFailEntry{}
	quarkFailTTL   = 10 * time.Minute
)

type quarkFailEntry struct {
	err error
	at  time.Time
}

func quarkFailKey(shareID, fileID string) string { return shareID + "|" + fileID }

// quarkFailCached 取缓存的分享级失败（过期自动清除）。
func quarkFailCached(shareID, fileID string) (error, bool) {
	quarkFailMu.Lock()
	defer quarkFailMu.Unlock()
	e, ok := quarkFailCache[quarkFailKey(shareID, fileID)]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > quarkFailTTL {
		delete(quarkFailCache, quarkFailKey(shareID, fileID))
		return nil, false
	}
	return e.err, true
}

// quarkFailRemember 记一个分享级失败（调用方需确保 err 是分享级、非临时性）。
func quarkFailRemember(shareID, fileID string, err error) {
	quarkFailMu.Lock()
	quarkFailCache[quarkFailKey(shareID, fileID)] = quarkFailEntry{err: err, at: time.Now()}
	quarkFailMu.Unlock()
}

// quarkFailClear 清掉缓存（转存成功时调：分享级状态已自愈或本条已跳过）。
func quarkFailClear(shareID, fileID string) {
	quarkFailMu.Lock()
	delete(quarkFailCache, quarkFailKey(shareID, fileID))
	quarkFailMu.Unlock()
}

// quarkFailCacheReset 清空全部缓存（账号切换时调：新号的分享状态不同）。
func quarkFailCacheReset() {
	quarkFailMu.Lock()
	quarkFailCache = map[string]quarkFailEntry{}
	quarkFailMu.Unlock()
}

// quarkTransferForPlay 播放前的转存：确保临时目录 → 查重复用 → 没有才转存。
// 返回自己盘上的 fid。
func quarkTransferForPlay(q *Quark, item *Item) (string, error) {
	// 分享级失败缓存：命中则直接返回上次错误，不打任何夸克 API。
	if ferr, ok := quarkFailCached(item.ShareID, item.FileID); ok {
		log.Printf("[quark-tv] 失败缓存命中（%v 内不重试）%s: %v", quarkFailTTL, item.Title+item.Ext, ferr)
		return "", ferr
	}
	pdir := quarkTmpDirFID(q)
	if pdir == "" {
		return "", fmt.Errorf("夸克临时目录（%s）创建失败，稍后再试", quarkTmpDir)
	}
	// 查重复用：同名同大小视为同一个文件（连看时 sweeper 会续期，不会误清）
	fileName := item.Title + item.Ext
	if fid := quarkFindOwnFile(q, pdir, fileName, item.Size); fid != "" {
		quarkRecordTemp(fid) // 复用也续期：连看的剧不会被 sweeper 清掉
		log.Printf("[quark-tv] 复用已转存文件 %s（fid=%s）", fileName, fid)
		return fid, nil
	}
	// stoken：游客态即可拿（ShareToken 不需要登录）
	st, err := q.ShareToken(item.ShareID, item.Passcode)
	if err != nil {
		// ShareToken 失败 = 分享级（链接失效/提取码错），缓存。
		ferr := fmt.Errorf("分享已失效？%w", err)
		quarkFailRemember(item.ShareID, item.FileID, ferr)
		return "", ferr
	}
	// share_fid_token：转存必带。老条目（本字段上线前导入的）库里是空——
	// 曾因「token != "" 才刷新」的守卫，空 token 直接转存必败且永远不刷新，
	// 所有存量条目 302 播放全挂（2026-10-20 线上 41020 事故）。
	// 现在：空 token 直接用同一个 stoken 会话重扫分享补令牌，失败也照样刷新重试。
	token := item.FidToken
	var fid string
	if token != "" {
		fid, err = quarkSaveFile(q, item.ShareID, st, item.FileID, token, pdir)
	}
	if err != nil || token == "" {
		if nt := quarkRefreshFidToken(q, item, st); nt != "" && nt != token {
			log.Printf("[quark-tv] 分享令牌已刷新，重试转存 %s", fileName)
			token = nt
			item.FidToken = nt
			quarkRememberFidToken(item.ID, nt)
			fid, err = quarkSaveFile(q, item.ShareID, st, item.FileID, token, pdir)
		} else if err == nil {
			// 两种「没找到」要分开说：审核屏蔽（元数据实锤）vs 真被删（猜）。
			// 屏蔽是持久状态，记失败缓存，止住播放器无限重试。
			if meta, serr := q.ShareStatus(item.ShareID, st); serr == nil && meta.Blocked() {
				err = fmt.Errorf("分享内容被夸克审核屏蔽（%d 个文件违规下架），请更换分享源或删除该条目", meta.ViolationCnt)
			} else {
				err = fmt.Errorf("分享里没找到这个文件（可能已被分享者删除）")
			}
			quarkFailRemember(item.ShareID, item.FileID, err)
		}
	}
	if err != nil {
		return "", err
	}
	quarkFailClear(item.ShareID, item.FileID) // 转存成功：分享是活的，清失败缓存
	quarkRecordTemp(fid)                      // 记账：sweeper 按 2 小时 TTL 清，连看会续期
	log.Printf("[quark-tv] 已转存 %s → fid=%s", fileName, fid)
	return fid, nil
}

// quarkRememberTVFID / quarkForgetTVFID / quarkRememberFidToken
// 回写条目字段（item 是 Store 副本，直接改不落库，必须走 Update）。
func quarkRememberTVFID(itemID, fid string) {
	if globalStore != nil && itemID != "" {
		globalStore.Update(itemID, func(it *Item) { it.TVFID = fid })
	}
}

func quarkForgetTVFID(itemID string) {
	if globalStore != nil && itemID != "" {
		globalStore.Update(itemID, func(it *Item) { it.TVFID = "" })
	}
}

func quarkRememberFidToken(itemID, token string) {
	if globalStore != nil && itemID != "" {
		globalStore.Update(itemID, func(it *Item) { it.FidToken = token })
	}
}
