package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// 夸克网盘接口
// ---------------------------------------------------------------------------

// apiQuarkImport 导入夸克凭证：POST /api/quark/import {"cookie":"..."}
func apiQuarkImport(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		// 去掉可能的换行/引号
		ck = strings.ReplaceAll(ck, "\n", "")
		ck = strings.ReplaceAll(ck, "\r", "")
		ck = strings.Trim(ck, "\"")

		q := newQuark(ck)
		member, err := q.Check()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		installAccount(as, Account{
			Provider: "quark",
			Label:    "夸克网盘（" + member + "）",
			Cookie:   ck,
		})
		writeJSON(w, map[string]interface{}{"ok": true, "member": member})
	}
}

// apiQuarkList 列目录：GET /api/quark/list?fid=xxx（空表示根目录）
func apiQuarkList(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := quarkClient(as)
		if q == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "还没有导入夸克凭证"})
			return
		}
		fid := r.URL.Query().Get("fid")
		files, err := q.ListDir(fid)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		type row struct {
			FID   string `json:"fid"`
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			IsDir bool   `json:"is_dir"`
		}
		out := make([]row, 0, len(files))
		for _, f := range files {
			out = append(out, row{f.FID, f.Name, f.Size, f.IsDir})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "files": out})
	}
}

// apiQuarkAdd 把文件加入放映列表：POST /api/quark/add {"fid":"..","name":"..","size":123}
func apiQuarkAdd(store *Store, as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if quarkClient(as) == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "还没有导入夸克凭证"})
			return
		}
		var req struct {
			FID  string `json:"fid"`
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数错误"})
			return
		}
		title := strings.TrimSuffix(req.Name, extOf(req.Name))
		if title == "" {
			title = req.Name
		}
		store.Add(Item{
			Title:    title,
			QuarkFID: req.FID,
			Provider: "quark",
			UA:       uaQuark,
			Size:     req.Size,
		})
		writeJSON(w, map[string]interface{}{"ok": true})
	}
}

func extOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i:]
}

// ---------------------------------------------------------------------------
// 夸克分享链接解析（虚拟网盘入口）
// ---------------------------------------------------------------------------

var quarkShareRe = regexp.MustCompile(`(?i)pan\.quark\.cn/s/([A-Za-z0-9]+)`)

// parseQuarkLink 从分享链接里提取 pwd_id（分享码）与提取码。
func parseQuarkLink(raw string) (pwdID, passcode string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("请粘贴分享链接")
	}
	// 提取码：?pwd=xxxx 或 密码：xxxx
	if u, e := url.Parse(raw); e == nil {
		passcode = u.Query().Get("pwd")
		if passcode == "" {
			passcode = u.Query().Get("password")
		}
	}
	if passcode == "" {
		re := regexp.MustCompile(`(?:提取码|访问码|密码|pwd|password)\s*[:：]?\s*([A-Za-z0-9]{4})`)
		if m := re.FindStringSubmatch(raw); m != nil {
			passcode = m[1]
		}
	}
	if m := quarkShareRe.FindStringSubmatch(raw); m != nil {
		return m[1], passcode, nil
	}
	return "", "", fmt.Errorf("这不是夸克分享链接")
}

// resolveQuarkShare 解析夸克分享，展开一层目录，返回视频文件。
func resolveQuarkShare(rawLink string) (title, pwdID, passcode string, files []ShareFile, err error) {
	pwdID, passcode, err = parseQuarkLink(rawLink)
	if err != nil {
		return "", "", "", nil, err
	}
	title, files, err = scanQuarkShare(pwdID, passcode)
	return title, pwdID, passcode, files, err
}

// scanQuarkShare 按分享标识扫描整棵分享目录树 —— 首次解析与追更刷新共用。
func scanQuarkShare(pwdID, passcode string) (title string, files []ShareFile, err error) {
	// 解析不需要登录
	q := newQuark("")
	stoken, err := q.ShareToken(pwdID, passcode)
	if err != nil {
		return "", nil, err
	}
	title = q.ShareTitle(pwdID, passcode)
	if title == "" {
		title = pwdID
	}

	// 递归展开整棵分享目录树（BFS），保留完整相对路径。
	// 上限：目录 200 个 / 文件 500 个，防止超大分享把解析拖死。
	type dirJob struct {
		fid, prefix string
	}
	queue := []dirJob{{}}
	seenDirs := map[string]bool{"": true}
	for len(queue) > 0 && len(files) < 500 {
		job := queue[0]
		queue = queue[1:]
		list, err := q.ShareList(pwdID, stoken, job.fid)
		if err != nil {
			continue
		}
		for _, f := range list {
			p := f.Name
			if job.prefix != "" {
				p = job.prefix + "/" + f.Name
			}
			if f.IsDir {
				if !seenDirs[f.FID] && len(seenDirs) < 200 {
					seenDirs[f.FID] = true
					queue = append(queue, dirJob{fid: f.FID, prefix: p})
				}
				continue
			}
		if isVideo(f.Name) {
			files = append(files, ShareFile{
				ID: f.FID, Name: f.Name, Size: f.Size, Path: p,
				Code: pwdID, Pwd: passcode,
				FidToken: f.ShareFidToken, // 转存要用，随文件带回前端再入库
			})
		}
		}
	}
	if len(files) == 0 {
		// 分享被审核屏蔽是「链接还在、内容没了」的第三种状态：ShareToken
		// 照常成功、BFS 列表全空，只有 detail 元数据里的 partial_violation
		// 能识别（2026-10-20 美人余事故：12 个文件违规下架，巡检漏判）。
		// 拿不到元数据按非屏蔽处理——宁可漏判，不可误判。
		if meta, serr := q.ShareStatus(pwdID, stoken); serr == nil && meta.Blocked() {
			return title, nil, fmt.Errorf("分享内容被网盘审核屏蔽（%d 个文件违规下架），请更换分享源或删除该条目", meta.ViolationCnt)
		}
		return title, nil, fmt.Errorf("这个分享里没有找到视频文件")
	}
	return title, files, nil
}
