package main

// ---------------------------------------------------------------------------
// STRM 导出：把虚拟盘镜像成本地 .strm 目录树，供飞牛影视等媒体服务器扫描。
//
// 为什么要这个：飞牛影视走 WebDAV 挂载时，媒体库扫描会 ffprobe 读每个文件头，
// 而百度分享必须先「转存到网盘」才有流 —— 全库扫一遍就把网盘空间撑爆
// （实测 2026-10-17：33 集 × 1.7GB = 56GB，直接 errno=12 全线 502）。
//
// STRM 是几 KB 的文本快捷方式，扫描阶段只读本地文本，**零转存**；
// 只有真正播放某一集时才由 bigpan 转存那一个文件。
//
// URL 铁律：**一律 302 直连**（/direct/），数据由客户端直接去网盘 CDN 拉，
// 绝不经过部署机。理由：本项目要部署在甲骨文等云服务器上，走中转 = 视频流量
// 全部计入出网账单，而且必然卡。代码里不允许存在「中转/直连」开关。
//
// 唯一例外：夸克（quark）拿不到可用直链，技术上无法 302，只能走 /play 中转。
// 夸克不会部署在云服务器上（家用盒子专属），因此不构成流量风险。
// ---------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// strmRoot 导出根目录，由 -strm 参数指定（默认 /data/strm）。
// 推荐挂到用户空间（如 /vol1/1000/bigpan），fnOS 的媒体服务对 docker 目录常无读权限。
var strmRoot = "/data/strm"

// strmMu 串行化重建（手动 API 与追更定时器可能同时触发）。
var strmMu sync.Mutex

// strmConfigPath 配置文件路径（与 items.json 同目录）。
var strmConfigPath = "/data/strm_config.json"

// STRMConfig 导出设置，网页/API 可改。
//
// 参考 CloudPan189-share 与 SmartStrm 的做法：生成任务不该写死，
// 路径、播放模式、格式白名单、过滤条件都交给用户。
type STRMConfig struct {
	// Dir 输出根目录。空 = 用启动参数 -strm（默认 /data/strm）。
	// 每台机器挂载位置不同，网页上可改，改完立即重建。
	Dir string `json:"dir"`
	// BaseURL 写进 .strm 的对外地址（如 http://192.168.1.10:8099 或 https://pan.example.com）。
	// 空 = 用 -self / 自动探测。多网卡、容器、反代场景必须手填，否则生成的地址别人访问不到。
	BaseURL string `json:"base_url"`
	// Layout 目录层级：
	//   full        = 网盘/分类/剧名（默认，与现有部署一致）
	//   no_provider = 分类/剧名（不想要"百度/夸克"这层时用）
	//   show_only   = 仅剧名（最扁平）
	Layout string `json:"layout"`
	// Providers 只导出这些网盘（空 = 全部）
	Providers []string `json:"providers"`
	// Categories 只导出这些分类（空 = 全部）
	Categories []string `json:"categories"`
	// Exts 视频后缀白名单（空 = 内置默认列表）
	Exts []string `json:"exts"`
	// AutoRebuild 追更新增剧集后自动重建
	AutoRebuild bool `json:"auto_rebuild"`
}

var strmCfg = STRMConfig{
	Layout:      "full",
	AutoRebuild: true,
}

var defaultSTRMExts = []string{".mp4", ".mkv", ".iso", ".ts", ".m2ts", ".avi", ".rmvb", ".rm", ".wmv", ".mov", ".flv", ".webm", ".mpg", ".mpeg", ".vob"}

// loadSTRMConfig 读配置；文件不存在时用默认值并落盘一份，方便用户直接改。
func loadSTRMConfig(path string) {
	strmConfigPath = path
	b, err := os.ReadFile(path)
	if err != nil {
		_ = saveSTRMConfig()
		return
	}
	var c STRMConfig
	if json.Unmarshal(b, &c) == nil {
		if len(c.Exts) == 0 {
			c.Exts = defaultSTRMExts
		}
		switch c.Layout {
		case "full", "no_provider", "show_only":
		default:
			c.Layout = "full" // 老配置文件没有该字段，保持原有层级
		}
		strmCfg = c
	}
}

