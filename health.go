package main

// ---------------------------------------------------------------------------
// 分享失效巡检
//
// 为什么需要：分享链接会失效（分享者取消、过期、内容被删）。失效后条目还留在
// 库里，媒体库照样显示海报，点播放却失败 —— 排查时极易被误判成"播放器坏了"
// "WebDAV 兼容问题""网盘限速"，浪费大量时间。
//
// 做法：定时全量遍历所有登记分享，只探测、不入库。确认失效后：
//   ① 删除该分享在 items.json 里的全部条目
//   ② 删除 shares.json 里的追更登记
//   ③ 全量重建 STRM（cleanSTRM 会连带清掉对应 .strm 与空目录）
//
// 防误删（这是本功能最关键的部分）：
//   - 只认「分享本身没了」类错误；网络超时/限流/登录过期/账号未导入一律不算。
//   - 必须连续 healthFailLimit 轮都确认失效才动手（3 小时一轮 → 6 小时）。
//   - 同一轮里多数分享同时报失效 → 判定为账号级故障（登录过期/风控），整轮不清理。
//   - 只按 (provider, shareID) 精确删除，绝不碰别的分享的条目。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// healthFailLimit 连续多少轮确认失效才清理。
const healthFailLimit = 2

// healthLock 保护最近一次巡检结果（给管理接口查询）。
var healthLock sync.Mutex

// suspectedShare 一轮扫描里被判定为「分享没了」的明细。
// 前端用它列出清单，让用户自己决定是否立即清理（而不是只能等定时巡检）。
type suspectedShare struct {
	Provider string `json:"provider"`
	ShareID  string `json:"share_id"`
	Show     string `json:"show"`
	Error    string `json:"error"`
	Fail     int    `json:"fail"`
}

// healthState 最近一轮巡检的摘要。
type healthState struct {
	LastRun   time.Time `json:"last_run"`
	Checked   int       `json:"checked"`
	Gone      int       `json:"gone"`       // 本轮确认失效的分享数
	Pending   int       `json:"pending"`    // 累计失败但未达阈值的分享数
	Purged    int       `json:"purged"`     // 本轮清理的分享数
	PurgedEps int       `json:"purged_eps"` // 本轮删掉的条目数
	// Suspected 本轮所有「确认失效」的分享明细，供前端一键确认清理
	Suspected []suspectedShare `json:"suspected,omitempty"`
	Note      string           `json:"note,omitempty"`
}

var lastHealth healthState

// startShareHealthChecker 启动全量巡检（every<=0 关闭）。
// 首轮延迟 10 分钟：避开开机时的账号加载与追更首轮，且给网盘一点喘息。
func startShareHealthChecker(store *Store, every time.Duration, self string) {
	if every <= 0 || globalShares == nil {
		return
	}
	go func() {
		first := 10 * time.Minute
		if every < first {
			first = every
		}
		log.Printf("[巡检] 分享失效巡检已开启：%v 后首轮，之后每 %v 全量扫一遍（连续 %d 轮确认失效才清理）",
			first, every, healthFailLimit)
		time.Sleep(first)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			healthCycle(store, self)
			<-t.C
		}
	}()
}

// healthTargets 本轮要扫的分享：登记表 ∪ 条目里出现过的分享。
//
// 必须包含三类「不在追更列表里」的：
//   - 已完结（AutoRefresh=false）：不再追更，但分享照样会失效；
//   - 已隐藏（Hidden=true）：用户点过「隐藏」，条目还留在媒体库里；
//   - 无登记（老数据、登记被清过）：items.json 里还有条目，shares.json 没记录。
//
// 这三类失效后，媒体库仍会显示、点开却播不了 —— 正是要清掉的对象。
func healthTargets(store *Store) []ShareMeta {
	if globalShares == nil {
		return nil
	}
	metas := globalShares.List()
	seen := make(map[string]bool, len(metas))
	for _, m := range metas {
		seen[shareKey(m.Provider, m.ShareID)] = true
	}
	for _, it := range store.List() {
		if it.Provider == "" || it.ShareID == "" {
			continue
		}
		if it.SubUserID != "" {
			continue // 订阅号收录的分享由订阅号面板负责，不参加通用巡检
		}
		k := shareKey(it.Provider, it.ShareID)
		if seen[k] {
			continue
		}
		seen[k] = true
		metas = append(metas, ShareMeta{
			Provider: it.Provider, ShareID: it.ShareID,
			Passcode: it.Passcode, Category: it.Category, Show: it.Show,
		})
	}
	return metas
}

