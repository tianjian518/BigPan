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
			writeJSON(w, map[string]interface{}{
				"ok":          true,
				"config":      strmCfg,
				"dir":         strmRootDir(),
				"dir_startup": strmRoot,
				"base_url":    strmPublicBase(self),
				"self":        self,
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
			strmMu.Lock()
			n, err := buildSTRM(store, self)
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
// 用在「导入分享 / 删除条目 / 巡检清理」之后：让 .strm 与库保持强同步，
// 又不阻塞接口响应（重建 100+ 个文件约 1~2 秒）。
// AutoRebuild=false 时跳过（用户明确关掉了自动重建，只能手动点重建）。
func rebuildSTRMAsync(store *Store, self, reason string) {
	if self == "" || !strmCfg.AutoRebuild {
		return
	}
	go func() {
		strmMu.Lock()
		defer strmMu.Unlock()
		n, err := buildSTRM(store, self)
		if err != nil {
			log.Printf("[strm] %s后重建失败: %v", reason, err)
			return
		}
		log.Printf("[strm] %s后已重建：%d 个", reason, n)
	}()
}

// apiSTRMRebuild 手动/脚本触发全量重建。
func apiSTRMRebuild(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		strmMu.Lock()
		n, err := buildSTRM(store, self)
		strmMu.Unlock()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "count": n, "dir": strmRoot, "self": self})
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

// buildSTRM 全量重建 STRM 目录树，返回生成数量。
//
// 清理策略：只删「自身或子树里含 .strm 的目录」以及根下的 .strm 文件，
// 不碰 strmRoot 以外的任何东西；用户若往该目录塞了别的文件也不会误删。
func buildSTRM(store *Store, self string) (int, error) {
	items := store.List()
	root := strmRootDir()

	if err := os.MkdirAll(root, 0755); err != nil {
		return 0, fmt.Errorf("建目录失败: %w", err)
	}
	if err := cleanSTRM(root); err != nil {
		return 0, fmt.Errorf("清理旧 strm 失败: %w", err)
	}

	n, failed, skipped := 0, 0, 0
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
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("[strm] 建目录失败 %s: %v", dir, err)
			failed++
			continue
		}
		name := it.FileName() + ".strm"
		// 不写末尾换行：部分媒体服务器解析 strm 时对空行敏感，纯 URL 最稳。
		content := itemPlayURL(it, self)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			log.Printf("[strm] 写文件失败 %s: %v", name, err)
			failed++
			continue
		}
		n++
	}
	// 目录权限统一放宽：SmartStrm 踩过的坑 —— 媒体服务进程用户不同，
	// 700 的目录会让飞牛影视完全读不到。
	fixPerms(root)
	log.Printf("[strm] 重建完成：%d 个 .strm（失败 %d，按配置跳过 %d），非夸克一律 302 直连，根目录 %s，对外地址 %s，层级 %s",
		n, failed, skipped, root, strmPublicBase(self), strmCfg.Layout)
	return n, nil
}

// cleanSTRM 只删 .strm 文件与因此变空的目录，其他内容一律保留。
func cleanSTRM(root string) error {
	var dirs []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
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
			_ = os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 自下而上删空目录（含只剩空子目录的）
	for i := len(dirs) - 1; i >= 0; i-- {
		ents, err := os.ReadDir(dirs[i])
		if err == nil && len(ents) == 0 {
			_ = os.Remove(dirs[i])
		}
	}
	return nil
}

// fixPerms 目录 755、文件 644，并把属主改为 fnOS 的 admin:Users(1000:1001)。
// 媒体服务（飞牛影视 mediasrv）通常以普通用户运行，root 属主 + 700 会直接读不到
// （SmartStrm 踩过同样的坑）。chown 失败（非 root 运行）忽略即可，权限位仍有效。
func fixPerms(root string) {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		_ = os.Chown(p, 1000, 1001)
		if info.IsDir() {
			_ = os.Chmod(p, 0755)
		} else {
			_ = os.Chmod(p, 0644)
		}
		return nil
	})
}
