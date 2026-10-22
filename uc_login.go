package main

// ---------------------------------------------------------------------------
// UC 网盘的登录入口（两个凭证各一个）：
//   POST /api/login/uctv/start  → 取 UC TV 二维码（302 直链用）
//   GET  /api/login/uctv/poll   → 轮询（复用夸克的通用 poll 处理器）
//   POST /api/uc/import         → 导入 PC Cookie（转存用）
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"net/http"
	"strings"
)

// apiLoginStartUC 开始 UC TV 扫码：POST /api/login/uctv/start
func apiLoginStartUC(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		s, err := startQRUC(as)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":  true,
			"sid": s.id,
			"qr":  s.label,
			"ttl": 300,
		})
	}
}

// apiUCImport 导入 UC PC Cookie：POST /api/uc/import {"cookie":"..."}
func apiUCImport(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Cookie string `json:"cookie"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
			return
		}
		ck := strings.TrimSpace(req.Cookie)
		if ck == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "凭证不能为空"})
			return
		}
		ck = strings.ReplaceAll(ck, "\n", "")
		ck = strings.ReplaceAll(ck, "\r", "")
		ck = strings.Trim(ck, "\"")

		u := newUC(ck)
		member, err := u.Check()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		label := "UC网盘"
		if member != "" {
			label += "（" + member + "）"
		}
		installAccount(as, Account{Provider: "uc", Label: label, Cookie: ck})
		writeJSON(w, map[string]interface{}{"ok": true, "member": member})
	}
}
