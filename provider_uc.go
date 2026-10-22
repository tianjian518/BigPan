package main

// ---------------------------------------------------------------------------
// UC 网盘 provider
//
// 与夸克的关系：API 同源（同族后端），差异是域名 pc-api.uc.cn、参数 pr=UCBrowser，
// 以及**分享直链带 CDN 回调**（游客拿到链也拉不到流）——所以播放一律走
// 「转存 → TV 接口免认证直链 → 302」，不碰分享直链。
//
// 解析分享不需要登录；播放取流需要两样凭证：
//   Provider "uc"    → Cookie（PC 版，负责转存）
//   Provider "uc_tv" → 扫码（TV 版，负责取 302 直链）
// ---------------------------------------------------------------------------

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var ucShareRe = regexp.MustCompile(`(?i)(?:drive|fast|pan)\.uc\.cn/s/([A-Za-z0-9]+)`)

// parseUCLink 从分享链接里提取 pwd_id 与提取码。
func parseUCLink(raw string) (pwdID, passcode string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("请粘贴分享链接")
	}
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
	if m := ucShareRe.FindStringSubmatch(raw); m != nil {
		return m[1], passcode, nil
	}
	return "", "", fmt.Errorf("这不是 UC 网盘分享链接")
}

// scanUCShare 扫描整棵 UC 分享目录树（游客态，不需要账号）。
func scanUCShare(pwdID, passcode string) (title string, files []ShareFile, err error) {
	u := newUC("")
	stoken, err := u.ShareToken(pwdID, passcode)
	if err != nil {
		return "", nil, err
	}
	title = pwdID
	if root, e := u.ShareDetail(pwdID, stoken, ucRootFID); e == nil && len(root) > 0 {
		// 单个顶层目录时用它的名字当剧名（UC 分享多是「剧名(年份)」一层壳）
		if len(root) == 1 && root[0].IsDir {
			title = root[0].Name
		} else {
			title = root[0].Name
		}
	}

	type job struct {
		fid, prefix string
	}
	queue := []job{{}}
	seen := map[string]bool{"": true}
	for len(queue) > 0 && len(files) < 500 {
		j := queue[0]
		queue = queue[1:]
		list, e := u.ShareDetail(pwdID, stoken, j.fid)
		if e != nil {
			continue
		}
		for _, f := range list {
			p := f.Name
			if j.prefix != "" {
				p = j.prefix + "/" + f.Name
			}
			if f.IsDir {
				if !seen[f.FID] && len(seen) < 200 {
					seen[f.FID] = true
					queue = append(queue, job{fid: f.FID, prefix: p})
				}
				continue
			}
			if isVideo(f.Name) {
				files = append(files, ShareFile{
					ID: f.FID, Name: f.Name, Size: f.Size, Path: p,
					Code: pwdID, Pwd: passcode, FidToken: f.ShareFidToken,
				})
			}
		}
	}
	return title, files, nil
}

// ---- provider ----

type ucProvider struct{}

func init() { registerProvider(ucProvider{}) }

func (ucProvider) Name() string { return "uc" }

func (ucProvider) MatchLink(link string) bool { return ucShareRe.MatchString(link) }

func (ucProvider) ListShareFiles(link string) (ShareInfo, error) {
	pwdID, passcode, err := parseUCLink(link)
	if err != nil {
		return ShareInfo{}, err
	}
	title, files, err := scanUCShare(pwdID, passcode)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: "uc",
		Title:    title,
		ShareID:  pwdID,
		Passcode: passcode,
		Files:    files,
		Logged:   ucClient(globalAccounts) != nil,
	}, nil
}

func (ucProvider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	return scanUCShare(shareID, passcode)
}

// Wants302 UC 走「转存 → TV 接口」，返回的直链不绑 Cookie，可直接 302。
func (ucProvider) Wants302() bool { return true }

func (ucProvider) ResolvePlay(item *Item) (PlayTarget, error) {
	return ucTVPlayTarget(item)
}

// ---- 客户端工厂 ----

var (
	ucMu    sync.Mutex
	ucCur   *UC
	ucCurCK string

	ucTVMu    sync.Mutex
	ucTVCur   *QuarkTV
	ucTVCache string
)

// ucClient 取当前配置的 UC PC 客户端（未导入 Cookie 返回 nil）。
func ucClient(as *AccountStore) *UC {
	if as == nil {
		return nil
	}
	acc := as.Get("uc")
	if acc == nil || acc.Cookie == "" {
		return nil
	}
	ucMu.Lock()
	defer ucMu.Unlock()
	if ucCur != nil && ucCurCK == acc.Cookie {
		return ucCur
	}
	ucCur = newUC(acc.Cookie)
	ucCurCK = acc.Cookie
	return ucCur
}

// ucTVClient 取当前配置的 UC TV 客户端（未扫码返回 nil）。
// 缓存的 key 用「设备号|access|refresh」，token 刷新后自动重建。
func ucTVClient(as *AccountStore) *QuarkTV {
	if as == nil {
		return nil
	}
	acc := as.Get("uc_tv")
	if acc == nil || acc.Cookie == "" {
		return nil
	}
	key := acc.DeviceID + "|" + acc.AccessToken + "|" + acc.Cookie
	ucTVMu.Lock()
	defer ucTVMu.Unlock()
	if ucTVCur != nil && ucTVCache == key {
		return ucTVCur
	}
	tv := NewUCTV(acc.DeviceID, acc.AccessToken, acc.Cookie)
	tv.onRefresh = func(access, refresh string) {
		// 在旧记录上原地更新，保留 Label / LoginAt。
		old := *acc
		old.AccessToken = access
		old.Cookie = refresh
		as.Put(old)
		ucTVMu.Lock()
		ucTVCache = acc.DeviceID + "|" + access + "|" + refresh
		ucTVMu.Unlock()
	}
	ucTVCur = tv
	ucTVCache = key
	return tv
}

// ucStableDeviceID 取一个稳定的 UC TV 设备号（同夸克策略：一个部署占一个名额）。
func ucStableDeviceID(as *AccountStore) string {
	if a := as.Get("uc_tv"); a != nil && a.DeviceID != "" {
		return a.DeviceID
	}
	return qtvStableDeviceIDFor(as, "uc_tv", "uctv_device_id")
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
