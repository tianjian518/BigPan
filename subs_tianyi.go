package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 天翼「订阅号」收录
//
// 用户在天翼 App 里订阅资源号（UP 主），本模块按 upUserId 定时拉取其新发布
// 的分享，自动收进虚拟盘。实测（2026-10-20）：GetUpResourceShare 响应里
// 直接带数字 shareId，所以收录的分享不需要再换 shareId，也不受
// getShareInfoByCode 不返回 shareId 的影响。
//
// 水位（watermark）：首次订阅把水位设成「当前时间」，只收之后发布的分享，
// 避免一上来就把 UP 主的历史 1600 条全导进来。历史分享可在面板里手动收录。
// ---------------------------------------------------------------------------

// tianyiSubs 全局订阅号登记（main 里初始化，供 API/定时器共用）。
var tianyiSubs *tianyiSubStore

// tianyiSubEntry 一条订阅登记。
type tianyiSubEntry struct {
	UserID     string    `json:"user_id"`
	Name       string    `json:"name"`
	Category   string    `json:"category,omitempty"`    // 收录到哪个分类（可空）
	AutoImport bool      `json:"auto_import"`           // 新分享自动收录
	Watermark  string    `json:"watermark,omitempty"`   // 只收 ShareDate 晚于它的分享
	AddedAt    time.Time `json:"added_at"`
	LastSync   time.Time `json:"last_sync,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Imported   int       `json:"imported"` // 累计收录的分享数
	// Done 历史导入的断点：shareId → 已处理。一键导入历史时跳过这些，
	// 中途叫停/断电后再次点击能从断点接着走，不会重复扫描已收过的分享。
	Done map[string]bool `json:"done,omitempty"`
}

// tianyiSubStore 订阅登记持久化（JSON 文件，独立于 items.json）。
type tianyiSubStore struct {
	mu   sync.Mutex
	path string
	list []tianyiSubEntry
}

func newTianyiSubStore(path string) *tianyiSubStore {
	s := &tianyiSubStore{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return s
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.list)
	}
	return s
}

func (s *tianyiSubStore) saveLocked() {
	b, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, b, 0644)
}

func (s *tianyiSubStore) List() []tianyiSubEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tianyiSubEntry, len(s.list))
	copy(out, s.list)
	return out
}

func (s *tianyiSubStore) Get(userID string) *tianyiSubEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].UserID == userID {
			e := s.list[i]
			return &e
		}
	}
	return nil
}

func (s *tianyiSubStore) Put(e tianyiSubEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].UserID == e.UserID {
			s.list[i] = e
			s.saveLocked()
			return
		}
	}
	s.list = append(s.list, e)
	s.saveLocked()
}

func (s *tianyiSubStore) Delete(userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].UserID == userID {
			s.list = append(s.list[:i], s.list[i+1:]...)
			s.saveLocked()
			return true
		}
	}
	return false
}

// tianyiSubUUIDRe 订阅号 userId：32 位十六进制（天翼资源号 ID 就是这种形态）。
var tianyiSubUUIDRe = regexp.MustCompile(`([0-9a-f]{32})`)

// parseTianyiSubscribe 从「订阅号链接 / 裸 userId」里提取 userId。
// 支持：
//   https://content.21cn.com/h5/subscrip/index.html#/pages/own-home/index?uuid=xxxx
//   https://h5.cloud.189.cn/...?userId=xxxx
//   直接粘贴 32 位 userId
func parseTianyiSubscribe(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请粘贴订阅号链接或订阅号 ID")
	}
	low := strings.ToLower(raw)
	for _, key := range []string{"uuid=", "userid=", "upuserid="} {
		if i := strings.Index(low, key); i >= 0 {
			v := raw[i+len(key):]
			if j := strings.IndexAny(v, "&#?/ "); j >= 0 {
				v = v[:j]
			}
			v = strings.TrimSpace(v)
			if tianyiSubUUIDRe.MatchString(strings.ToLower(v)) {
				return tianyiSubUUIDRe.FindString(strings.ToLower(v)), nil
			}
			if v != "" {
				return v, nil
			}
		}
	}
	if m := tianyiSubUUIDRe.FindString(low); m != "" {
		return m, nil
	}
	return "", fmt.Errorf("没认出订阅号 ID，请粘贴天翼 App 里「订阅号 → 分享主页」的链接")
}

// tianyiSubNormalizeWM 统一水位格式（空 = 最早）。
func tianyiSubNormalizeWM(wm string) string {
	wm = strings.TrimSpace(wm)
	if wm == "" {
		return ""
	}
	return wm
}

// tianyiSubImportOne 收录订阅号里的一条分享（扫描目录树 → 写条目）。
// 返回写入的条目数；同一分享重复收录会按路径覆盖，不会产生重复。
func tianyiSubImportOne(store *Store, self string, e tianyiSubEntry, s tianyiSubShare) (int, error) {
	cred := tianyiCredOf(globalAccounts)
	title, files, err := tianyiSubWalk(cred, s)
	if err != nil {
		return 0, err
	}
	category, err := normalizeCategory(e.Category)
	if err != nil {
		return 0, err
	}
	showRaw := strings.TrimSpace(title)
	if showRaw == "" {
		showRaw = s.Name
	}
	showFinal := safeFilename(stripScrape(showRaw))
	if showFinal == "" {
		showFinal = "未命名"
	}
	numID := fmt.Sprintf("%d", s.ShareId)
	batch := make([]Item, 0, len(files))
	for _, f := range files {
		dir := buildItemDir("tianyi", category, showRaw, f.Path)
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext == "" {
			ext = strings.ToLower(filepath.Ext(f.Path))
		}
		t := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		if t == "" {
			t = strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		}
		batch = append(batch, Item{
			Title:      t,
			Dir:        dir,
			Category:   category,
			Show:       showFinal,
			Provider:   "tianyi",
			ShareID:    numID, // 订阅号分享没有 shareCode，用数字 shareId 标识
			Passcode:   "",
			FileID:     f.ID,
			ShareNumID: numID,
			SubUserID:  e.UserID,
			Ext:        ext,
			Size:       f.Size,
		})
	}
	if len(batch) == 0 {
		return 0, fmt.Errorf("分享里没有视频文件")
	}
	n := store.AddBatch(batch)
	rebuildSTRMAsync(store, self, "订阅号收录")
	return n, nil
}

// tianyiSubFetchNew 拉取水位之后的新分享（按发布时间从旧到新返回）。
// 只翻前 maxPages 页（默认 10 页 × 50 条 = 500 条），防止一次性拉爆。
func tianyiSubFetchNew(cred *tianyiCred, userID, watermark string, maxPages int) ([]tianyiSubShare, error) {
	if maxPages <= 0 {
		maxPages = 10
	}
	var all []tianyiSubShare
	for page := 1; page <= maxPages; page++ {
		list, _, err := tianyiUpShares(cred, userID, int64(page), 50)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			break
		}
		stop := false
		for _, s := range list {
			if watermark != "" && s.ShareDate <= watermark {
				stop = true
				break
			}
			all = append(all, s)
		}
		if stop || len(list) < 50 {
			break
		}
	}
	// 从旧到新收录，水位推进才稳定
	sort.SliceStable(all, func(i, j int) bool { return all[i].ShareDate < all[j].ShareDate })
	return all, nil
}

// tianyiSubSyncOne 同步一个订阅号：拉新分享、按开关自动收录、推进水位。
// 返回新收录的分享数。
func tianyiSubSyncOne(store *Store, subs *tianyiSubStore, self, userID string) (int, error) {
	e := subs.Get(userID)
	if e == nil {
		return 0, fmt.Errorf("没有这个订阅号")
	}
	cred := tianyiCredOf(globalAccounts)
	newShares, err := tianyiSubFetchNew(cred, userID, tianyiSubNormalizeWM(e.Watermark), 10)
	if err != nil {
		e.LastError = err.Error()
		subs.Put(*e)
		return 0, err
	}
	imported := 0
	maxDate := e.Watermark
	for _, s := range newShares {
		if e.AutoImport {
			// self 传空 = 不逐条重建 STRM，整轮结束后重建一次（避免 N 次全量重建）
			if _, err := tianyiSubImportOne(store, "", *e, s); err == nil {
				imported++
			}
		}
		if s.ShareDate > maxDate {
			maxDate = s.ShareDate
		}
	}
	if imported > 0 {
		rebuildSTRMAsync(store, self, "订阅号同步")
	}
	e.Watermark = maxDate
	e.Imported += imported
	e.LastSync = time.Now()
	e.LastError = ""
	subs.Put(*e)
	return imported, nil
}

// startTianyiSubRefresher 订阅号定时器：每隔 every 同步所有「自动收录」的订阅号。
func startTianyiSubRefresher(store *Store, subs *tianyiSubStore, self string, every time.Duration) {
	if subs == nil || every <= 0 {
		return
	}
	go func() {
		time.Sleep(2 * time.Minute) // 开机先让主服务稳定
		for {
			for _, e := range subs.List() {
				if !e.AutoImport {
					continue
				}
				_, _ = tianyiSubSyncOne(store, subs, self, e.UserID)
				time.Sleep(3 * time.Second) // 多个订阅号之间留点间隔，别把天翼打急
			}
			time.Sleep(every)
		}
	}()
}

// ---------------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------------

// apiTianyiSubs GET 列表 / POST 新增。
func apiTianyiSubs(subs *tianyiSubStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{"ok": true, "subs": subs.List()})
		case http.MethodPost:
			var req struct {
				Link       string `json:"link"` // 订阅号链接或 userId
				Category   string `json:"category"`
				AutoImport *bool  `json:"auto_import"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "参数不合法"})
				return
			}
			userID, err := parseTianyiSubscribe(req.Link)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			name, err := tianyiSubUser(tianyiCredOf(globalAccounts), userID)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			category, err := normalizeCategory(req.Category)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			auto := true
			if req.AutoImport != nil {
				auto = *req.AutoImport
			}
			old := subs.Get(userID)
			e := tianyiSubEntry{
				UserID: userID, Name: name, Category: category,
				AutoImport: auto,
				Watermark:  time.Now().Format(time.DateTime), // 只收以后发布的
				AddedAt:    time.Now(),
			}
			if old != nil {
				e.AddedAt = old.AddedAt
				e.Imported = old.Imported
				e.Watermark = old.Watermark // 已订阅过：保留原水位，不清空
			}
			subs.Put(e)
			writeJSON(w, map[string]interface{}{"ok": true, "sub": e})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// apiTianyiSubItem DELETE 删除订阅 / PATCH 改分类或开关。
