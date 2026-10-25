package main

// 批量导入：一次粘贴多条分享链接，逐条解析 → 列表预览（每行可单独改分类）→ 一键入库。
//
// 设计要点（踩过的坑都在这里）：
//  1. 解析与入库分两步：解析结果先给前端看（标题、文件数、失败原因），
//     用户逐条确认分类后再入库，避免"导完才发现选错分类"。
//  2. 解析结果缓存在内存里（按 provider|shareID），入库时直接取，
//     不再解析第二遍 —— 大合集解析一次要几秒，重复解析纯浪费。
//  3. 串行 + 间隔：同时打多个网盘接口既不礼貌也容易触发风控。
//  4. STRM 只在全部导完后重建一次，绝不逐条触发（N1 盒子就是被这个打死的）。

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// batchParseCache 解析结果缓存：key = provider|shareID。
// 只活 30 分钟，够用户"解析 → 调分类 → 导入"这段操作。
type batchParseCacheT struct {
	mu    sync.Mutex
	items map[string]batchParsed
}

type batchParsed struct {
	info ShareInfo
	at   time.Time
}

var batchParseCache = &batchParseCacheT{items: map[string]batchParsed{}}

func batchCacheKey(provider, shareID string) string {
	return strings.ToLower(provider) + "|" + shareID
}

func (c *batchParseCacheT) put(info ShareInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 顺手清过期项，避免长期运行堆积
	now := time.Now()
	for k, v := range c.items {
		if now.Sub(v.at) > 30*time.Minute {
			delete(c.items, k)
		}
	}
	c.items[batchCacheKey(info.Provider, info.ShareID)] = batchParsed{info: info, at: now}
}

func (c *batchParseCacheT) get(provider, shareID string) (ShareInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[batchCacheKey(provider, shareID)]
	if !ok || time.Since(v.at) > 30*time.Minute {
		return ShareInfo{}, false
	}
	return v.info, true
}

// ---------------------------------------------------------------------------
// 第一步：批量解析（不入库）
// ---------------------------------------------------------------------------

// apiShareBatchParse POST {links:[...]} → 逐条解析，返回摘要供前端预览与调分类。
func apiShareBatchParse(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Links []string `json:"links"`
			Pwd   string   `json:"pwd"` // 单独填的提取码，仅在只粘了一条链接时生效
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
			return
		}
		if len(req.Links) > 50 {
			req.Links = req.Links[:50] // 一次最多 50 条，再多请分批
		}

		type row struct {
			Link     string `json:"link"`
			OK       bool   `json:"ok"`
			Provider string `json:"provider,omitempty"`
			Title    string `json:"title,omitempty"`
			ShareID  string `json:"share_id,omitempty"`
			Passcode string `json:"passcode,omitempty"`
			Count    int    `json:"count"` // 视频文件数
			Exists   bool   `json:"exists,omitempty"`
			// Files 预览用：让用户在入库前能展开取消勾选（和单条导入一致）。
			Files []ShareFile `json:"files,omitempty"`
			// Pending 大目录分段扫还没扫完（豆包自家盘限速）。
			// Job 非空表示服务端已在后台续扫，前端轮询 /api/share/parse/status 即可，
			// 关掉页面也会继续扫完。
			Pending bool   `json:"pending,omitempty"`
			Scanned int    `json:"scanned,omitempty"`
			Job     string `json:"job,omitempty"`
			Remain  int    `json:"remain,omitempty"`
			Error   string `json:"error,omitempty"`
		}
		out := make([]row, 0, len(req.Links))

		// 只粘了一条时，允许用独立的「提取码」输入框（和单条导入的用法一致）。
		var singlePwd string
		if len(req.Links) == 1 {
			singlePwd = strings.TrimSpace(req.Pwd)
		}
		for _, raw := range req.Links {
			raw = normalizeBatchLink(raw)
			if raw == "" {
				continue
			}
			it := row{Link: raw}
			link := mergeSharePwd(raw, singlePwd)
			p := findProvider(link)
			if p == nil {
				it.Error = "不认识的链接（支持：" + strings.Join(providerList(), "、") + "）"
				out = append(out, it)
				continue
			}
			info, err := p.ListShareFiles(link)
			if err != nil {
				it.Error = err.Error()
				out = append(out, it)
				continue
			}
			n := 0
			for _, f := range info.Files {
				if !f.IsDir && isVideo(f.Name) {
					n++
				}
			}
			it.OK = true
			it.Provider = info.Provider
			it.Title = info.Title
			it.ShareID = info.ShareID
			it.Passcode = info.Passcode
			it.Count = n
			it.Files = info.Files
			it.Pending = info.Pending
			it.Scanned = info.Scanned
			if info.Pending && info.Resume != nil && info.ScanKey != "" {
				// 交给后台 goroutine 扫完，前端只轮询进度（关页面不影响）
				it.Job = info.ScanKey
				dbTaskStart(info.ScanKey, info.Title, info.Files, info.Resume)
				it.Remain = dbScanPending(info.ScanKey)
			}
			it.Exists = shareExists(store, info.Provider, info.ShareID)
			// 分段扫描第一段可能只扫到目录、还没扫到视频，这时不能判「没有视频」。
			if n == 0 && !info.Pending {
				it.OK = false
				it.Error = "分享里没有视频文件"
			}
			batchParseCache.put(info)
			out = append(out, it)
			time.Sleep(300 * time.Millisecond) // 串行限速，别把网盘接口打急
		}
		writeJSON(w, map[string]interface{}{"ok": true, "rows": out})
	}
}

// ---------------------------------------------------------------------------
// 第二步：按前端确认的分类逐条入库
// ---------------------------------------------------------------------------

