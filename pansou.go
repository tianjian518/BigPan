package main

// ---------------------------------------------------------------------------
// 资源搜索（PanSou 集成）
//
// PanSou 是一个独立的搜索服务（tianjian518/pansou，112 个插件 + 189 个 TG 频道），
// 标准接口：
//
//	GET {base}/api/search?kw=关键词&res=merge&cloud_types=quark,aliyun,...
//	→ {"code":200,"data":{"total":N,
//	     "merged_by_type":{"quark":[{"url","password","note","datetime"}], ...}}}
//
// 这里只做一层薄代理，不碰任何播放/导入逻辑：
//   1. 地址只认配置里的 base（**不接受前端传 URL**，杜绝 SSRF）；
//   2. 结果拍平成一维列表、按时间倒序、限条数；
//   3. 用 mergeSharePwd 把提取码拼进链接，前端点一下直接进现有导入流程。
//
// 配置为空 = 功能关闭，前端不显示搜索入口。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type panSouConfig struct {
	// URL PanSou 服务地址，例如 http://192.168.1.10:3005（末尾不写斜杠也行）
	URL string `json:"url"`
	// Limit **每个网盘**最多返回多少条（默认 40）。
	// 注意语义：不是总数上限 —— 混在一起截断会把小网盘挤没，筛选就没货了。
	Limit int `json:"limit"`
	// Clouds 只搜这些网盘（空 = 全部）。值用 pansou 的云盘标识：
	// quark / aliyun / baidu / tianyi / uc / 115 / xunlei / magnet / ed2k
	Clouds []string `json:"clouds"`
}

// defaultPanSouClouds 默认只搜本项目能导入的网盘 —— 搜出来点进去发现
// 「暂不支持这个网盘」纯属浪费点击（阿里云盘、PikPak 等都不支持）。
var defaultPanSouClouds = []string{"quark", "baidu", "tianyi", "uc", "115", "mobile"}

var (
	panSouCfg     = panSouConfig{Limit: 40, Clouds: defaultPanSouClouds}
	panSouCfgPath string
)

// panSouCloudCN 云盘标识 → 中文名（前端标签用）。
var panSouCloudCN = map[string]string{
	"quark": "夸克", "aliyun": "阿里云盘", "baidu": "百度", "tianyi": "天翼",
	"uc": "UC", "115": "115", "xunlei": "迅雷", "magnet": "磁力", "ed2k": "电驴",
	"pikpak": "PikPak", "mobile": "移动云盘", "weiyun": "微云",
}

func loadPanSouConfig(path string) {
	panSouCfgPath = path
	b, err := os.ReadFile(path)
	if err != nil {
		_ = savePanSouConfig()
		return
	}
	var c panSouConfig
	if json.Unmarshal(b, &c) == nil {
		if c.Limit <= 0 || c.Limit > 120 {
			c.Limit = 40
		}
		if len(c.Clouds) == 0 {
			c.Clouds = defaultPanSouClouds
		}
		panSouCfg = c
	}
}

func savePanSouConfig() error {
	if panSouCfgPath == "" {
		return fmt.Errorf("配置路径未初始化")
	}
	b, _ := json.MarshalIndent(panSouCfg, "", "  ")
	return os.WriteFile(panSouCfgPath, b, 0644)
}

// panSouItem 拍平后的一条搜索结果。
type panSouItem struct {
	Cloud    string `json:"cloud"`    // quark / baidu / ...
	CloudCN  string `json:"cloud_cn"` // 夸克 / 百度 ...
	Note     string `json:"note"`     // 标题
	URL      string `json:"url"`      // 原始分享链接
	Password string `json:"password"` // 提取码（可能为空）
	Link     string `json:"link"`     // 拼好提取码的链接（前端直接用它导入）
	Datetime string `json:"datetime"` // 分享时间（pansou 给的字符串）
}

