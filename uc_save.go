package main

// ---------------------------------------------------------------------------
// UC 302 直链：转存到自己盘 → UC TV 接口取免认证直链
//
// 与夸克 TV 302 同构（quark_save.go），差异只有三处：
//  ① PC 接口域名/参数不同（uc_api.go）；
//  ② 临时目录用 /bigpan_tmp/uc，与夸克互不干扰；
//  ③ TV 客户端是 UC 配置的 QuarkTV（见 quark_tv.go 的 uctvConf）。
//
// 为什么必须转存：UC 分享直链带 CDN callback，播放器没有登录 cookie 会 403；
// 转存后从自己盘取链走 TV 接口，令牌制、不带 callback，可安全 302。
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	ucTmpRoot = "bigpan_tmp"
	ucTmpName = "uc"
)

var (
	ucTmpMu  sync.Mutex
	ucTmpFID string
)

// ucEnsureTmpDir 确保自己盘上存在 /bigpan_tmp/uc，返回其 fid。
func ucEnsureTmpDir(pc *UC) string {
	ucTmpMu.Lock()
	defer ucTmpMu.Unlock()
	if ucTmpFID != "" {
		return ucTmpFID
	}
	cur := ucRootFID
	for _, name := range []string{ucTmpRoot, ucTmpName} {
		list, err := pc.ListDir(cur)
		if err != nil {
			log.Printf("[uc-tv] 列目录失败（%v）", err)
			return ""
		}
		found := ""
		for _, f := range list {
			if f.IsDir && f.Name == name {
				found = f.FID
				break
			}
		}
		if found == "" {
			found, err = pc.Mkdir(cur, name)
			if err != nil {
				log.Printf("[uc-tv] 建目录 %s 失败（%v）", name, err)
				return ""
			}
		}
		cur = found
	}
	ucTmpFID = cur
	log.Printf("[uc-tv] 临时目录就绪 /%s/%s (fid=%s)", ucTmpRoot, ucTmpName, cur)
	return cur
}

// ucFindOwnFile 在自己盘临时目录里按「同名同大小」找已转存的文件。
func ucFindOwnFile(pc *UC, pdir, name string, size int64) string {
	list, err := pc.ListDir(pdir)
	if err != nil {
		return ""
	}
	for _, f := range list {
		if !f.IsDir && f.Name == name && (size <= 0 || f.Size == size) {
			return f.FID
		}
	}
	return ""
}