func saveSTRMConfig() error {
	b, _ := json.MarshalIndent(strmCfg, "", "  ")
	return os.WriteFile(strmConfigPath, b, 0644)
}

// ---------------------------------------------------------------------------
// STRM 同步状态：记住「上次同步时用的对外地址」
//
// 为什么需要：.strm 文件内容就是 <对外地址>/direct/... 的播放地址。地址一变
// （换域名/换IP/换反代），存量 .strm 全部指向失效地址，必须重写 —— 而一次
// 性重写全部文件，飞牛影视/Emby 会把整个媒体库当成新内容重新刮削（重新拉
// 元数据、重新匹配、重新生成封面）。库小无所谓，几万部时能把服务器和 TMDB
// 一起拖垮。所以：
//   - 自动路径（导入/追更/删除/巡检/改分类）检测到地址变过，只做安全部分：
//     删孤儿、写新文件；存量文件原样留着，等用户自己点「全量重建 STRM」。
//   - 人工路径（设置页按钮 / 保存配置）才全量重写，并刷新这里记录的地址。
// ---------------------------------------------------------------------------

// strmState 上次同步完成时的状态。
type strmState struct {
	// BuiltBase 上次写入 .strm 使用的对外地址（strmPublicBase 的值）。
	BuiltBase string `json:"built_base"`
	// BuiltAt 上次同步完成时间（仅展示用）。
	BuiltAt time.Time `json:"built_at"`
}

// strmStatePath 状态文件路径（与 strm_config.json 同目录，即 /data/strm_state.json）。
func strmStatePath() string {
	return filepath.Join(filepath.Dir(strmConfigPath), "strm_state.json")
}

func loadSTRMState() strmState {
	var st strmState
	if b, err := os.ReadFile(strmStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveSTRMState(st strmState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(strmStatePath(), b, 0644)
}

// detectSTRMBase 从现有 .strm 内容反推对外地址。
//
// 用在状态文件丢失 / 老版本升级：盘上 .strm 都是 <base>/direct/... 或
// <base>/play/...，取标记前的前缀即为当时用的地址。读不到返回空串
// （调用方按「不拦截、正常同步」处理，行为与老版本一致）。
func detectSTRMBase(root string) string {
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || found != "" || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".strm") {
			return nil
		}
		b, e := os.ReadFile(p) // .strm 只有一行 URL，几 KB，直接读
		if e != nil {
			return nil
		}
		s := string(b)
		for _, mark := range []string{"/direct/", "/play/"} {
			if i := strings.Index(s, mark); i > 0 {
				found = s[:i]
				break
			}
		}
		if found != "" {
			return filepath.SkipAll // 找到一个就够，全部文件同源
		}
		return nil
	})
	return found
}

// strmStatus 汇总 STRM 同步状态（设置页展示用）。
// 返回：是否「地址已变等待手动重建」、上次同步用的地址、当前生效地址。
// 状态文件缺失时先从盘上 .strm 反推（只读，不落盘；落盘由 buildSTRM 负责）。
func strmStatus(self string) (stale bool, builtBase, curBase string) {
	st := loadSTRMState()
	curBase = strmPublicBase(self)
	builtBase = st.BuiltBase
	if builtBase == "" {
		builtBase = detectSTRMBase(strmRootDir())
	}
	stale = builtBase != "" && builtBase != curBase
	return
}

