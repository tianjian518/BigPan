package main

import "strings"

// ---------------------------------------------------------------------------
// 网盘接入层（只做「辅路」：分享链接 → 虚拟盘 → 不占空间）
//
// 设计参照巨库的 CloudProvider 接口，但砍掉了主路相关的方法
// （列自己网盘 / 绑目录 / 同步），只保留辅路必需的两件事：
//   ① ListShareFiles  丢分享链接 → 列出里面的内容
//   ② ResolvePlay     丢分享链接 → 拿到可播放的直链
//
// 后续接入新网盘 = 新建一个文件实现本接口 + 在 init() 里注册一行。
// ---------------------------------------------------------------------------

// ShareInfo 一次分享解析的结果。
type ShareInfo struct {
	Provider string      `json:"provider"`
	Title    string      `json:"title"`
	ShareID  string      `json:"pwd_id,omitempty"`   // 分享标识（夸克 pwd_id / 115 share_code）
	Passcode string      `json:"passcode,omitempty"` // 提取码
	Files    []ShareFile `json:"files"`
	Logged   bool        `json:"logged"` // 取流所需的账号是否已就绪
	// Pending / Scanned 用于「大目录分段扫」：豆包自家盘限速 0.6~2 秒/请求，
	// 几百个目录同步扫必然撞反向代理超时。这里每段扫 45 秒就返回，
	// 带上 pending=true 和已扫目录数，前端自动续调一次接着扫（断点已落盘）。
	Pending bool `json:"pending,omitempty"`
	Scanned int  `json:"scanned,omitempty"`
	// ScanKey + Resume 给后台续扫用（不参与 JSON）：
	// 解析接口发现 pending 就把 Resume 丢进 goroutine 扫到完，前端只轮询进度，
	// 关掉页面也不影响；ScanKey 同时用来做任务去重（同一个目录不重复开）。
	ScanKey string                                      `json:"-"`
	Resume  func() (string, []ShareFile, bool, error)   `json:"-"`
}

// PlayTarget 播放地址及取流所需的凭据。
// 不同网盘要求不同的 UA / Cookie，所以这里一并返回。
type PlayTarget struct {
	URL    string // 直链
	UA     string // 取流时使用的 User-Agent
	Cookie string // 取流时使用的 Cookie（可为空）
}

// PanProvider 一个网盘的分享直连实现。
type PanProvider interface {
	// Name 网盘标识，如 "quark" / "115"
	Name() string
	// MatchLink 判断这个链接是否属于本网盘
	MatchLink(link string) bool
	// ListShareFiles 解析分享链接，返回其中的文件（通常不需要登录）
	ListShareFiles(link string) (ShareInfo, error)
	// RefreshShare 追更刷新：按分享标识（不再需要原始链接）重新扫描整棵
	// 分享目录树，返回最新文件清单。增量入库由调用方按路径去重。
	RefreshShare(shareID, passcode string) (title string, files []ShareFile, err error)
	// Wants302 该网盘的直链能否被客户端直连（不绑会话 Cookie）。
	// true：WebDAV 读该网盘文件时直接 302 跳直链，数据不经过本机；
	// false：直链绑会话（如夸克），必须走本地多线程加速中转。
	Wants302() bool
	// ResolvePlay 取某个条目的播放直链（可能需要登录）
	ResolvePlay(item *Item) (PlayTarget, error)
}

var panProviders []PanProvider

// registerProvider 由各网盘实现文件在 init() 中调用。
func registerProvider(p PanProvider) {
	panProviders = append(panProviders, p)
}

// findProvider 根据链接自动识别网盘。
func findProvider(link string) PanProvider {
	for _, p := range panProviders {
		if p.MatchLink(link) {
			return p
		}
	}
	return nil
}

// providerByName 按名称取网盘实现。
func providerByName(name string) PanProvider {
	for _, p := range panProviders {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// providerList 返回已接入的网盘名（用于界面展示）。
func providerList() []string {
	out := make([]string, 0, len(panProviders))
	for _, p := range panProviders {
		out = append(out, p.Name())
	}
	return out
}

// providerCN 网盘的中文名 —— 虚拟盘顶层目录用它（如 /夸克/剧名/...）。
func providerCN(p string) string {
	switch p {
	case "quark":
		return "夸克"
	case "115":
		return "115网盘"
	case "tianyi", "cloud189":
		return "天翼"
	case "aliyun":
		return "阿里"
	case "xunlei":
		return "迅雷"
	case "baidu":
		return "百度"
	case "mobile139":
		return "移动"
	case "uc":
		return "UC"
	}
	// 豆包多账号：doubao1..doubaoN 各算一个独立网盘
	if strings.HasPrefix(p, "doubao") {
		return "豆包" + strings.TrimPrefix(p, "doubao")
	}
	return p
}
