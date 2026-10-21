package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 天翼云盘 provider
//
// 与 115/夸克的差异：
//   ① 解析分享（GetShareInfo / 列目录）不需要登录，游客态就能扫整棵树；
//   ② 取流（GetFileDownload）需要 token，但拿到的是「不跟随重定向的
//      Location 直链」，不绑会话Cookie —— 所以 Wants302 = true，
//      WebDAV 读文件直接跳直链，不耗本机流量；
//   ③ 追更凭 shareCode + 访问码重扫（不需要登录）。
// ---------------------------------------------------------------------------

type pan189Provider struct{}

func init() { registerProvider(pan189Provider{}) }

func (pan189Provider) Name() string { return "tianyi" }

func (pan189Provider) MatchLink(link string) bool {
	return tianyiShareRe.MatchString(link)
}

func (pan189Provider) ListShareFiles(link string) (ShareInfo, error) {
	code, pwd, err := parseTianyiLink(link)
	if err != nil {
		return ShareInfo{}, err
	}
	title, files, err := scanTianyiShare(code, pwd)
	if err != nil {
		return ShareInfo{}, err
	}
	return ShareInfo{
		Provider: "tianyi",
		Title:    title,
		ShareID:  code,
		Passcode: pwd,
		Files:    files,
		Logged:   tianyiCredOf(globalAccounts) != nil,
	}, nil
}

// Wants302 天翼直链（Location）不绑会话，客户端可直连 —— 与 139 一样
// 走 302，数据不经过本机。
func (pan189Provider) Wants302() bool { return true }

// RefreshShare 追更刷新：凭 shareCode + 访问码重扫目录树（不需要登录）。
// 数字 shareID（订阅号收录的分享）不走这里 —— 它们的追更由订阅号自己的
// 收录循环负责（见 subs.go），通用追更定时器不碰。
func (pan189Provider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	if isAllDigits(shareID) && passcode == "" {
		return "", nil, fmt.Errorf("订阅号收录的分享请在天翼「订阅号」面板里刷新")
	}
	return scanTianyiShare(shareID, passcode)
}

func (pan189Provider) ResolvePlay(item *Item) (PlayTarget, error) {
	cred := tianyiCredOf(globalAccounts)
	if cred == nil {
		return PlayTarget{}, fmt.Errorf("还没有导入天翼凭证（播放需要登录，解析分享不需要）")
	}
	shareId, err := tianyiShareIdOf(item)
	if err != nil {
		return PlayTarget{}, err
	}
	u, err := tianyiDownloadURL(cred, shareId, item.FileID)
	if err != nil {
		// 兜底：VLC 播放地址（m3u8 转码流）——原画直链拿不到时至少能播
		if u2, err2 := tianyiVLCPlayURL(cred, shareId, item.FileID); err2 == nil && u2 != "" {
			return PlayTarget{URL: u2}, nil
		}
		return PlayTarget{}, err
	}
	return PlayTarget{URL: u}, nil
}

// tianyiShareIdOf 取条目的数字 shareId：入库时带了就直接用（省一次接口），
// 没带（2026-10 之前入库的老数据）就用 shareCode 现换。
func tianyiShareIdOf(item *Item) (int64, error) {
	if item.ShareNumID != "" {
		if id, err := strconv.ParseInt(strings.TrimSpace(item.ShareNumID), 10, 64); err == nil && id > 0 {
			return id, nil
		}
	}
	if item.ShareID == "" {
		return 0, fmt.Errorf("条目缺少天翼分享标识，请重新导入该分享")
	}
	meta, err := tianyiGetShare(item.ShareID, item.Passcode)
	if err != nil {
		return 0, err
	}
	return meta.ShareId, nil
}

// isAllDigits 判断是否纯数字串（订阅号分享的 ShareID 是数字 shareId）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
