package main

// ---------------------------------------------------------------------------
// 事后改分类：剧导入后发现分类分错，在这里补救。
//
// 一个分享 = 一部剧 / 一部电影。改分类 = 把该分享全部条目的 Category 与
// 虚拟目录（Dir）里的分类段换掉，追更登记（ShareMeta.Category）同步更新
// —— 否则以后追更出来的新剧集还按旧分类建目录，又得再改一次。
//
// .strm 不用操心：改完调一次增量同步，旧路径变成孤儿被删、新路径落地，
// 媒体库只重新刮削这一部，别的剧原样不动（见 strm.go 的差量逻辑）。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// itemDirWithCategory 重算条目的虚拟目录：把「网盘/[旧分类/]剧名/分享内子目录」
// 里的分类段换成 newCat，其余原样保留。
//
// 为什么不直接调 buildItemDir：它要「分享内文件路径」，而 Item 只存了成品
// 目录。这里按「剧名段」定位后缀（剧名及以后的分享内子目录），前面拼上网盘
// 与新分类即可。老条目没有 Category 字段时，分类段按目录结构猜（Shares()
// 也是这么猜的），找到剧名段后它前面的段自然被替换掉。
//
// 【2026-10-20 两级分类】旧分类可能是「父/子」两段，剥前缀要按段数全部剥掉；
// 新分类同样按段拼（网盘/电视剧/国产剧/剧名）。一级分类的老逻辑不变。
func itemDirWithCategory(it *Item, newCat string) string {
	segs := strings.Split(strings.Trim(it.Dir, "/"), "/")
	// ① 去掉网盘顶层（"百度" / "quark" 两种写法都兼容）
	if len(segs) > 0 && (segs[0] == providerCN(it.Provider) || segs[0] == it.Provider) {
		segs = segs[1:]
	}
	// ② 去掉旧分类段（与 Item.Category 对应，按段匹配前缀全部剥掉；
	// 老条目 Category 为空、或目录与字段对不上时，跳过让 ③ 按剧名兜底）
	if it.Category != "" && len(segs) > 0 {
		oldSegs := strings.Split(it.Category, "/")
		if len(segs) >= len(oldSegs) {
			match := true
			for i, os := range oldSegs {
				if segs[i] != safeFilename(strings.TrimSpace(os)) {
					match = false
					break
				}
			}
			if match {
				segs = segs[len(oldSegs):]
			}
		}
	}
	// ③ 从剧名段开始取后缀：分享内的子目录结构原样保留
	show := safeFilename(stripScrape(it.Show))
	if show != "" {
		for i, s := range segs {
			if s == show {
				segs = segs[i:]
				break
			}
		}
	}
	suffix := strings.Join(segs, "/")
	if suffix == "" {
		suffix = show // 目录与字段对不上时保底：剧名这层不能丢
	}
	dir := providerCN(it.Provider)
	// 与 buildItemDir 同一规矩：先判空再净化，空分类不套「未命名」；
	// 两级分类按段拼（电视剧/国产剧 → 两层目录）
	if cat := strings.TrimSpace(newCat); cat != "" {
		for _, seg := range strings.Split(cat, "/") {
			if seg = strings.TrimSpace(seg); seg != "" {
				dir += "/" + safeFilename(seg)
			}
		}
	}
	return dir + "/" + suffix
}