// healthCycle 跑一轮全量巡检：只探测分享是否还在，不写条目。
func healthCycle(store *Store, self string) healthState {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	metas := healthTargets(store)
	st := healthState{LastRun: time.Now(), Checked: len(metas)}
	if len(metas) == 0 {
		recordHealth(st)
		return st
	}

	var dead []ShareMeta
	pending := 0
	for i := range metas {
		m := metas[i]
		p := providerByName(m.Provider)
		if p == nil {
			continue
		}
		// 只探测，不入库：RefreshShare 返回文件列表，这里直接丢弃。
		_, _, err := p.RefreshShare(m.ShareID, m.Passcode)
		now := time.Now()
		m.LastCheck = now
		if err == nil {
			if m.FailCount != 0 || m.LastError != "" {
				m.FailCount, m.LastError = 0, ""
				globalShares.Put(m)
			}
			continue
		}
		if !shareGone(err) {
			// 网络/限流/账号问题，或「列表为空但分享还在」（典型：139 的 ISO
			// 分享，scan139Share 主动跳过 .iso 后文件数为 0）——都不算失效。
			// 顺带清零历史计数：避免「一轮网络抖动 + 一轮真失效」凑成连续两轮。
			if m.FailCount != 0 || m.LastError != "" {
				m.FailCount, m.LastError = 0, ""
				globalShares.Put(m)
				log.Printf("[巡检] %s/%s 计数已清零（本轮错误不属于失效类）", m.Provider, m.Show)
			}
			log.Printf("[巡检] %s/%s 探测失败（非失效，不计数）：%v", m.Provider, m.Show, err)
			continue
		}
		m.FailCount++
		m.LastError = err.Error()
		globalShares.Put(m)
		st.Gone++
		pending++
		st.Suspected = append(st.Suspected, suspectedShare{
			Provider: m.Provider, ShareID: m.ShareID, Show: m.Show,
			Error: m.LastError, Fail: m.FailCount,
		})
		log.Printf("[巡检] %s/%s 确认失效（第 %d/%d 轮）：%v",
			m.Provider, m.Show, m.FailCount, healthFailLimit, err)
		if m.FailCount >= healthFailLimit {
			dead = append(dead, m)
		}
	}

	// 账号级故障保护：多数分享同时失效，几乎不可能是真的全挂了。
	if st.Gone >= 2 && st.Gone*2 >= len(metas) {
		st.Note = "多数分享同时报失效，疑似账号级故障（登录过期/风控），本轮不清理"
		st.Pending = pending
		log.Printf("[巡检] %s：%d/%d 个分享报失效", st.Note, st.Gone, len(metas))
		recordHealth(st)
		return st
	}

	// 执行清理
	for _, m := range dead {
		eps := purgeShare(store, m)
		st.Purged++
		st.PurgedEps += eps
		log.Printf("[巡检] 已清理失效分享 %s/%s：删除 %d 个条目（最后一次错误：%s）",
			m.Provider, m.Show, eps, m.LastError)
		appendGoneLog(m, eps)
	}
	st.Pending = pending - st.Purged
	if st.Purged > 0 {
		log.Printf("[巡检] 本轮清理 %d 个失效分享、共 %d 个条目", st.Purged, st.PurgedEps)
	}
	recordHealth(st)

	// 有清理才重建 STRM（重建本身会删掉不再存在的 .strm 与空目录）。
	// force=false：地址变过时只删孤儿、写新文件，存量等用户手动全量重建。
	if st.Purged > 0 && self != "" {
		strmMu.Lock()
		n, err := buildSTRM(store, self, false)
		strmMu.Unlock()
		if err != nil {
			log.Printf("[巡检] 清理后重建 STRM 失败：%v", err)
		} else {
			log.Printf("[巡检] 清理后 STRM 已重建：%d 个", n)
		}
	}
	return st
}

