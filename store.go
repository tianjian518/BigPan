package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Item 是一条放映条目（虚拟盘里的一部片）。
//
// 统一来源模型：网盘 + 分享标识 + 文件 ID。
// 播放时由对应的 PanProvider 实时换取直链 —— 文件始终在别人网盘里，
// 本地不存副本、网盘不占空间。
type Item struct {
	ID    string `json:"id"`
	Title string `json:"title"`

	// 分享来源（虚拟盘）
	Provider string `json:"provider,omitempty"` // quark / 115
	// Dir 是虚拟盘内的目录（不含文件名），如 "夸克/国产剧/新白娘子传奇/Season 1"。
	// 顶层固定为网盘中文名，其后依次是「分类（可选）」「剧名」，再往下保留分享内的原目录。
	Dir string `json:"dir,omitempty"`
	// Category 用户自定义分类（如 国产剧/欧美剧/华语电影/动画电影），可空。
	Category string `json:"category,omitempty"`
	// Show 剧名（用户可改）。追更刷新时靠它定位目录，保证路径稳定。
	Show string `json:"show,omitempty"`
	ShareID  string `json:"share_id,omitempty"` // 分享标识
	Passcode string `json:"passcode,omitempty"` // 提取码
	FileID   string `json:"file_id,omitempty"`  // 分享内文件 ID

	// —— 夸克 TV 302 专用字段（普通用户无感，开关打开时才用上）——
	// FidToken 分享文件的访问令牌（share_fid_token），播放时转存要用；
	// 导入分享时一并入库，避免播放时再扫整棵分享树。
	FidToken string `json:"fid_token,omitempty"`
	// TVFID 转存到「自己盘」后的文件 fid（夸克 TV 接口只认自己盘的文件）。
	// 播过一次就有；被 sweeper 清掉后取链失败会自动重转存，用户无感。
	TVFID string `json:"tv_fid,omitempty"`

	// —— 天翼云盘专用字段 ——
	// ShareNumID 天翼分享的数字 shareId。GetFileDownload 取链时要用；
	// 解析分享时一并入库，播放时不必再调 GetShareInfo 换 shareId。
	// 订阅号收录的分享没有 shareCode，播放全靠这个字段。
	ShareNumID string `json:"share_num_id,omitempty"`
	// SubUserID 天翼订阅号收录的条目会记下 UP 主 ID（普通链接导入为空）。
	// 这类条目不进「分享列表」（它们由订阅号面板统一管理、刷新）。
	SubUserID string `json:"sub_user_id,omitempty"`

	// 静态直链（兼容：手动填 URL 的老用法）
	URL string `json:"url,omitempty"`

	UA      string    `json:"ua,omitempty"`  // 取流 UA（部分网盘强校验）
	Ext     string    `json:"ext,omitempty"` // 真实扩展名（含点，如 .mkv）。决定播放器用哪个解析器，不能瞎写
	Size    int64     `json:"size"`
	AddedAt time.Time `json:"added_at"`

	// —— 以下为旧字段，仅用于读取历史数据，不再写入 ——
	ShareCode     string `json:"share_code,omitempty"`
	SharePwd      string `json:"share_pwd,omitempty"`
	QuarkPwdID    string `json:"quark_pwd_id,omitempty"`
	QuarkPasscode string `json:"quark_passcode,omitempty"`
	QuarkFID      string `json:"quark_fid,omitempty"`
}

// migrate 把旧格式条目升级到统一模型，并补齐目录层级。
func (i *Item) migrate() {
	if i.Provider == "" {
		switch {
		case i.QuarkPwdID != "":
			i.Provider = "quark"
			i.ShareID = i.QuarkPwdID
			i.Passcode = i.QuarkPasscode
			i.FileID = i.QuarkFID
		case i.ShareCode != "":
			i.Provider = "115"
			i.ShareID = i.ShareCode
			i.Passcode = i.SharePwd
		}
		// 旧字段清空
		i.ShareCode, i.SharePwd = "", ""
		i.QuarkPwdID, i.QuarkPasscode, i.QuarkFID = "", "", ""
	}
	// 老数据的 Title 曾是 "分享名/文件名" 挤在一起 —— 拆到 Dir
	if i.Dir == "" {
		if idx := strings.LastIndex(i.Title, "/"); idx >= 0 {
			i.Dir = strings.Trim(i.Title[:idx], "/")
			i.Title = i.Title[idx+1:]
		}
	}
	// 目录统一加上「网盘中文名」顶层（如 "夸克/剧名"），并规范化。
	// 每一段都做安全化：分享名里常带 # & ? 等字符，直接进 URL 会被截断。
	if i.Dir != "" {
		i.Dir = strings.Trim(i.Dir, "/")
		top := strings.SplitN(i.Dir, "/", 2)[0]
		if top != providerCN(i.Provider) && top != i.Provider {
			i.Dir = providerCN(i.Provider) + "/" + i.Dir
		} else if top == i.Provider {
			i.Dir = providerCN(i.Provider) + "/" + strings.TrimPrefix(i.Dir, i.Provider+"/")
		}
		parts := strings.Split(i.Dir, "/")
		for k := range parts {
			parts[k] = safeFilename(parts[k])
		}
		i.Dir = strings.Join(parts, "/")
	}
}

