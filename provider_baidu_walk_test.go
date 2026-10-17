package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestBaiduWalkThreeLayer 三层套娃分享回归测试（2026-10-17 用户实踩 bug）。
//
// 真实病例：https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=1016
// 结构为 分享根/「D - 示例剧 (2026)」/「【D】示例剧」/视频，
// 旧实现只展开一层，第二层仍是目录 → 报「没有找到可播放的视频文件」。
// 本测试用 httptest 假百度复刻该结构，锁死 BFS 递归修复。
func TestBaiduWalkThreeLayer(t *testing.T) {
	// 三层结构（复刻真实分享，含特殊字符 丨/-/【】/空格）。
	// wxlist 语义：root=1（dir=""）列分享根，返回分享目录自身这一项；
	// root=0 按 dir（服务端精确 path）列该目录内容。真实日志实拍：
	//   分享 "/剧集/更新/D - 示例剧 (2026)" 根目录 1 项（目录 1）。
	const shareRoot = "/剧集/更新"
	dir2 := shareRoot + "/D - 示例剧 (2026)"
	dir3 := dir2 + "/【D】示例剧"
	children := map[string][]baiduWxFile{
		// root=1：分享根本身的内容 = 一个目录（第二层入口）
		"": {
			{FsID: "1001", Path: dir2, ServerFilename: "D - 示例剧 (2026)", IsDir: "1", Size: "0"},
		},
		// 第二层：还是一个目录（第三层入口）
		dir2: {
			{FsID: "1002", Path: dir3, ServerFilename: "【D】示例剧", IsDir: "1", Size: "0"},
		},
		// 第三层：两个视频 + 一个字幕（非视频应被 isVideo 过滤）
		dir3: {
			{FsID: "2001", Path: dir3 + "/S01E01.2160p.mkv", ServerFilename: "S01E01.2160p.mkv", IsDir: "0", Size: "1234567890"},
			{FsID: "2002", Path: dir3 + "/S01E01.srt", ServerFilename: "S01E01.srt", IsDir: "0", Size: "20000"},
			{FsID: "2003", Path: dir3 + "/S01E02.2160p.mkv", ServerFilename: "S01E02.2160p.mkv", IsDir: "0", Size: "2234567890"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/share/verify") {
			w.Write([]byte(`{"errno":0,"randsk":"J%2FsVg8UW%2Byx%2Bp1ko1fOkd%2FNiz%2FDquKandkLEP11Ce%2Bs%3D"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/share/wxlist") {
			_ = r.ParseForm()
			dir := r.Form.Get("dir")
			title := shareRoot + "/D - 示例剧 (2026)"
			if dir != "" {
				title = dir
			}
			resp := baiduWxListResp{Errno: 0}
			resp.Data.Title = title
			resp.Data.UK = 1103481891240
			resp.Data.ShareID = 2787910643
			resp.Data.List = children[dir]
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	// 把接口地址指到假服务器（var 化就是为这个）
	oldVerify, oldWx := baiduShareVerifyURL, baiduWxListURL
	defer func() { baiduShareVerifyURL, baiduWxListURL = oldVerify, oldWx }()
	u, _ := url.Parse(srv.URL)
	baiduShareVerifyURL = srv.URL + "/share/verify?t=%s&surl=%s"
	baiduWxListURL = srv.URL + "/share/wxlist?channel=weixin&version=2.2.2&clienttype=25&web=1"
	_ = u

	surFull, pwd := "1AbCdEfGhIjKlMnOpQrStUv", "1016"
	randsk, err := baiduVerify(surFull, pwd)
	if err != nil {
		t.Fatalf("verify 失败: %v", err)
	}
	_, _, _, title, files, err := baiduWalk(surFull, randsk, pwd)
	if err != nil {
		t.Fatalf("baiduWalk 失败（三层套娃未修?）: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("应收集到 2 个视频（第三层），实际 %d 个", len(files))
	}
	wantPaths := map[string]bool{
		"D - 示例剧 (2026)/【D】示例剧/S01E01.2160p.mkv": true,
		"D - 示例剧 (2026)/【D】示例剧/S01E02.2160p.mkv": true,
	}
	for _, f := range files {
		if !wantPaths[f.Path] {
			t.Errorf("多余/错误路径: %q", f.Path)
		}
		if f.Size <= 0 {
			t.Errorf("size 解析失败: %s → %d", f.Name, f.Size)
		}
	}
	t.Logf("title=%q 文件数=%d", title, len(files))
	for _, f := range files {
		t.Logf("  [OK] %s (id=%s size=%d)", f.Path, f.ID, f.Size)
	}
}

// TestBaiduWalkFlat 平铺分享（根目录直接是视频）不应被回归破坏。
func TestBaiduWalkFlat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/share/verify") {
			w.Write([]byte(`{"errno":0,"randsk":"abc%3D"}`))
			return
		}
		resp := baiduWxListResp{Errno: 0}
		resp.Data.Title = "/movies/Test Movie (2025)"
		resp.Data.UK = 1
		resp.Data.ShareID = 2
		resp.Data.List = []baiduWxFile{
			// 根目录直属文件：path 必须是【含文件名的完整路径】（真实 wxlist 行为）
			{FsID: "3001", Path: "/movies/Test Movie (2025)/Test Movie (2025).mp4", ServerFilename: "Test Movie (2025).mp4", IsDir: "0", Size: "999"},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	oldVerify, oldWx := baiduShareVerifyURL, baiduWxListURL
	defer func() { baiduShareVerifyURL, baiduWxListURL = oldVerify, oldWx }()
	baiduShareVerifyURL = srv.URL + "/share/verify?t=%s&surl=%s"
	baiduWxListURL = srv.URL + "/share/wxlist?channel=weixin&version=2.2.2&clienttype=25&web=1"

	_, _, _, _, files, err := baiduWalk("1abc", "abc%3D", "")
	if err != nil {
		t.Fatalf("平铺分享解析失败: %v", err)
	}
	// collect 返回的是「相对 shareRoot（title 的父目录）的完整路径」，含分享目录名；
	// 与剧名同名的第一段由 buildItemDir 负责折叠，这里只验证解析层。
	if len(files) != 1 || files[0].Path != "Test Movie (2025)/Test Movie (2025).mp4" {
		t.Fatalf("平铺分享结果不对: %+v", files)
	}
}
