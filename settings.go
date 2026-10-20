package main

// ---------------------------------------------------------------------------
// 全局播放设置（网页可改，落盘在数据目录 settings.json）
//
// 与 strm_config.json 分开：那份管「.strm 怎么生成」，这份管「播放怎么走」。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// AppSettings 全局播放设置。
type AppSettings struct {
	// QuarkTV302 夸克 TV 302 总开关（§23.47 定稿的「一个开关」设计）：
	//
	//	true  = 全站夸克走 TV 302 直链：播放时把分享文件转存到自己盘临时目录
	//	        （PC Cookie），再用夸克 TV 接口取免认证直链，客户端直连 CDN，
	//	        数据不经过本机 —— 云服务器部署必须开这个（流量贵）。
	//	false = 全站夸克走本机代理（现状）：分享直链绑登录会话，只能中转，
	//	        数据过本机 —— 家用盒子部署用这个（省事，且 TV 设备无名额压力）。
	//
	// 默认 false：保持现状零行为变化，要用户主动开。
	// 切换立即生效（存量 .strm 双向兼容，一个都不用重生成）：
	//   开 → /play 内部转 302、/direct 正常 302；清晰度只走 TV 转码流
	//        （原画直链单连接限速 ~100KB/s 是死路，画质选项已下架，§23.59）；
	//   关 → /direct 内部转 /play（中转接手）、/play 走中转。
	QuarkTV302 bool `json:"quark_tv_302"`
}

var (
	appSettingsMu   sync.RWMutex
	appSettings     = AppSettings{}
	appSettingsPath = "/data/settings.json"
)

// loadAppSettings 读设置；文件不存在时落盘一份默认值（全关），方便用户直接改。
func loadAppSettings(path string) {
	appSettingsPath = path
	b, err := os.ReadFile(path)
	if err != nil {
		_ = saveAppSettings()
		return
	}
	var s AppSettings
	if json.Unmarshal(b, &s) == nil {
		appSettingsMu.Lock()
		appSettings = s
		appSettingsMu.Unlock()
	}
}

func saveAppSettings() error {
	appSettingsMu.RLock()
	b, _ := json.MarshalIndent(appSettings, "", "  ")
	appSettingsMu.RUnlock()
	return os.WriteFile(appSettingsPath, b, 0644)
}

// quarkTV302Enabled 读夸克 TV 302 开关（播放链路各分叉点共用）。
func quarkTV302Enabled() bool {
	appSettingsMu.RLock()
	defer appSettingsMu.RUnlock()
	return appSettings.QuarkTV302
}

// setQuarkTV302 改开关并落盘；返回改动后的值。
// 播放直链缓存一并清掉：开关切过之后，缓存里旧的取流方式（中转直链 /
// TV 直链）不能再复用，否则要等 25 分钟 TL 自然过期才切得过来。
func setQuarkTV302(on bool) bool {
	appSettingsMu.Lock()
	appSettings.QuarkTV302 = on
	appSettingsMu.Unlock()
	if err := saveAppSettings(); err != nil {
		log.Printf("[设置] 保存失败: %v", err)
	}
	dropAllPlayTargets()
	return on
}

// apiSettings 读取/修改全局播放设置。
//
//	GET  /api/settings           → {ok, settings:{...}}
//	POST /api/settings           → {ok, settings:{...}}（改哪些传哪些）
func apiSettings() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			appSettingsMu.RLock()
			out := appSettings
			appSettingsMu.RUnlock()
			writeJSON(w, map[string]interface{}{"ok": true, "settings": out})
		case http.MethodPost:
			// 指针字段：只覆盖本次传了的开关，没传的原样不动。
			// （曾经是全量覆盖：UI 只传 quark_tv_302，将来多一个字段就会
			// 把没传的悄悄清零——2026-10-20 差点让用户设置神秘失踪。）
			var req struct {
				QuarkTV302 *bool `json:"quark_tv_302"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
				return
			}
			appSettingsMu.Lock()
			if req.QuarkTV302 != nil {
				appSettings.QuarkTV302 = *req.QuarkTV302
			}
			appSettingsMu.Unlock()
			if err := saveAppSettings(); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "保存失败: " + err.Error()})
				return
			}
			dropAllPlayTargets() // 切换立即生效，不让旧缓存拖 25 分钟
			appSettingsMu.RLock()
			out := appSettings
			appSettingsMu.RUnlock()
			writeJSON(w, map[string]interface{}{"ok": true, "settings": out})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// settingsPath 与条目库同目录（data 目录）。
func settingsPath(dataFile string) string {
	return filepath.Join(filepath.Dir(dataFile), "settings.json")
}