// IsShare 表示该条目是「分享直连」类型（虚拟盘）。
func (i *Item) IsShare() bool {
	return i.Provider != "" && i.ShareID != "" && i.FileID != ""
}

// NeedsLiveURL 表示播放时需要实时换取直链。
func (i *Item) NeedsLiveURL() bool {
	return i.IsShare()
}

// FileName 返回该条目在 WebDAV 中呈现的文件名。
// 【注意】必须保持真实视频后缀（.mp4/.mkv）：
//   - 网易爆米花等外部 WebDAV 播放器按后缀识别文件，302 到 m3u8 后
//     播放器内核自己解析 HLS，实测可播；
//   - 伪装成 .m3u8 会导致这类客户端直接不识别（2026-10-15 实测翻车）。
func (i *Item) FileName() string {
	ext := i.Ext
	if ext == "" {
		ext = ".mp4"
	}
	return safeFilename(i.Title) + ext
}

// FullPath 虚拟盘内的完整路径（不含前导斜杠），如 "夸克/剧名/S01E07.mkv"。
func (i *Item) FullPath() string {
	if i.Dir == "" {
		return i.FileName()
	}
	return i.Dir + "/" + i.FileName()
}

// Store 条目持久化存储（JSON 文件）。
//
// 【性能】N1（ARM A53）上 PROPFIND 列目录曾慢到 8~11 秒：webdav 库对每个子项
// 都会调 Stat → IsDirPath/FindByName，旧实现每次 O(n) 遍历全部条目重算
// FullPath()（字符串拼接），681 条目 × 250 子项 ≈ 34 万次拼接。
// 现加三类索引（FullPath/ID 映射、目录集合、每目录子项预聚合），
// 写入置 dirty、读时按需重建，之后所有查找 O(1)。
type Store struct {
	mu    sync.RWMutex
	path  string
	items []Item
	dirty bool // 索引是否过期（Add/Delete 后置位）

	// —— 落盘（异步合并）——
	// 病历（2026-10-21）：盒子 eMMC 写速仅 2.7MB/s，items.json 涨到 858KB 后
	// 一次全量写要 ~0.3s。批量导入/删除会连续触发几百次写，且旧实现是「持写锁
	// 写盘」——PROPFIND 全部排队，实测从 4ms 涨到 9s 再到超时，客户端报
	// 「WebDAV 连不上」（CPU 0%、内存正常，纯锁等待）。
	// 现在：锁内只 marshal（毫秒级），写盘交给单个后台协程，短时间多次变更合并
	// 成一次写，并先写临时文件再 rename（避免写一半断电留个坏 JSON）。
	saveQ     chan struct{}
	pendingMu sync.Mutex
	pending   []Item
	lastN     int64 // 上次落盘时的条目数（用来决定合并窗口）

	// —— 索引（dirty 时由 rebuildIndex 重建，持写锁访问）——
	fpIdx      map[string]int            // FullPath → items 下标
	fnIdx      map[string][]int          // FileName → items 下标（兼容裸名匹配）
	idIdx      map[string]int            // ID → items 下标
	dirSet     map[string]bool           // 所有祖先目录（IsDirPath 用）
	childCache map[string]*childrenEntry // 目录 → 预聚合的直接子目录与文件
}

// childrenEntry 一个目录的直接子项（预聚合，只读返回）。
type childrenEntry struct {
	dirs  []string
	files []Item
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, saveQ: make(chan struct{}, 1)}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.items)
		// 旧格式自动升级到统一模型
		for i := range s.items {
			s.items[i].migrate()
		}
	}
	s.dirty = true // 首次读时建索引
	go s.saveLoop()
	return s, nil
}