// wantItem 按配置过滤条目。
func (c *STRMConfig) wantItem(it *Item) bool {
	if len(c.Providers) > 0 {
		ok := false
		for _, p := range c.Providers {
			if strings.EqualFold(p, it.Provider) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(c.Categories) > 0 && it.Category != "" {
		ok := false
		for _, cat := range c.Categories {
			if cat == it.Category {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	exts := c.Exts
	if len(exts) == 0 {
		exts = defaultSTRMExts
	}
	lext := strings.ToLower(it.Ext)
	for _, e := range exts {
		if strings.EqualFold(e, lext) {
			return true
		}
	}
	return false
}

// apiSTRMConfig 读写导出设置（POST 保存后可选立即重建）。
func apiSTRMConfig(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			stale, builtBase, curBase := strmStatus(self)
			st := loadSTRMState()
			if st.BuiltBase == "" {
				st.BuiltBase = builtBase
			}
			writeJSON(w, map[string]interface{}{
				"ok":          true,
				"config":      strmCfg,
				"dir":         strmRootDir(),
				"dir_startup": strmRoot,
				"base_url":    curBase,
				"self":        self,
				"built_base":  st.BuiltBase,
				"built_at":    st.BuiltAt,
				"stale":       stale,
			})
		case http.MethodPost:
			var c STRMConfig
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误: " + err.Error()})
				return
			}
			if len(c.Exts) == 0 {
				c.Exts = defaultSTRMExts
			}
			switch c.Layout {
			case "full", "no_provider", "show_only":
			default:
				c.Layout = "full"
			}
			strmCfg = c
			if err := saveSTRMConfig(); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "保存失败: " + err.Error()})
				return
			}
			// 用户亲手点的「保存并重建」= 人工触发：地址变过也全量重写
			strmMu.Lock()
			n, err := buildSTRM(store, self, true)
			strmMu.Unlock()
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{"ok": true, "count": n, "config": strmCfg})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// rebuildSTRMAsync 后台串行重建 STRM。
//
// 用在「导入分享 / 删除条目 / 改分类 / 巡检清理 / 手动刷新」之后：让 .strm
// 与库保持强同步，又不阻塞接口响应（重建 100+ 个文件约 1~2 秒）。
// AutoRebuild=false 时跳过（用户明确关掉了自动重建，只能手动点重建）。
//
// force=false：对外地址变过时只删孤儿、写新文件，存量 .strm 原样保留
// （重写全部会让媒体库重新刮削整个库，见 strmState 注释）。
func rebuildSTRMAsync(store *Store, self, reason string) {
	if self == "" || !strmCfg.AutoRebuild {
		return
	}
	go func() {
		strmMu.Lock()
		defer strmMu.Unlock()
		n, err := buildSTRM(store, self, false)
		if err != nil {
			log.Printf("[strm] %s后重建失败: %v", reason, err)
			return
		}
		log.Printf("[strm] %s后已重建：%d 个", reason, n)
	}()
}

// apiSTRMRebuild 手动全量重建（设置页按钮）：地址变过也全部重写。
func apiSTRMRebuild(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		strmMu.Lock()
		n, err := buildSTRM(store, self, true)
		strmMu.Unlock()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		_, builtBase, curBase := strmStatus(self)
		writeJSON(w, map[string]interface{}{"ok": true, "count": n, "dir": strmRoot, "self": self, "built_base": builtBase, "base_url": curBase})
	}
}

// strmRootDir 当前生效的输出根目录：网页配置优先，其次启动参数 -strm。
func strmRootDir() string {
	if d := strings.TrimSpace(strmCfg.Dir); d != "" {
		return d
	}
	return strmRoot
}

// strmPublicBase 当前生效的对外地址：网页配置优先，其次 -self / 自动探测。
// 反代、域名、多网卡场景必须手填，否则生成的 .strm 指向的地址客户端访问不到。
func strmPublicBase(self string) string {
	if b := strings.TrimSpace(strmCfg.BaseURL); b != "" {
		return strings.TrimRight(b, "/")
	}
	return strings.TrimRight(self, "/")
}

// strmItemDir 按布局把虚拟目录（网盘/分类/剧名）映射成 .strm 子目录。
func strmItemDir(it *Item) string {
	p := strings.Trim(it.Dir, "/")
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	switch strmCfg.Layout {
	case "show_only":
		return segs[len(segs)-1]
	case "no_provider":
		if len(segs) > 1 {
			return strings.Join(segs[1:], "/")
		}
		return segs[0]
	default: // full
		return p
	}
}

