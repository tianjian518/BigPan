package main

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 已接入的网盘实现
//
// 加新网盘 = 在下面照葫芦画瓢加一个 struct + init() 注册。
// ---------------------------------------------------------------------------

// 全局账号库引用（由 main 注入，供各 provider 取凭证）
var globalAccounts *AccountStore

// 全局条目库引用（由 main 注入，播放链路回写条目字段用，
// 如夸克 TV 302 转存后的 fid 缓存 Item.TVFID、刷新后的分享令牌）
var globalStore *Store

// ---- 夸克 ----

type quarkProvider struct{}

func init() { registerProvider(quarkProvider{}) }

func (quarkProvider) Name() string { return "quark" }

func (quarkProvider) MatchLink(link string) bool {
	return strings.Contains(link, "quark.cn") || quarkShareRe.MatchString(link)
}

func (quarkProvider) ListShareFiles(link string) (ShareInfo, error) {
	title, pwdID, passcode, files, err := resolveQuarkShare(link)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: "quark",
		Title:    title,
		ShareID:  pwdID,
		Passcode: passcode,
		Files:    files,
		Logged:   quarkClient(globalAccounts) != nil,
	}, nil
}

// RefreshShare 追更刷新：夸克只凭分享 ID + 提取码即可重扫整棵目录树。
func (quarkProvider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	return scanQuarkShare(shareID, passcode)
}

// Wants302 夸克直链绑登录会话，302 会被 412 拒绝 —— 默认必须本地中转加速。
// 但用户打开「夸克 TV 302」开关后，取链走的是 TV 免认证直链（见 ResolvePlay），
// 那时就可以 302 了。WebDAV 侧（main.go 的 /dav 分支）读的就是这个方法，
// 这里改动态，那边零改动自动跟随。
func (quarkProvider) Wants302() bool { return quarkTV302Enabled() }

func (quarkProvider) ResolvePlay(item *Item) (PlayTarget, error) {
	// 开关打开：转存到临时目录 → TV 接口取免认证直链（UA/Cookie 全空，可 302）。
	// 开关关闭：现状 —— 分享直链绑登录会话，本机中转。
	if quarkTV302Enabled() {
		return quarkTVPlayTarget(item)
	}
	q := quarkClient(globalAccounts)
	if q == nil {
		return PlayTarget{}, fmt.Errorf("还没有导入夸克凭证（取流需要登录，解析分享不需要）")
	}
	st, err := q.ShareToken(item.ShareID, item.Passcode)
	if err != nil {
		return PlayTarget{}, fmt.Errorf("分享已失效？%w", err)
	}
	u, err := q.ShareDownloadURL(item.ShareID, st, item.FileID)
	if err != nil {
		return PlayTarget{}, err
	}
	return PlayTarget{URL: u, UA: uaQuark, Cookie: q.Cookies()}, nil
}

// ---- 115 ----

type pan115Provider struct{}

func init() { registerProvider(pan115Provider{}) }

func (pan115Provider) Name() string { return "115" }

func (pan115Provider) MatchLink(link string) bool {
	return shareHostRe.MatchString(link) || (!strings.Contains(link, "/") && shareCodeOnlyRe.MatchString(link))
}

func (pan115Provider) ListShareFiles(link string) (ShareInfo, error) {
	cookie := ""
	if globalAccounts != nil {
		if acc := globalAccounts.Get("115"); acc != nil {
			cookie = acc.Cookie
		}
	}
	title, files, err := resolveShare(link, cookie)
	if err != nil {
		return ShareInfo{}, err
	}
	code, pwd, _ := parseShareLink(link)
	return ShareInfo{
		Provider: "115",
		Title:    title,
		ShareID:  code,
		Passcode: pwd,
		Files:    files,
		Logged:   cookie != "",
	}, nil
}

// Wants302 115 直链同样绑 Cookie，走本地中转。
func (pan115Provider) Wants302() bool { return false }

// RefreshShare 追更刷新：115 凭分享码 + 提取码重扫目录树（需已导入 Cookie）。
func (pan115Provider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	cookie := ""
	if globalAccounts != nil {
		if acc := globalAccounts.Get("115"); acc != nil {
			cookie = acc.Cookie
		}
	}
	return resolveShareCode(shareID, passcode, cookie)
}

func (pan115Provider) ResolvePlay(item *Item) (PlayTarget, error) {
	cookie := ""
	if globalAccounts != nil {
		if acc := globalAccounts.Get("115"); acc != nil {
			cookie = acc.Cookie
		}
	}
	u, err := shareFileURL(item.ShareID, item.Passcode, item.FileID, cookie)
	if err != nil {
		return PlayTarget{}, err
	}
	return PlayTarget{URL: u, UA: ua115Disk, Cookie: cookie}, nil
}