// readLockFresh 拿读锁并确保索引最新。
// 用法：s.readLockFresh(); defer s.mu.RUnlock()
// 索引过期时先在写锁下重建，再重新拿读锁返回（double-check 防并发重复重建）。
func (s *Store) readLockFresh() {
	s.mu.RLock()
	if !s.dirty {
		return
	}
	s.mu.RUnlock()
	s.mu.Lock()
	if s.dirty {
		s.rebuildIndex()
		s.dirty = false
	}
	s.mu.Unlock()
	s.mu.RLock()
}

// rebuildIndex 全量重建索引。调用方必须持写锁。
func (s *Store) rebuildIndex() {
	n := len(s.items)
	s.fpIdx = make(map[string]int, n)
	s.fnIdx = make(map[string][]int, n)
	s.idIdx = make(map[string]int, n)
	s.dirSet = make(map[string]bool, n)
	s.childCache = make(map[string]*childrenEntry, n/4+1)
	// 子目录去重用的临时集合（parent → 子目录名集合）
	dirsSeen := make(map[string]map[string]bool, n/4+1)
	for i := range s.items {
		it := &s.items[i]
		fn := it.FileName()
		var fp string
		if it.Dir == "" {
			fp = fn
		} else {
			fp = it.Dir + "/" + fn
		}
		s.fpIdx[fp] = i
		s.idIdx[it.ID] = i
		s.fnIdx[fn] = append(s.fnIdx[fn], i)
		// 目录链：Dir 本身及其所有祖先
		if it.Dir == "" {
			// 顶层文件（无目录）挂根下
			e := s.childCache[""]
			if e == nil {
				e = &childrenEntry{}
				s.childCache[""] = e
			}
			e.files = append(e.files, *it)
		}
		d := it.Dir
		first := true
		for d != "" {
			s.dirSet[d] = true
			if first {
				// 文件只挂 Dir 本身（不挂各级祖先）
				e := s.childCache[d]
				if e == nil {
					e = &childrenEntry{}
					s.childCache[d] = e
				}
				e.files = append(e.files, *it)
				first = false
			}
			// d 作为子目录挂到其父目录 p
			p := ""
			if idx := strings.LastIndex(d, "/"); idx >= 0 {
				p = d[:idx]
			}
			if p != d {
				// 子目录名只存基名（path.Base）—— webdav 库 walkFS 会
				// path.Join(父路径, fi.Name())，返回完整路径会拼出
				// 「/移动/移动/未命名」这种幽灵路径导致 Stat 失败、
				// 整个 PROPFIND 中断（2026-10-16 实测翻车）。
				base := d
				if idx := strings.LastIndex(d, "/"); idx >= 0 {
					base = d[idx+1:]
				}
				seen := dirsSeen[p]
				if seen == nil {
					seen = map[string]bool{}
					dirsSeen[p] = seen
				}
				if !seen[base] {
					seen[base] = true
					pe := s.childCache[p]
					if pe == nil {
						pe = &childrenEntry{}
						s.childCache[p] = pe
					}
					pe.dirs = append(pe.dirs, base)
				}
			}
			d = p
		}
	}
	// 每个目录的子目录排序（与旧实现 sort.Strings(dirs) 一致）
	for _, e := range s.childCache {
		sort.Strings(e.dirs)
	}
}

// save 调度落盘（调用方必须持写锁）。
//
// 锁内只做一次切片快照（1461 条约 0.1ms），marshal 与磁盘写全交给后台协程。
// 旧实现是「持写锁 marshal + 全量写盘」，在 2.7MB/s 的 eMMC 上一次要 0.3s，
// 批量增删时 WebDAV 的 PROPFIND 全排在这后面 → 客户端报「连不上」。
// saveQ 容量 1 + 非阻塞投递 = 天然合并：写盘期间再来 N 次变更，只留最新快照。
func (s *Store) save() {
	snap := make([]Item, len(s.items))
	copy(snap, s.items)
	s.pendingMu.Lock()
	s.pending = snap
	s.pendingMu.Unlock()
	select {
	case s.saveQ <- struct{}{}:
	default: // 已有待写任务，后台会把最新快照一起写掉
	}
}

