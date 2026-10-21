// 天翼接入实测工具（开发用，不参与正式构建）。
//
//	go run ./cmd/t189smoke parse "https://cloud.189.cn/web/share?code=xxxx"
//	go run ./cmd/t189smoke parse "https://cloud.189.cn/web/share?code=xxxx 访问码"
//	go run ./cmd/t189smoke login
//
// parse：不需要登录，直接扫整棵分享树打印视频文件 —— 验证链接解析/列目录/
// 分页/视频过滤整条链路。
// login：完整走一遍扫码登录（二维码写 /tmp/tianyi_qr.png，同时打印 URL），
// 确认后打印 token 与过期时间，并调 GetUserInfo 验证。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	tclient "bigpan/internal/tianyi/client"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: t189smoke parse <分享链接[+访问码]> | login")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "parse":
		parseMode(strings.Join(os.Args[2:], " "))
	case "login":
		loginMode()
	default:
		fmt.Println("未知模式:", os.Args[1])
		os.Exit(1)
	}
}

func parseMode(raw string) {
	code, pwd := splitCodePwd(raw)
	fmt.Printf("shareCode=%q accessCode=%q\n", code, pwd)

	// GetShareInfo（匿名）
	cl := tclient.New()
	resp, err := cl.GetShareInfo(context.Background(), code, func(req *tclient.GetShareInfoRequest) {
		req.AccessCode = pwd
	})
	if err != nil {
		fmt.Println("GetShareInfo 失败:", err)
		return
	}
	fmt.Printf("res_code=%d shareId=%d fileId=%q fileName=%q isFolder=%v shareMode=%d accessCode=%q needAccessCode=%d\n",
		resp.ResCode, resp.ShareId, string(resp.FileId), resp.FileName, resp.IsFolder, resp.ShareMode, resp.AccessCode, resp.NeedAccessCode)

	if !resp.IsFolder {
		fmt.Println("单文件分享，结束")
		return
	}

	// BFS 列目录（匿名），统计文件
	type job struct{ fid, prefix string }
	queue := []job{{fid: string(resp.FileId)}}
	seen := map[string]bool{string(resp.FileId): true}
	var videos, dirs, others int
	for len(queue) > 0 {
		j := queue[0]
		queue = queue[1:]
		page := 1
		for page <= 50 {
			r, e := cl.ListShareDir(context.Background(), resp.ShareId, tclient.String(j.fid), func(req *tclient.ListShareFileRequest) {
				req.PageNum, req.PageSize = page, 100
				req.AccessCode = pwd
				req.ShareMode = resp.ShareMode
				req.IconOption = 5
			})
			if e != nil {
				fmt.Println("列目录失败:", e)
				return
			}
			if r.ResCode != 0 {
				fmt.Printf("列目录业务错误 res_code=%d msg=%s\n", r.ResCode, r.ResMessage)
				return
			}
			for _, f := range r.FileListAO.FolderList {
				dirs++
				id := string(f.Id)
				if !seen[id] && len(seen) < 200 {
					seen[id] = true
					p := f.Name
					if j.prefix != "" {
						p = j.prefix + "/" + f.Name
					}
					queue = append(queue, job{fid: id, prefix: p})
				}
			}
			for _, f := range r.FileListAO.FileList {
				n := strings.ToLower(f.Name)
				if strings.HasSuffix(n, ".mp4") || strings.HasSuffix(n, ".mkv") ||
					strings.HasSuffix(n, ".avi") || strings.HasSuffix(n, ".ts") ||
					strings.HasSuffix(n, ".mov") || strings.HasSuffix(n, ".wmv") ||
					strings.HasSuffix(n, ".flv") || strings.HasSuffix(n, ".m3u8") ||
					strings.HasSuffix(n, ".rmvb") || strings.HasSuffix(n, ".webm") {
					videos++
					p := f.Name
					if j.prefix != "" {
						p = j.prefix + "/" + f.Name
					}
					fmt.Printf("  [视频] %s  (%d 字节, id=%s)\n", p, f.Size, string(f.Id))
				} else {
					others++
				}
			}
			got := int64(len(r.FileListAO.FileList) + len(r.FileListAO.FolderList))
			if got == 0 || got < 100 || int64(dirs+len(r.FileListAO.FileList)) >= r.FileListAO.Count {
				page = 99
			}
			page++
		}
	}
	fmt.Printf("合计：视频 %d，目录 %d，其他文件 %d\n", videos, dirs, others)
}