func apiTianyiSubItem(subs *tianyiSubStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := strings.TrimPrefix(r.URL.Path, "/api/tianyi/subs/")
		userID = strings.Trim(userID, "/")
		if userID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "缺少订阅号 ID"})
			return
		}
		switch r.Method {
		case http.MethodDelete:
			ok := subs.Delete(userID)
			writeJSON(w, map[string]interface{}{"ok": ok})
		case http.MethodPatch, http.MethodPost:
			e := subs.Get(userID)
			if e == nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "没有这个订阅号"})
				return
			}
			var req struct {
				Category   *string `json:"category"`
				AutoImport *bool   `json:"auto_import"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "参数不合法"})
				return
			}
			if req.Category != nil {
				c, err := normalizeCategory(*req.Category)
				if err != nil {
					writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
					return
				}
				e.Category = c
			}
			if req.AutoImport != nil {
				e.AutoImport = *req.AutoImport
			}
			subs.Put(*e)
			writeJSON(w, map[string]interface{}{"ok": true, "sub": e})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// apiTianyiSubShares GET 列出订阅号下的分享（分页浏览，供手动收录）。
// ?user_id=xxx&page=1&size=50&only_new=1
func apiTianyiSubShares(store *Store, subs *tianyiSubStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "缺少 user_id"})
			return
		}
		page := 1
		size := 50
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		fmt.Sscanf(r.URL.Query().Get("size"), "%d", &size)
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 200 {
			size = 50
		}
		cred := tianyiCredOf(globalAccounts)
		list, total, err := tianyiUpShares(cred, userID, int64(page), int64(size))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		// 已收录的 shareId 集合（条目标了 SubUserID 的）
		imported := map[string]int{}
		for _, it := range store.List() {
			if it.SubUserID == userID && it.ShareNumID != "" {
				imported[it.ShareNumID]++
			}
		}
		type row struct {
			tianyiSubShare
			Imported int  `json:"imported"`
			IsNew    bool `json:"is_new"`
		}
		e := subs.Get(userID)
		wm := ""
		if e != nil {
			wm = e.Watermark
		}
		out := make([]row, 0, len(list))
		for _, s := range list {
			out = append(out, row{
				tianyiSubShare: s,
				Imported:       imported[fmt.Sprintf("%d", s.ShareId)],
				IsNew:          wm != "" && s.ShareDate > wm,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "total": total, "page": page, "size": size, "shares": out})
	}
}

// apiTianyiSubImport POST 手动收录一条分享：{user_id, share_id}
func apiTianyiSubImport(store *Store, subs *tianyiSubStore, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			UserID  string `json:"user_id"`
			ShareID int64  `json:"share_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" || req.ShareID == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数不合法"})
			return
		}
		e := subs.Get(req.UserID)
		if e == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "没有这个订阅号"})
			return
		}
		s, err := tianyiSubShareByID(tianyiCredOf(globalAccounts), req.UserID, req.ShareID)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		n, err := tianyiSubImportOne(store, self, *e, *s)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "added": n, "title": s.Name})
	}
}