// saveLoop 后台落盘：合并短时间内的多次变更，先写临时文件再 rename。
func (s *Store) saveLoop() {
	for range s.saveQ {
		// 合并窗口：批量导入/删除会在几十毫秒内触发上百次 save，
		// 等这一下就能只写一次盘。库越大（JSON 越大），marshal+写盘越久，
		// 窗口同步拉长，避免「刚写完又立刻重写」把 eMMC 打满。
		time.Sleep(storeSaveDelay(int(atomic.LoadInt64(&s.lastN))))
		s.pendingMu.Lock()
		snap := s.pending
		s.pending = nil
		s.pendingMu.Unlock()
		if snap == nil {
			continue
		}
		atomic.StoreInt64(&s.lastN, int64(len(snap)))
		s.writeSnapshot(snap)
	}
}

// writeSnapshot 一次原子落盘（临时文件 + rename，避免写一半断电留坏 JSON）。
func (s *Store) writeSnapshot(snap []Item) {
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Flush 把待写数据立即落盘（退出前调用，避免丢最后一批变更）。
func (s *Store) Flush() {
	s.pendingMu.Lock()
	snap := s.pending
	s.pending = nil
	s.pendingMu.Unlock()
	if snap == nil {
		// 没有待写快照时也补一次，保证内存与磁盘一致（例如刚启动就退出）
		s.mu.RLock()
		snap = make([]Item, len(s.items))
		copy(snap, s.items)
		s.mu.RUnlock()
	}
	s.writeSnapshot(snap)
}

func (s *Store) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}

func (s *Store) Add(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it.ID == "" {
		it.ID = newID()
	}
	if it.AddedAt.IsZero() {
		it.AddedAt = time.Now()
	}
	// 索引过期先重建，让下面的同路径查找 O(1)
	if s.dirty {
		s.rebuildIndex()
		s.dirty = false
	}
	// 同路径覆盖（位置与 ID 不变，仅值被替换）
	if i, ok := s.fpIdx[it.FullPath()]; ok {
		it.ID = s.items[i].ID
		it.AddedAt = s.items[i].AddedAt
		s.items[i] = it
		// childCache 里存的是 Item 值拷贝（size/file_id 等可能已变），
		// 必须置脏让下次读重建，否则 PROPFIND 显示旧 size。
		s.dirty = true
		s.save()
		return
	}
	s.items = append(s.items, it)
	s.dirty = true // 新条目：索引需重建（读路径按需处理）
	s.save()
}

// AddBatch 批量写入，只在整个批次结束时落盘一次。
//
// 为什么需要：Add 每次都全量重写 items.json，导入 N 条就是 O(N²) 写盘量。
// 一次导入几百集时，N1 的磁盘会被拖得很明显（每加一条重写几十 KB~几 MB）。
func (s *Store) AddBatch(list []Item) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.rebuildIndex()
		s.dirty = false
	}
	n := 0
	for _, it := range list {
		if it.ID == "" {
			it.ID = newID()
		}
		if it.AddedAt.IsZero() {
			it.AddedAt = time.Now()
		}
		fp := it.FullPath()
		if i, ok := s.fpIdx[fp]; ok {
			// 同路径覆盖：ID 与入库时间不变
			it.ID = s.items[i].ID
			it.AddedAt = s.items[i].AddedAt
			s.items[i] = it
			n++
			continue
		}
		s.items = append(s.items, it)
		idx := len(s.items) - 1
		s.fpIdx[fp] = idx
		s.fnIdx[it.FileName()] = append(s.fnIdx[it.FileName()], idx)
		n++
	}
	s.dirty = true
	s.save()
	return n
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 索引过期先重建，让下面 O(1) 定位（旧实现是 O(N) 线性扫描；
	// 脏状态下两者同为 O(N)，干净状态下这里是 O(1)）
	if s.dirty {
		s.rebuildIndex()
		s.dirty = false
	}
	i, ok := s.idIdx[id]
	if !ok {
		return false
	}
	s.items = append(s.items[:i], s.items[i+1:]...)
	s.dirty = true // 下标全部移位，下次读重建
	s.save()
	return true
}