func loginMode() {
	info, err := tclient.LoginInit()
	if err != nil {
		fmt.Println("LoginInit 失败:", err)
		return
	}
	qrURL := "https://cloud.189.cn/api/portal/loginUrl.action?redirectURL=https://cloud.189.cn&uuid=" + info.UUID
	fmt.Println("二维码 URL:", qrURL)
	if err := qrcode.WriteFile(qrURL, qrcode.Medium, 256, "/tmp/tianyi_qr.png"); err != nil {
		fmt.Println("写二维码图片失败:", err)
	} else {
		fmt.Println("二维码图片: /tmp/tianyi_qr.png（用天翼 App 扫这个文件里的码）")
	}
	fmt.Println("等待扫码（5 分钟超时）…")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		resp, err := tclient.LoginQuery(info.UUID)
		if err != nil {
			if strings.Contains(err.Error(), "QrCodeRollLoginFail") {
				continue // 还没扫
			}
			fmt.Println("轮询错误:", err)
			continue
		}
		if resp == nil || resp.AccessToken == "" {
			continue
		}
		fmt.Printf("登录成功！accessToken=%s expiresIn=%d\n", resp.AccessToken, resp.ExpiresIn)
		// 换算过期时间（>1e10 视为毫秒时间戳，否则按剩余秒数）
		now := time.Now().UnixMilli()
		var exp int64
		if resp.ExpiresIn > 1e10 {
			exp = resp.ExpiresIn
		} else {
			exp = now + resp.ExpiresIn*1000
		}
		fmt.Printf("过期时间: %s\n", time.UnixMilli(exp).Format("2006-01-02 15:04:05"))
		// 验证 token
		cl := tclient.New().WithToken(tclient.NewAuthToken(resp.AccessToken, exp))
		u, err := cl.GetUserInfo(context.Background())
		if err != nil {
			fmt.Println("GetUserInfo 失败:", err)
			return
		}
		fmt.Printf("GetUserInfo: res_code=%d loginName=%s capacity=%d available=%d\n",
			u.ResCode, u.LoginName, u.Capacity, u.Available)
		return
	}
	fmt.Println("超时未扫码")
}

// splitCodePwd 从命令行参数里拆 shareCode 和访问码：
// "链接 访问码" 或 "链接"（访问码随链接 ?pwd= 给也行）。
func splitCodePwd(raw string) (code, pwd string) {
	raw = strings.TrimSpace(raw)
	parts := strings.Fields(raw)
	for _, p := range parts {
		if strings.Contains(p, "189.cn") {
			if i := strings.Index(p, "code="); i >= 0 {
				code = p[i+5:]
				if j := strings.IndexAny(code, "&"); j >= 0 {
					code = code[:j]
				}
			}
			if i := strings.Index(strings.ToLower(p), "pwd="); i >= 0 {
				pwd = p[i+4:]
				if j := strings.IndexAny(pwd, "&"); j >= 0 {
					pwd = pwd[:j]
				}
			}
			continue
		}
		if len(p) == 4 {
			pwd = p // 独立 4 位 = 访问码
		}
	}
	// 兜底：整串里找 code=
	if code == "" {
		if i := strings.Index(raw, "code="); i >= 0 {
			code = raw[i+5:]
			if j := strings.IndexAny(code, "& "); j >= 0 {
				code = code[:j]
			}
		}
	}
	return code, pwd
}