// purgeShare 删除某分享的全部条目 + 追更登记，返回删除条目数。
// 严格按 (provider, shareID) 匹配，不会波及其他分享。
func purgeShare(store *Store, m ShareMeta) int {
	var ids []string
	for _, it := range store.List() {
		if it.Provider == m.Provider && it.ShareID == m.ShareID {
			ids = append(ids, it.ID)
		}
	}
	n := store.DeleteBatch(ids)
	if globalShares != nil {
		globalShares.Delete(m.Provider, m.ShareID)
	}
	return n
}

// shareGone 判断错误是否属于「分享本身没了」。
//
// 两类判断都做：先排除临时性错误（网络/限流/登录），再认失效关键词。
// 宁可漏判（多等一轮）也不误判（删错整部剧）。
func shareGone(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// ① 临时性错误：一律不算失效
	for _, k := range []string{
		"timeout", "timed out", "connection", "eof", "no such host", "tls handshake",
		"502", "503", "504", "429", "too many", "rate limit",
		"频率", "限流", "网络", "还没有导入", "凭证", "登录",
	} {
		if strings.Contains(s, k) {
			return false
		}
	}
	// ② 明确的失效文案
	//
	// 注意：**不含「没有找到视频文件」**。这条是「分享还在、但扫不出视频」，
	// 139 的 ISO 分享会命中（scan139Share 主动跳过 .iso），如果拿来判死，
	// 会把整部剧误删。宁可漏判，不可误判。
	//
	// 但「审核屏蔽」例外：它由 ShareStatus 元数据实锤（partial_violation +
	// violation_cnt>0），不是「扫不出视频」的猜测，且 139 的 ISO 分享元数据里
	// 没有这些字段，不会误伤（2026-10-20 美人余事故补漏）。
	for _, k := range []string{
		"分享已失效", "分享地址已失效", "分享不存在", "取消分享", "分享已过期",
		"分享内容不存在", "分享可能已失效", "链接不存在", "已被删除", "分享失效",
		"审核屏蔽",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// appendGoneLog 把清理记录追加到 /data/shares_gone.json（保留最近 200 条），
// 方便事后查"某部剧什么时候、为什么被清掉了"。
func appendGoneLog(m ShareMeta, eps int) {
	path := filepath.Join(filepath.Dir(*flagData), "shares_gone.json")
	var list []map[string]interface{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	list = append(list, map[string]interface{}{
		"time":     time.Now().Format("2006-01-02 15:04:05"),
		"provider": m.Provider,
		"share_id": m.ShareID,
		"show":     m.Show,
		"episodes": eps,
		"error":    m.LastError,
	})
	if len(list) > 200 {
		list = list[len(list)-200:]
	}
	if b, err := json.MarshalIndent(list, "", "  "); err == nil {
		_ = os.WriteFile(path, b, 0644)
	}
}

func recordHealth(st healthState) {
	healthLock.Lock()
	lastHealth = st
	healthLock.Unlock()
}

// apiShareHealth GET 查最近一轮结果，POST 立即跑一轮（同步返回明细）。
func apiShareHealth(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			healthLock.Lock()
			st := lastHealth
			healthLock.Unlock()
			writeJSON(w, map[string]interface{}{"ok": true, "state": st, "fail_limit": healthFailLimit})
		case http.MethodPost:
			st := healthCycle(store, self)
			writeJSON(w, map[string]interface{}{"ok": true, "state": st})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