// apiPanSouSearch GET /api/pansou/search?kw=关键词
func apiPanSouSearch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kw := strings.TrimSpace(r.URL.Query().Get("kw"))
		if kw == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "请输入关键词"})
			return
		}
		if len([]rune(kw)) > 50 {
			writeJSON(w, map[string]any{"ok": false, "error": "关键词太长（最多 50 字）"})
			return
		}
		base := strings.TrimRight(strings.TrimSpace(panSouCfg.URL), "/")
		if base == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "还没配置 PanSou 地址（到「账号与接入」或下方设置里填）"})
			return
		}
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "PanSou 地址格式不对，应形如 http://ip:3005"})
			return
		}

		q := url.Values{}
		q.Set("kw", kw)
		q.Set("res", "merge")
		if len(panSouCfg.Clouds) > 0 {
			q.Set("cloud_types", strings.Join(panSouCfg.Clouds, ","))
		}
		full := base + "/api/search?" + q.Encode()

		cli := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest(http.MethodGet, full, nil)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		req.Header.Set("Accept", "application/json")
		resp, err := cli.Do(req)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "连不上 PanSou：" + err.Error()})
			return
		}
		defer resp.Body.Close()
		// 限 32MB：pansou 一次全量结果可能很大，别让异常响应打爆内存
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "读取 PanSou 响应失败：" + err.Error()})
			return
		}

		var out struct {
			Code int `json:"code"`
			Data struct {
				Total        int `json:"total"`
				MergedByType map[string][]struct {
					URL      string `json:"url"`
					Password string `json:"password"`
					Note     string `json:"note"`
					Datetime string `json:"datetime"`
				} `json:"merged_by_type"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "PanSou 返回格式无法识别（版本不匹配？）"})
			return
		}

		items := make([]panSouItem, 0, 128)
		// 按网盘分组收集：截断必须**每个网盘各自算**。
		// 混在一起截断时夸克一家动辄上百条，会把百度/天翼挤到只剩几条，
		// 前端按网盘筛选就直接空屏了（v2.3.0 实测）。
		byCloud := make(map[string][]panSouItem, 8)
		for cloud, list := range out.Data.MergedByType {
			cn := panSouCloudCN[cloud]
			if cn == "" {
				cn = cloud
			}
			for _, it := range list {
				if strings.TrimSpace(it.URL) == "" {
					continue
				}
				if panSouIsEbook(it.Note) {
					continue // 电子书/文档：影视库用不上，白白占屏
				}
				byCloud[cloud] = append(byCloud[cloud], panSouItem{
					Cloud: cloud, CloudCN: cn,
					Note: strings.TrimSpace(it.Note), URL: it.URL, Password: it.Password,
					Link:     mergeSharePwd(it.URL, it.Password),
					Datetime: it.Datetime,
				})
			}
		}
		total := 0
		lim := panSouCfg.Limit
		if lim <= 0 {
			lim = 40
		}
		counts := make(map[string]int, len(byCloud))
		for cloud, list := range byCloud {
			total += len(list)
			// 每类内部按时间倒序（datetime 是 "2026-10-25 12:00:00" 这种可比较字符串）
			sort.SliceStable(list, func(i, j int) bool { return list[i].Datetime > list[j].Datetime })
			if len(list) > lim {
				list = list[:lim]
			}
			counts[cloud] = len(list)
			items = append(items, list...)
		}
		// 整体再按时间倒序，前端「全部」看着才顺
		sort.SliceStable(items, func(i, j int) bool { return items[i].Datetime > items[j].Datetime })

		writeJSON(w, map[string]any{
			"ok": true, "kw": kw, "total": total, "shown": len(items),
			"counts": counts, "items": items,
		})
	}
}

// apiPanSouConfig GET/POST /api/pansou/config
func apiPanSouConfig() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"ok": true, "config": panSouCfg})
		case http.MethodPost:
			var c panSouConfig
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "请求格式错误"})
				return
			}
			c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
			if c.URL != "" {
				u, err := url.Parse(c.URL)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					writeJSON(w, map[string]any{"ok": false, "error": "地址要形如 http://ip:3005"})
					return
				}
			}
			if c.Limit <= 0 || c.Limit > 120 {
				c.Limit = 40
			}
			if len(c.Clouds) == 0 {
				c.Clouds = defaultPanSouClouds
			}
			panSouCfg = c
			if err := savePanSouConfig(); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "保存失败：" + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "config": panSouCfg})
		default:
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		}
	}
}

// panSouIsEbook 判断标题是不是电子书/文档（搜剧名时 pansou 会混进一堆小说 txt）。
func panSouIsEbook(note string) bool {
	n := strings.ToLower(note)
	for _, ext := range []string{".txt", ".epub", ".mobi", ".azw3", ".azw", ".pdf", ".doc", ".docx"} {
		if strings.Contains(n, ext) {
			return true
		}
	}
	return false
}