// itemPlayURL 构造条目在 STRM 里要写的播放地址。
//
// 铁律：非夸克一律 302 直连（/direct/），数据不过本机。
// 夸克是唯一例外——取不到可用直链，只能 /play 中转。
//
// 两种形态都必须带文件名后缀：飞牛影视、Emby 等媒体服务器按 URL 后缀选解码器，
// 没有扩展名会被判成「未知格式」直接拒绝播放（症状：媒体库能扫出来、点播放全失败）。
//
//	strm URL(direct) = http://host/direct/网盘/分类/剧名/文件名.mkv?k=<路径令牌>
//	strm URL(quark)  = http://host/play/文件名.mkv?id=xxx
func itemPlayURL(it *Item, self string) string {
	base := strmPublicBase(self)
	// 夸克例外：无法 302，只能本机中转。
	if strings.EqualFold(it.Provider, "quark") {
		v := url.Values{}
		if it.IsShare() || it.URL == "" {
			v.Set("id", it.ID)
		} else {
			v.Set("url", it.URL)
		}
		if it.Size > 0 {
			v.Set("size", fmt.Sprintf("%d", it.Size))
		}
		return base + "/play/" + url.PathEscape(it.FileName()) + "?" + v.Encode()
	}
	// 其余一律 302 直连：路径即虚拟盘内完整路径，/direct/ 按路径查条目后回 302。
	// 带 k=路径令牌免去 Basic Auth（播放器不带用户名密码）。
	p := it.FullPath()
	return base + "/direct/" + escapePath(p) + "?k=" + strmPathToken(p)
}

// strmPathToken 为 /direct/ 路径签发播放令牌。
//
// 由账号密码派生（无状态）：改密码后旧 STRM 失效，重建一次即可；
// 令牌只对该路径有效，换路径签不出来，不会变成万能通行证。
func strmPathToken(path string) string {
	sum := sha256.Sum256([]byte("bigpan-strm|" + *flagUser + "|" + *flagPass + "|" + path))
	return hex.EncodeToString(sum[:12])
}