// apiShareBatchAdd POST {items:[{provider,share_id,category}]} → 从缓存取解析结果入库。
func apiShareBatchAdd(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Items []struct {
				Provider string `json:"provider"`
				ShareID  string `json:"share_id"`
				Category string `json:"category"` // 每行独立分类（可两级：父/子）
				Title    string `json:"title"`    // 每行独立的剧名（空 = 用分享自带标题）
				// Files 可选：只收这些 fileID（用户在预览里取消了部分勾选）；
				// 空 = 全收（默认）。
				Files []string `json:"files"`
			} `json:"items"`
			AutoRefresh *bool `json:"auto_refresh"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Items) == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "没有要导入的分享"})
			return
		}
		ar := true
		if req.AutoRefresh != nil {
			ar = *req.AutoRefresh
		}

		type res struct {
			Provider string `json:"provider"`
			ShareID  string `json:"share_id"`
			Title    string `json:"title,omitempty"`
			OK       bool   `json:"ok"`
			Added    int    `json:"added"`
			Skipped  bool   `json:"skipped,omitempty"`
			Error    string `json:"error,omitempty"`
		}
		out := make([]res, 0, len(req.Items))
		totalAdded := 0
		imported := 0

		for i, item := range req.Items {
			rr := res{Provider: item.Provider, ShareID: item.ShareID}
			info, ok := batchParseCache.get(item.Provider, item.ShareID)
			if !ok {
				rr.Error = "解析结果已过期，请重新解析"
				out = append(out, rr)
				continue
			}
			// 剧名以用户改过的为准；没改就用分享自带标题。
			showName := strings.TrimSpace(item.Title)
			if showName == "" {
				showName = info.Title
			}
			rr.Title = showName
			if shareExists(store, info.Provider, info.ShareID) {
				rr.OK, rr.Skipped = true, true
				out = append(out, rr)
				continue
			}
			category, catErr := normalizeCategory(item.Category)
			if catErr != nil {
				rr.Error = catErr.Error()
				out = append(out, rr)
				continue
			}
			showFinal := safeFilename(showName)
			if showFinal == "" {
				showFinal = "未命名"
			}
			// 勾选白名单（预览里取消过的文件不入库）
			var keep map[string]bool
			if len(item.Files) > 0 {
				keep = make(map[string]bool, len(item.Files))
				for _, id := range item.Files {
					keep[id] = true
				}
			}
			// 同 apiShareAdd：剥掉分享自带的容器目录（用户给的剧名已代表这一层）
			var bp []string
			for _, f := range info.Files {
				if !f.IsDir && isVideo(f.Name) {
					bp = append(bp, f.Path)
				}
			}
			shareRoot := commonShareRoot(bp)
			// 剧名留空 → 回退到分享顶层名（与单个导入一致）
			if strings.TrimSpace(showFinal) == "未命名" && shareRoot != "" {
				showFinal = safeFilename(shareRoot)
			}
			batch := make([]Item, 0, len(info.Files))
			for _, f := range info.Files {
				if f.ID == "" || f.IsDir || !isVideo(f.Name) {
					continue
				}
				if keep != nil && !keep[f.ID] {
					continue
				}
				dir := buildItemDir(info.Provider, category, showFinal, stripShareRoot(f.Path, shareRoot))
				ext := strings.ToLower(filepath.Ext(f.Name))
				if ext == "" {
					ext = strings.ToLower(filepath.Ext(f.Path))
				}
				title := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
				if title == "" {
					title = strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
				}
				batch = append(batch, Item{
					Title: title, Dir: dir, Category: category, Show: showFinal,
					Provider: info.Provider, ShareID: info.ShareID, Passcode: info.Passcode,
					FileID: f.ID, FidToken: f.FidToken, ShareNumID: f.ShareNumID,
					Ext: ext, Size: f.Size,
				})
			}
			if len(batch) == 0 {
				rr.Error = "分享里没有视频文件"
				out = append(out, rr)
				continue
			}
			added := store.AddBatch(batch)
			totalAdded += added
			imported++
			rr.OK, rr.Added = true, added
			if globalShares != nil {
				globalShares.Put(ShareMeta{
					Provider: info.Provider, ShareID: info.ShareID, Passcode: info.Passcode,
					Category: category, Show: showFinal, AutoRefresh: ar,
				})
			}
			out = append(out, rr)
			if i < len(req.Items)-1 {
				time.Sleep(400 * time.Millisecond) // 入库是小操作，给写盘留点喘息
			}
		}

		writeJSON(w, map[string]interface{}{
			"ok": true, "added": totalAdded, "imported": imported, "rows": out,
		})
		if imported > 0 {
			// 全部导完只重建一次 STRM —— 逐条触发会把小机器写死（见备忘录 §23.71）。
			rebuildSTRMAsync(store, self, "批量导入分享")
		}
	}
}

// normalizeBatchLink 把一行"片名 + 链接（访问码）"裁成纯链接。
// 从 http 开始截取，前面的中文标题、编号全部丢弃；链接尾部的
// 「（访问码：abcd）」保留 —— 天翼的解析器要靠它提取提取码。
func normalizeBatchLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "http"); i > 0 {
		raw = raw[i:]
	}
	return strings.TrimSpace(raw)
}

// shareExists 某个分享是否已经导入过（按 provider + 分享标识判断）。
func shareExists(store *Store, provider, shareID string) bool {
	if store == nil || shareID == "" {
		return false
	}
	for _, s := range store.Shares() {
		if strings.EqualFold(s.Provider, provider) && s.ShareID == shareID {
			return true
		}
	}
	return false
}