// tianyiSubShareByID 在订阅号分享列表里按 shareId 找一条（最多翻 40 页）。
func tianyiSubShareByID(cred *tianyiCred, userID string, shareID int64) (*tianyiSubShare, error) {
	for page := 1; page <= 40; page++ {
		list, total, err := tianyiUpShares(cred, userID, int64(page), 50)
		if err != nil {
			return nil, err
		}
		for i := range list {
			if list[i].ShareId == shareID {
				return &list[i], nil
			}
		}
		if len(list) < 50 || int64(page*50) >= total {
			break
		}
	}
	return nil, fmt.Errorf("订阅号里没找到这个分享（shareId=%d）", shareID)
}

// apiTianyiSubSync POST 立即同步：{user_id}（空 = 同步全部自动收录的）
func apiTianyiSubSync(store *Store, subs *tianyiSubStore, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type result struct {
			UserID   string `json:"user_id"`
			Imported int    `json:"imported"`
			Error    string `json:"error,omitempty"`
		}
		var out []result
		if req.UserID != "" {
			n, err := tianyiSubSyncOne(store, subs, self, req.UserID)
			rr := result{UserID: req.UserID, Imported: n}
			if err != nil {
				rr.Error = err.Error()
			}
			out = append(out, rr)
		} else {
			for _, e := range subs.List() {
				n, err := tianyiSubSyncOne(store, subs, self, e.UserID)
				rr := result{UserID: e.UserID, Imported: n}
				if err != nil {
					rr.Error = err.Error()
				}
				out = append(out, rr)
			}
		}
		writeJSON(w, map[string]interface{}{"ok": true, "results": out})
	}
}