// escapePath 逐段转义路径（保留 / 作为分隔符）。
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// buildSTRM 增量同步 STRM 目录树，返回写入（新增+更新）的文件数。
//
// 【2026-10-18 全量改增量】旧实现是 cleanSTRM 删光所有 .strm 再全写一遍：
// 每追更出一集新剧，整个库的文件全部换 inode/mtime，飞牛影视会把全部片子
// 当成「新内容」重新刮削（重新拉元数据、重新匹配、重新生成封面）。库小无
// 所谓，几万部时能把服务器和 TMDB 一起拖垮。现在按「期望集合 vs 磁盘实况」
// 做差量：内容不一致的才写，多出来的才删，其余原样不动（mtime 不变，媒体
// 服务器根本感知不到）——追更新集只落 1 个新文件，飞牛只刮削这一集。
//
// 【2026-10-19 地址变动守卫】force 区分人工与自动：
//   - force=false（导入/追更/删除/巡检/改分类等自动路径）：若对外地址相对
//     上次同步变过（存量 .strm 全部失效），只删孤儿、写新文件；存量文件
//     仅因旧地址而不同的，原样保留等用户点「全量重建 STRM」——自动全量
//     重写会让媒体库重新刮削整个库，这个决定权交给用户。
//   - force=true（设置页按钮 / 保存配置）：地址变过也全部重写，并刷新
//     strmState 里记录的地址。
//
// 清理策略不变：只删 .strm 及因此变空的目录，不碰 strmRoot 以外的任何东西，
// 用户塞进该目录的其他文件不会误删；改名/挪目录/删片依旧自动同步（自愈）。
func buildSTRM(store *Store, self string, force bool) (int, error) {
	items := store.List()
	root := strmRootDir()

	if err := os.MkdirAll(root, 0755); err != nil {
		return 0, fmt.Errorf("建目录失败: %w", err)
	}

	base := strmPublicBase(self)
	st := loadSTRMState()
	if st.BuiltBase == "" {
		// 状态文件丢失 / 老版本升级：按盘上 .strm 反推上次用的地址
		st.BuiltBase = detectSTRMBase(root)
	}
	// 地址变过 = 存量 .strm 全部指向失效地址，需要全量重写（是否重写见 ④）
	baseStale := st.BuiltBase != "" && st.BuiltBase != base

	// ① 期望集合：绝对路径 → 应写内容（布局/过滤/命名规则与旧版完全一致）
	want := make(map[string]string, len(items))
	skipped := 0
	for i := range items {
		it := &items[i]
		if it.Provider == "" && it.URL == "" {
			continue // 纯目录条目（正常不会出现，保险）
		}
		if !strmCfg.wantItem(it) {
			skipped++
			continue
		}
		dir := root
		if sub := strmItemDir(it); sub != "" {
			dir = filepath.Join(root, filepath.FromSlash(sub))
		}
		name := it.FileName() + ".strm"
		// 不写末尾换行：部分媒体服务器解析 strm 时对空行敏感，纯 URL 最稳。
		want[filepath.Join(dir, name)] = itemPlayURL(it, self)
	}

	// ② 磁盘实况：现有 .strm 路径集合（顺带记下目录，供删空目录用）
	have := make(map[string]bool)
	var dirs []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个条目出错不影响整体
		}
		if info.IsDir() {
			if p != root {
				dirs = append(dirs, p)
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(info.Name()), ".strm") {
			have[p] = true
		}
		return nil
	})

	// ③ 删孤儿：盘上有、期望里没有的（失效分享清理/删条目/改名的旧文件）
	del := 0
	for p := range have {
		if _, ok := want[p]; !ok {
			if os.Remove(p) == nil {
				del++
			}
		}
	}

	// ④ 写差量：不存在或内容不一致才落盘；已一致的跳过（mtime 原样保留）
	added, updated, failed, deferred := 0, 0, 0, 0
	for p, content := range want {
		if b, err := os.ReadFile(p); err == nil {
			if string(b) == content {
				continue
			}
			// 地址变过又是自动路径：这个文件仅仅因为旧地址而不同，
			// 原样保留，等用户点「全量重建 STRM」一起换。
			if baseStale && !force && string(b) == st.BuiltBase+strings.TrimPrefix(content, base) {
				deferred++
				continue
			}
		}
		existed := have[p]
		d := filepath.Dir(p)
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Printf("[strm] 建目录失败 %s: %v", d, err)
			failed++
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			log.Printf("[strm] 写文件失败 %s: %v", p, err)
			failed++
			continue
		}
		// 属主/权限只修本次动过的：fnOS 的 mediasrv 以 admin:Users(1000:1001)
		// 运行，root 属主 + 700 会读不到（SmartStrm 踩过的坑）。没动过的文件
		// 上次已经修过，再全库 chown 一遍纯属浪费——十万级文件时那是几万次
		// 系统调用，正是要避免的。
		fixPathPerms(p, d)
		if existed {
			updated++
		} else {
			added++
		}
	}

	// ⑤ 删因此变空的目录（自下而上，含只剩空子目录的）
	for i := len(dirs) - 1; i >= 0; i-- {
		if ents, err := os.ReadDir(dirs[i]); err == nil && len(ents) == 0 {
			_ = os.Remove(dirs[i])
		}
	}

	// ⑥ 刷新状态：人工重建过、地址没变、或已无「等重写」的存量文件时，
	// 记录本次用的地址。地址变过且还有存量文件等手动重建时，保持旧值不动
	// （stale 标志要继续有效）。
	if force || !baseStale || deferred == 0 {
		saveSTRMState(strmState{BuiltBase: base, BuiltAt: time.Now()})
	}
	if baseStale && !force && deferred > 0 {
		log.Printf("[strm] ⚠️ 对外地址已从 %s 变为 %s：%d 个存量 .strm 仍指向旧地址（媒体库里播不了），已原样保留——自动同步不重写它们，否则媒体库会把整个库重新刮削。请到设置页点「全量重建 STRM」一次性更新", st.BuiltBase, base, deferred)
	}

	log.Printf("[strm] 同步完成：新增 %d、更新 %d、删除 %d、未动 %d（按配置跳过 %d，失败 %d，等手动重建 %d），非夸克一律 302 直连，根目录 %s，对外地址 %s，层级 %s",
		added, updated, del, len(have)-del-updated, skipped, failed, deferred, root, base, strmCfg.Layout)
	return added + updated, nil
}

// fixPathPerms 把单个 .strm 文件及其上级目录（直到 root）修到
// 755/644 + admin:Users(1000:1001)，供 fnOS mediasrv 读取。
func fixPathPerms(file, dir string) {
	_ = os.Chown(file, 1000, 1001)
	_ = os.Chmod(file, 0644)
	for d := dir; ; d = filepath.Dir(d) {
		_ = os.Chown(d, 1000, 1001)
		_ = os.Chmod(d, 0755)
		if filepath.Dir(d) == d {
			break // 到达文件系统根，停（防御，正常应在 strmRoot 停）
		}
	}
}