// ucTransferForPlay 把分享里的文件转存到临时目录，返回自己盘的 fid。
func ucTransferForPlay(pc *UC, item *Item) (string, error) {
	pdir := ucEnsureTmpDir(pc)
	if pdir == "" {
		return "", fmt.Errorf("UC 临时目录创建失败，稍后再试")
	}
	fileName := item.Title + item.Ext
	if fid := ucFindOwnFile(pc, pdir, fileName, item.Size); fid != "" {
		ucRecordTemp(fid) // 复用即续期，连看的剧不会被 sweeper 清掉
		log.Printf("[uc-tv] 复用已转存文件 %s（fid=%s）", fileName, fid)
		return fid, nil
	}
	st, err := pc.ShareToken(item.ShareID, item.Passcode)
	if err != nil {
		return "", fmt.Errorf("分享已失效？%w", err)
	}
	token := item.FidToken
	if token == "" {
		// 老条目没有令牌：重扫分享树补齐（只扫同 fid 那条路径的父目录）。
		token = ucFindFidToken(pc, item, st)
		if token != "" {
			quarkRememberFidToken(item.ID, token)
		}
	}
	taskID, err := pc.SaveFile(item.ShareID, st, item.FileID, token, pdir)
	if err != nil {
		// 41020 = 令牌与当前会话不匹配（库里存的是旧会话签发的），
		// 用同一次 stoken 重扫分享树拿新令牌再试一次。
		if nt := ucFindFidToken(pc, item, st); nt != "" && nt != token {
			log.Printf("[uc-tv] 分享令牌已刷新，重试转存 %s", fileName)
			quarkRememberFidToken(item.ID, nt)
			taskID, err = pc.SaveFile(item.ShareID, st, item.FileID, nt, pdir)
		}
		if err != nil {
			return "", err
		}
	}
	fid, err := pc.PollTask(taskID)
	if err != nil {
		return "", err
	}
	if fid == "" {
		// UC 不回传 fid：转存完成后列临时目录，按「同名同大小」找回。
		for i := 0; i < 12 && fid == ""; i++ {
			fid = ucFindOwnFile(pc, pdir, fileName, item.Size)
			if fid != "" {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if fid == "" {
			return "", fmt.Errorf("转存完成但临时目录里没找到「%s」（可能空间不足）", fileName)
		}
	}
	ucRecordTemp(fid)
	log.Printf("[uc-tv] 已转存 %s → fid=%s", fileName, fid)
	return fid, nil
}

// ucFindFidToken 在分享树里按 file_id 找回 share_fid_token。
// 必须复用调用方那一次 ShareToken 的会话 —— UC 的 fid_token 与 stoken 绑定，
// 跨会话拿到的令牌转存会报 41020「token 校验异常」（与夸克同款事故）。
func ucFindFidToken(pc *UC, item *Item, st string) string {
	if st == "" {
		return ""
	}
	var walk func(pdir string, depth int) string
	walk = func(pdir string, depth int) string {
		if depth > 4 {
			return ""
		}
		list, err := pc.ShareDetail(item.ShareID, st, pdir)
		if err != nil {
			return ""
		}
		for _, f := range list {
			if f.FID == item.FileID {
				return f.ShareFidToken
			}
			if f.IsDir {
				if t := walk(f.FID, depth+1); t != "" {
					return t
				}
			}
		}
		return ""
	}
	return walk(ucRootFID, 0)
}

// ---- 302 直链短缓存（同夸克：60 秒，降 API 调用频率）----

var (
	ucLinkMu    sync.Mutex
	ucLinkCache = map[string]ucLinkEntry{}
	ucLinkTTL   = 60 * time.Second
)

type ucLinkEntry struct {
	url string
	at  time.Time
}

func ucLinkCached(id string) (string, bool) {
	ucLinkMu.Lock()
	defer ucLinkMu.Unlock()
	e, ok := ucLinkCache[id]
	if !ok || time.Since(e.at) > ucLinkTTL {
		return "", false
	}
	return e.url, true
}

func ucLinkRemember(id, u string) {
	ucLinkMu.Lock()
	defer ucLinkMu.Unlock()
	ucLinkCache[id] = ucLinkEntry{url: u, at: time.Now()}
}

func ucLinkForget(id string) {
	ucLinkMu.Lock()
	defer ucLinkMu.Unlock()
	delete(ucLinkCache, id)
}

// ucPickURL 取 TV 直链：优先转码流（H.264 MP4，浏览器/播放器都能解），
// 失败回落原画（片源本身是 MP4/H.264 或转码没排上时）。
func ucPickURL(ctx context.Context, tv *QuarkTV, fid string) (string, error) {
	u, err := tv.StreamingURL(ctx, fid)
	if err == nil {
		return u, nil
	}
	log.Printf("[uc-tv] 转码流不可用（%v），回落原画直链", err)
	return tv.DownloadURL(ctx, fid)
}

// ucTVPlayTarget 播放链路：TV 凭证 + PC Cookie 都必须在线。
func ucTVPlayTarget(item *Item) (PlayTarget, error) {
	if u, ok := ucLinkCached(item.ID); ok {
		return PlayTarget{URL: u, UA: "", Cookie: ""}, nil
	}
	tv := ucTVClient(globalAccounts)
	if tv == nil {
		return PlayTarget{}, fmt.Errorf("UC TV（302 直链）还没扫码登录：去「设置 → 网盘账号 → UC网盘 → 扫码登录」扫一下码")
	}
	pc := ucClient(globalAccounts)
	if pc == nil {
		return PlayTarget{}, fmt.Errorf("还没有导入 UC Cookie（转存要用它）：去「设置 → 网盘账号 → UC网盘 → 粘贴 Cookie」")
	}
	defer ucPlayMutex(item.ShareID + "|" + item.FileID)()

	fid := item.TVFID
	if fid == "" {
		var err error
		fid, err = ucTransferForPlay(pc, item)
		if err != nil {
			return PlayTarget{}, err
		}
		quarkRememberTVFID(item.ID, fid) // 字段通用，仅按 item.ID 写 TVFID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	u, err := ucPickURL(ctx, tv, fid)
	cancel()
	if err != nil {
		// 自愈：转存文件可能被清掉 → 丢掉记录重转存一次。
		log.Printf("[uc-tv] 取链失败（%v），自愈：重转存 %s", err, item.Title+item.Ext)
		quarkForgetTVFID(item.ID)
		fid2, err2 := ucTransferForPlay(pc, item)
		if err2 != nil {
			return PlayTarget{}, err2
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		u, err = ucPickURL(ctx2, tv, fid2)
		cancel2()
		if err != nil {
			return PlayTarget{}, err
		}
		quarkRememberTVFID(item.ID, fid2)
	}
	ucLinkRemember(item.ID, u)
	return PlayTarget{URL: u, UA: "", Cookie: ""}, nil
}

// ucPlayMutex 同一分享文件串行化，避免并发转存重复占空间。
var (
	ucPlayLocks sync.Map // key -> *sync.Mutex
)

func ucPlayMutex(key string) func() {
	v, _ := ucPlayLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ucTmpDirName 供界面/日志展示。
func ucTmpDirName() string { return "/" + ucTmpRoot + "/" + ucTmpName }

var _ = strings.TrimSpace