// DeleteBatch 批量按 ID 删除：整批只拿一次写锁、只重建一次索引、只落盘一次。
// 删除整季剧集（几百条）时，逐条 Delete 会做几百次索引重建 + 几百次快照，
// 在盒子上实测 337 条要 3.2s；批量版是毫秒级。
func (s *Store) DeleteBatch(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	del := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		del[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.items[:0]
	n := 0
	for _, it := range s.items {
		if _, ok := del[it.ID]; ok {
			n++
			continue
		}
		kept = append(kept, it)
	}
	if n == 0 {
		return 0
	}
	s.items = kept
	s.dirty = true
	s.save()
	return n
}

// Update 按 ID 原地修改条目（改分类/搬目录用）：ID 与入库时间不变，
// 只让 fn 动它要动的字段。改完置脏让索引下次读时重建，并立即落盘。
func (s *Store) Update(id string, fn func(*Item)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.rebuildIndex()
		s.dirty = false
	}
	i, ok := s.idIdx[id]
	if !ok {
		return false
	}
	fn(&s.items[i])
	s.dirty = true // Dir/Category 变了：路径索引与目录缓存都要重建
	s.save()
	return true
}

// UpdateBatch 批量原地修改：match 选中的条目逐个过 fn，整批只拿一次写锁、
// 只重建一次索引、只落盘一次。返回修改条数。
//
// 【为什么需要】改分类（整季几百条换目录）老写法是逐条 Update：每条开头
// rebuild-if-dirty、结尾置脏，下一条又触发一次全量重建 —— 300 集就是 300 次
// （1 万库 × 6.4ms ≈ 2 秒，全卡在同步路径里）。批量版还能让读者看到
// 「整批一起换」的原子效果，不会看到一半搬完的库。
// match/fn 都在持写锁状态下调用，不要在里面做耗时 I/O。
func (s *Store) UpdateBatch(match func(*Item) bool, fn func(*Item)) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.items {
		if !match(&s.items[i]) {
			continue
		}
		fn(&s.items[i])
		n++
	}
	if n > 0 {
		s.dirty = true
		s.save()
	}
	return n
}

func (s *Store) FindByName(name string) *Item {
	s.readLockFresh()
	defer s.mu.RUnlock()
	if i, ok := s.fpIdx[name]; ok {
		it := s.items[i]
		return &it
	}
	// 兼容裸文件名匹配（旧语义：FullPath 优先，其次 FileName）
	for _, i := range s.fnIdx[name] {
		it := s.items[i]
		return &it
	}
	return nil
}

// Children 聚合出 dir 目录下的直接子目录与文件（虚拟树）。
// dir 为空表示根：子目录就是各网盘中文名。
// 走索引 O(1)；返回的内部 slice 只读，调用方不得修改。
func (s *Store) Children(dir string) (dirs []string, files []Item) {
	s.readLockFresh()
	defer s.mu.RUnlock()
	dir = strings.Trim(dir, "/")
	if e := s.childCache[dir]; e != nil {
		return e.dirs, e.files
	}
	return nil, nil
}

// IsDirPath 判断虚拟盘内路径是不是目录（有子项挂在它下面）。
func (s *Store) IsDirPath(path string) bool {
	s.readLockFresh()
	defer s.mu.RUnlock()
	return s.dirSet[strings.Trim(path, "/")]
}

func (s *Store) FindByID(id string) *Item {
	s.readLockFresh()
	defer s.mu.RUnlock()
	if i, ok := s.idIdx[id]; ok {
		it := s.items[i]
		return &it
	}
	return nil
}

var idSeq int64
var idMu sync.Mutex