// apiShareCategory 事后改分类：POST /api/shares/category
// body: {"provider":"baidu","share_id":"xxx","category":"欧美剧"}
// category 传空串 = 不分类（直接放剧名）。
func apiShareCategory(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			ShareID  string `json:"share_id"`
			Category string `json:"category"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" || req.ShareID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		// 与导入同一规矩：规范化（支持两级「父/子」），空分类不套「未命名」
		newCat, catErr := normalizeCategory(req.Category)
		if catErr != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": catErr.Error()})
			return
		}

		// 整批换目录：一次拿锁、一次重建索引、一次落盘。
		// （老写法逐条 Update：每条都触发一次全量索引重建，整季几百条时在盒子上很肉，
		//  见 Store.UpdateBatch 注释。）
		show, passcode := "", ""
		moved := store.UpdateBatch(func(x *Item) bool {
			if x.Provider != req.Provider || x.ShareID != req.ShareID {
				return false
			}
			if show == "" {
				show = x.Show
			}
			if passcode == "" {
				passcode = x.Passcode
			}
			return true
		}, func(x *Item) {
			x.Category = newCat
			x.Dir = itemDirWithCategory(x, newCat)
		})
		if moved == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "没有找到这个分享的条目（可能已全部删除）"})
			return
		}

		// 追更登记同步换分类：否则以后追更出的新剧集还按旧分类建目录
		if globalShares != nil {
			m := globalShares.Get(req.Provider, req.ShareID)
			if m == nil {
				// 没登记的老分享：按条目聚合补一份（字段带全，将来重新导入整体覆盖）
				m = &ShareMeta{Provider: req.Provider, ShareID: req.ShareID, Passcode: passcode}
			}
			m.Category = newCat
			if show != "" {
				m.Show = show
			}
			globalShares.Put(*m)
		}

		log.Printf("[改分类] %s/%s → %q：%d 个条目换了目录", req.Provider, show, newCat, moved)
		writeJSON(w, map[string]interface{}{"ok": true, "moved": moved, "category": newCat, "show": show})
		// 增量同步：旧路径成孤儿被删、新路径落地，媒体库只重新刮削这一部
		rebuildSTRMAsync(store, self, "改分类")
	}
}

// ---------------------------------------------------------------------------
// 事后改剧名：订阅号自动收录的分享标题带 emoji（Emby 认不了）或名字太烂，
// 在这里整条分享统一改名。改法与改分类同一套：UpdateBatch 整批换
// Show 与 Dir 的剧名段，追更登记同步更新，STRM 走增量差量重建。
// ---------------------------------------------------------------------------

// itemDirWithShow 重算条目的虚拟目录：把剧名段换成 newShow，其余原样保留。
func itemDirWithShow(it *Item, newShow string) string {
	segs := strings.Split(strings.Trim(it.Dir, "/"), "/")
	// ① 去掉网盘顶层（与 itemDirWithCategory 同一兼容规则），原样记住，
	// 结尾拼回去 —— 改名不该动顶层，「天翼」「baidu」各保持各的写法。
	top := ""
	if len(segs) > 0 && (segs[0] == providerCN(it.Provider) || segs[0] == it.Provider) {
		top = segs[0]
		segs = segs[1:]
	}
	// ② 定位旧剧名段（老条目目录与字段对不上时按旧分类段数跳过兜底）
	show := safeFilename(stripScrape(it.Show))
	found := -1
	for i, s := range segs {
		if s == show {
			found = i
			break
		}
	}
	if found < 0 && it.Category != "" {
		oldSegs := strings.Split(it.Category, "/")
		if len(segs) > len(oldSegs) {
			found = len(oldSegs)
		}
	}
	if found < 0 {
		found = 0
	}
	if len(segs) == 0 {
		segs = []string{safeFilename(newShow)} // 目录只有网盘顶层：补上剧名段
	} else {
		segs[found] = safeFilename(newShow)
	}
	out := strings.Join(segs, "/")
	if top != "" {
		out = top + "/" + out
	}
	return out
}

// apiShareRename 事后改剧名：POST /api/shares/rename
// body: {"provider":"tianyi","share_id":"123","show":"豆瓣电影 Top250"}
func apiShareRename(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
			ShareID  string `json:"share_id"`
			Show     string `json:"show"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" || req.ShareID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		newShow := cleanShareTitle(strings.TrimSpace(req.Show))
		if rr := []rune(newShow); len(rr) > 60 {
			newShow = string(rr[:60])
		}
		if newShow == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "剧名不能为空（符号会被自动清掉）"})
			return
		}

		show, passcode, cat := "", "", ""
		renamed := store.UpdateBatch(func(x *Item) bool {
			if x.Provider != req.Provider || x.ShareID != req.ShareID {
				return false
			}
			if show == "" {
				show = x.Show
			}
			if passcode == "" {
				passcode = x.Passcode
			}
			if cat == "" {
				cat = x.Category
			}
			return true
		}, func(x *Item) {
			// 【顺序要紧】先按旧 Show 算新目录，再把 Show 换成新名 ——
			// 反了的话 itemDirWithShow 找不到旧剧名段，会误替换分类段。
			x.Dir = itemDirWithShow(x, newShow)
			x.Show = newShow
		})
		if renamed == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "没有找到这个分享的条目（可能已全部删除）"})
			return
		}

		// 追更登记同步：否则追更出的新剧集还按旧剧名落目录
		if globalShares != nil {
			m := globalShares.Get(req.Provider, req.ShareID)
			if m == nil {
				m = &ShareMeta{Provider: req.Provider, ShareID: req.ShareID, Passcode: passcode}
			}
			m.Show = newShow
			if m.Category == "" && cat != "" {
				m.Category = cat
			}
			globalShares.Put(*m)
		}

		log.Printf("[改剧名] %s/%s → %q：%d 个条目跟改", req.Provider, show, newShow, renamed)
		writeJSON(w, map[string]interface{}{"ok": true, "renamed": renamed, "show": newShow})
		rebuildSTRMAsync(store, self, "改剧名")
	}
}