// ---------------------------------------------------------------------------
// 历史导入（一键补收旧分享）
//
// 为什么不能「一次全导」：订阅号动辄 1600+ 条分享，一条分享几十上百集，
// 全导就是几万条条目 + 几万次目录请求，盒子会顶不住。参考项目能一键导入
// 是因为它做的是「挂载视图」，浏览时才扫，不落地；我们是落地成条目，必须限速。
//
// 机制：
//   · 后台队列，HTTP 立即返回，前端轮询进度；
//   · 每条分享之间 sleep（默认 1.5s），并复用订阅号的面板分类；
//   · 可设本次最多处理多少条，0 = 全部；
//   · 断点续传：处理过的 shareId 记在 entry.Done，下次点击自动跳过；
//   · 每 25 条整库 STRM 重建一次，避免逐条重建；
//   · 全局单任务，随时可停；条目总数超过 30000 自动收手。
// ---------------------------------------------------------------------------

type tianyiBackfill struct {
	mu       sync.Mutex
	Running  bool
	UserID   string
	Total    int
	Done     int
	Imported int
	Added    int
	Failed   int
	Current  string
	Error    string
	Started  time.Time
	stop     bool
}

var tianyiBackfillJob = &tianyiBackfill{}

func (j *tianyiBackfill) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]interface{}{
		"running": j.Running, "user_id": j.UserID, "total": j.Total,
		"done": j.Done, "imported": j.Imported, "added": j.Added,
		"failed": j.Failed, "current": j.Current, "error": j.Error,
		"started": j.Started,
	}
}

func (j *tianyiBackfill) stopped() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stop || !j.Running
}

func (j *tianyiBackfill) set(fn func(*tianyiBackfill)) {
	j.mu.Lock()
	fn(j)
	j.mu.Unlock()
}

// tianyiBackfillStart 启动历史导入。maxShares<=0 表示不限条数。
func tianyiBackfillStart(store *Store, subs *tianyiSubStore, self, userID string, maxShares int, gap time.Duration) error {
	e := subs.Get(userID)
	if e == nil {
		return fmt.Errorf("没有这个订阅号")
	}
	j := tianyiBackfillJob
	j.mu.Lock()
	if j.Running {
		uid := j.UserID
		j.mu.Unlock()
		return fmt.Errorf("已有导入任务在跑（%s），先等它跑完或点停止", uid)
	}
	j.Running, j.UserID, j.stop = true, userID, false
	j.Total, j.Done, j.Imported, j.Added, j.Failed = 0, 0, 0, 0, 0
	j.Current, j.Error, j.Started = "", "", time.Now()
	j.mu.Unlock()

	if gap <= 0 {
		gap = 1000 * time.Millisecond
	}
	go tianyiBackfillRun(store, subs, self, userID, maxShares, gap)
	return nil
}