func newID() string {
	idMu.Lock()
	defer idMu.Unlock()
	idSeq++
	return strings.ReplaceAll(time.Now().Format("20060102150405"), "-", "") +
		"-" + itoa(idSeq)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ShareSummary 一条已导入分享的概要（由条目聚合而来），供追更刷新。
type ShareSummary struct {
	Provider  string `json:"provider"`
	ShareID   string `json:"share_id"`
	Passcode  string `json:"passcode,omitempty"`
	Category  string `json:"category,omitempty"` // 分类（如 国产剧/欧美剧）
	ShowName  string `json:"show_name"`          // 剧名
	Files     int    `json:"files"`
	LastAdded string `json:"last_added"`
	// ShareURL 反构出的原始分享链接，前端剧名超链接到网盘原页面用。
	// 天翼的访问码没有 URL 参数位，落地页自行输入。
	ShareURL string `json:"share_url,omitempty"`
}

// shareURL 由（网盘 + 分享ID + 提取码）反构原始分享链接。
// 各家的输出格式与 parseXXXLink 接受的输入严格对应（见各家 provider 注释），
// 保证「点链接能打开、把链接粘回来还能解析」。
func shareURL(provider, shareID, passcode string) string {
	if shareID == "" {
		return ""
	}
	q := func(k, v string) string {
		if v == "" {
			return ""
		}
		return "?" + k + "=" + url.QueryEscape(v)
	}
	switch provider {
	case "quark":
		return "https://pan.quark.cn/s/" + shareID + q("pwd", passcode)
	case "115":
		return "https://115.com/s/" + shareID + q("password", passcode)
	case "baidu":
		return "https://pan.baidu.com/s/" + shareID + q("pwd", passcode)
	case "tianyi":
		return "https://cloud.189.cn/t/" + shareID
	case "mobile139":
		return "https://yun.139.com/shareweb/#/w/i/" + shareID + q("pwd", passcode)
	case "uc":
		return "https://drive.uc.cn/s/" + shareID + q("pwd", passcode)
	}
	if strings.HasPrefix(provider, "doubao") {
		if shareID == "" {
			return "" // 自己盘里的条目没有分享链接
		}
		return "https://www.doubao.com/drive/s/" + shareID
	}
	return ""
}

// Shares 按（网盘 + 分享ID）聚合出已导入的分享清单。
func (s *Store) Shares() []ShareSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type agg struct {
		pass string
		show string
		cat  string
		n    int
		last string
	}
	m := map[string]*agg{}
	var order []string
	for _, it := range s.items {
		if it.Provider == "" || it.ShareID == "" || it.Dir == "" {
			continue
		}
		if it.SubUserID != "" {
			continue // 订阅号收录的条目不进分享列表（由天翼订阅号面板管理）
		}
		key := it.Provider + "|" + it.ShareID
		a := m[key]
		if a == nil {
			a = &agg{}
			m[key] = a
			order = append(order, key)
		}
		a.n++
		a.pass = it.Passcode
		// 剧名/分类以入库时记录为准（buildItemDir 统一构造目录，字段最可靠）；
		// 老条目没有这两个字段时才按目录结构猜：网盘/分类/剧名/…。
		if it.Show != "" {
			a.show = it.Show
		} else {
			parts := strings.Split(it.Dir, "/")
			if len(parts) >= 3 {
				a.show = parts[2]
			} else if len(parts) >= 2 {
				a.show = parts[1]
			}
		}
		if it.Category != "" {
			a.cat = it.Category
		} else {
			parts := strings.Split(it.Dir, "/")
			if len(parts) >= 3 {
				a.cat = parts[1]
			}
		}
		ts := it.AddedAt.Format(time.RFC3339)
		if ts > a.last {
			a.last = ts
		}
	}
	out := make([]ShareSummary, 0, len(order))
	for _, k := range order {
		a := m[k]
		kv := strings.SplitN(k, "|", 2)
		out = append(out, ShareSummary{
			Provider: kv[0], ShareID: kv[1],
			Passcode: a.pass, Category: a.cat, ShowName: a.show,
			Files: a.n, LastAdded: a.last,
			ShareURL: shareURL(kv[0], kv[1], a.pass),
		})
	}
	return out
}

// FindByPath 按虚拟盘完整路径查找条目（"夸克/剧名/文件.mkv"）。
// FindByPath 按 FullPath 查条目（O(1) 走 fpIdx）。
// 【2026-10-21 优化】老实现是持读锁线性扫描：追更刷新每个文件都调一次，
// 50 集 × 1 万库 = 50 万次 FullPath() 字符串拼接，全卡在同步路径里。
// 改为 readLockFresh + fpIdx，语义不变（同样返回命中条目的值拷贝）。
func (s *Store) FindByPath(path string) *Item {
	path = strings.Trim(path, "/")
	s.readLockFresh()
	defer s.mu.RUnlock()
	if i, ok := s.fpIdx[path]; ok {
		it := s.items[i]
		return &it
	}
	return nil
}

// storeSaveDelay 按库大小决定合并窗口。
// 小库（几百条）120ms 足够；几万条时 JSON 几十 MB，marshal + eMMC 写要好几秒，
// 窗口必须拉长，否则会连续重写同一份大文件。
func storeSaveDelay(n int) time.Duration {
	switch {
	case n > 50000:
		return 3 * time.Second
	case n > 20000:
		return 1500 * time.Millisecond
	case n > 8000:
		return 500 * time.Millisecond
	default:
		return 120 * time.Millisecond
	}
}