func tianyiBackfillRun(store *Store, subs *tianyiSubStore, self, userID string, maxShares int, gap time.Duration) {
	j := tianyiBackfillJob
	defer j.set(func(x *tianyiBackfill) { x.Running = false; x.Current = "" })

	e := subs.Get(userID)
	if e == nil {
		j.set(func(x *tianyiBackfill) { x.Error = "订阅号已被删除" })
		return
	}
	if e.Done == nil {
		e.Done = map[string]bool{}
	}
	// 断点预热：库里已有的（此前手动收录/定时收录过的）直接算已处理
	for _, it := range store.List() {
		if it.SubUserID == userID && it.ShareNumID != "" {
			e.Done[it.ShareNumID] = true
		}
	}

	cred := tianyiCredOf(globalAccounts)

	// 拉全量分享列表（按发布时间倒序，先收新的）
	var all []tianyiSubShare
	for page := 1; page <= 200; page++ {
		list, total, err := tianyiUpShares(cred, userID, int64(page), 50)
		if err != nil {
			j.set(func(x *tianyiBackfill) { x.Error = "拉取分享列表失败：" + err.Error() })
			subs.Put(*e)
			return
		}
		all = append(all, list...)
		j.set(func(x *tianyiBackfill) { x.Total = len(all) })
		if len(list) < 50 || int64(page*50) >= total {
			break
		}
	}
	if len(all) == 0 {
		j.set(func(x *tianyiBackfill) { x.Error = "这个订阅号没有分享" })
		return
	}

	limit := maxShares
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}

	processed, imported, added, failed := 0, 0, 0, 0
	sinceRebuild := 0
	for _, s := range all {
		if processed >= limit {
			break
		}
		if j.stopped() {
			break
		}
		key := fmt.Sprintf("%d", s.ShareId)
		if e.Done[key] {
			continue // 已处理过，直接跳过（不计入 limit）
		}

		// 极端兜底：20 万条只防内存爆（N1 共 2GB），正常订阅号到不了
		if len(store.List()) > 200000 {
			j.set(func(x *tianyiBackfill) { x.Error = "条目已超过 20 万，已自动停止" })
			break
		}

		j.set(func(x *tianyiBackfill) { x.Current = s.Name })
		n, err := tianyiSubImportOne(store, "", *e, s) // self=""：不逐条重建 STRM
		processed++
		if err != nil {
			failed++
			// 「没有视频文件」是永久结果，标记已处理避免反复扫；网络类错误留待重试
			if strings.Contains(err.Error(), "没有视频文件") {
				e.Done[key] = true
			}
		} else {
			imported++
			added += n
			sinceRebuild++
			e.Imported++
			e.Done[key] = true
		}
		j.set(func(x *tianyiBackfill) {
			x.Done, x.Imported, x.Added, x.Failed = processed, imported, added, failed
		})

		// 每 10 条落一次断点，减少小文件写次数
		if processed%10 == 0 {
			subs.Put(*e)
		}
		// 每 25 条重建一次 STRM（整库一次，别逐条）
		if sinceRebuild >= 25 {
			if self != "" {
				rebuildSTRMAsync(store, self, "订阅号历史导入")
			}
			sinceRebuild = 0
		}
		time.Sleep(gap)
	}

	subs.Put(*e)
	if self != "" && imported > 0 {
		rebuildSTRMAsync(store, self, "订阅号历史导入")
	}
	j.set(func(x *tianyiBackfill) {
		x.Done, x.Imported, x.Added, x.Failed = processed, imported, added, failed
	})
}

// apiTianyiSubBackfill GET 进度 / POST 启动 / DELETE 停止。
func apiTianyiSubBackfill(store *Store, subs *tianyiSubStore, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{"ok": true, "job": tianyiBackfillJob.snapshot()})
		case http.MethodDelete:
			tianyiBackfillJob.set(func(x *tianyiBackfill) { x.stop = true })
			writeJSON(w, map[string]interface{}{"ok": true})
		case http.MethodPost:
			var req struct {
				UserID    string `json:"user_id"`
				MaxShares int    `json:"max_shares"` // 0 = 全部
				GapMS     int    `json:"gap_ms"`     // 每条之间间隔，默认 1500
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "参数不合法"})
				return
			}
			gap := time.Duration(req.GapMS) * time.Millisecond
			if err := tianyiBackfillStart(store, subs, self, req.UserID, req.MaxShares, gap); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{"ok": true, "job": tianyiBackfillJob.snapshot()})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
