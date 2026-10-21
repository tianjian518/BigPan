package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

// ---------------------------------------------------------------------------
// 虚拟 WebDAV 文件系统
//
// 把「条目列表」映射成 WebDAV 里的文件：
//   /                     目录
//   /影片名.mp4           虚拟视频文件
//
// 客户端（Emby / 网易爆米花 / Infuse…）读取该文件时，
// 实际请求的是本机 /play 接口 —— 也就是走多线程加速引擎。
// ---------------------------------------------------------------------------

type vfsHandler struct {
	store *Store
	self  string // 本机对外地址，如 http://192.168.1.10:8099
}

// ---- 工具 ----

func cleanName(name string) string {
	if name == "" {
		return "/"
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return path.Clean(name)
}

// filenameReplacer 预构建一次，全局复用。
//
// 【重要】strings.NewReplacer 每次调用都要编译一棵替换树，代价不小；
// 而 PROPFIND 列目录时会按条目数平方级地调用 FileName()。
// 在 x86 上毫秒级、看不出来；在 N1 这种 ARM 盒子上会被放大成几十秒 ——
// 表现为"WebDAV 列目录超时 → 客户端报已断开"。
var filenameReplacer = strings.NewReplacer(
	"/", "_", "\\", "_", ":", "_", "*", "_",
	"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	"#", "_", "&", "_", "%", "_",
)

func safeFilename(s string) string {
	s = filenameReplacer.Replace(strings.TrimSpace(s))
	// 【防路径穿越】整个名字恰好是 "." 或 ".." 时，filepath.Join 会向上跳一级：
	// 分享主把目录/标题命名成 ".." 时，STRM 会被写到父目录之外。
	// 只处理这两种"整名"情况——"a..b" 这种中间含点的是正常文件名，不动。
	if s == "." || s == ".." {
		s = "_" + s
	}
	if s == "" {
		s = "未命名"
	}
	return s
}

// playURL 构造指向加速引擎的地址。
// 分享类条目传 id（播放时实时解析直链），静态条目直接传 url。
func (h *vfsHandler) playURL(item *Item) string {
	v := url.Values{}
	if item.IsShare() || item.URL == "" {
		v.Set("id", item.ID)
	} else {
		v.Set("url", item.URL)
	}
	if item.Size > 0 {
		v.Set("size", fmt.Sprintf("%d", item.Size))
	}
	return h.self + "/play?" + v.Encode()
}

// ---- os.FileInfo 实现 ----

type itemInfo struct {
	name  string
	size  int64
	isDir bool
	mod   time.Time
}

func (i *itemInfo) Name() string { return i.name }
func (i *itemInfo) Size() int64  { return i.size }
func (i *itemInfo) Mode() os.FileMode {
	if i.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (i *itemInfo) ModTime() time.Time { return i.mod }
func (i *itemInfo) IsDir() bool        { return i.isDir }
func (i *itemInfo) Sys() interface{}   { return nil }

// ---- 实现 webdav 库的两个【可选接口】，避免它为了猜属性去读文件 ----
//
// 【关键】不实现时，库会这么做（见 x/net/webdav/prop.go）：
//   ctype := mime.TypeByExtension(ext)
//   if ctype == "" {           // 容器镜像里没有 mime 映射表，.mkv 就是这个情况
//       f, _ := fs.OpenFile(...)   // ← 打开我们的远程流
//       io.ReadFull(f, buf[:512])  // ← 真的去读数据
//       f.Seek(0, io.SeekStart)    // ← 再回卷
//   }
// 对我们这种「每次打开都要解析网盘直链」的虚拟文件，这等于列一次目录就对每个文件
// 取一次流 —— 7 个 MKV 就要 20 秒，客户端直接判定超时（爆米花显示"已断开"）。
//
// 实现这两个方法后，库直接采用我们提供的值，一次文件都不读。

func (i *itemInfo) ContentType(ctx context.Context) (string, error) {
	return videoContentType(strings.ToLower(path.Ext(i.name))), nil
}

func (i *itemInfo) ETag(ctx context.Context) (string, error) {
	return fmt.Sprintf(`"%x-%x"`, i.mod.UnixNano(), i.size), nil
}

// ---- WebDAV FileSystem 接口 ----

func (h *vfsHandler) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return os.ErrPermission
}

func (h *vfsHandler) RemoveAll(ctx context.Context, name string) error {
	return os.ErrPermission
}

func (h *vfsHandler) Rename(ctx context.Context, oldName, newName string) error {
	return os.ErrPermission
}

func (h *vfsHandler) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	name = cleanName(name)
	if name == "/" {
		return &itemInfo{name: "/", isDir: true, mod: time.Now()}, nil
	}
	vpath := strings.TrimPrefix(name, "/")
	// 虚拟目录（网盘层 / 分享层 / 分享内子目录）
	if h.store.IsDirPath(vpath) {
		return &itemInfo{name: path.Base(vpath), isDir: true, mod: time.Now()}, nil
	}
	it := h.store.FindByName(vpath)
	if it == nil {
		return nil, os.ErrNotExist
	}
	return &itemInfo{name: it.FileName(), size: it.Size, mod: it.AddedAt}, nil
}

func (h *vfsHandler) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	name = cleanName(name)
	vpath := strings.TrimPrefix(name, "/")

	// 根目录或任何虚拟目录 → 返回可遍历的目录（聚合子目录与文件）
	if name == "/" || h.store.IsDirPath(vpath) {
		dirs, files := h.store.Children(vpath)
		infos := make([]os.FileInfo, 0, len(dirs)+len(files))
		for _, d := range dirs {
			infos = append(infos, &itemInfo{name: d, isDir: true, mod: time.Now()})
		}
		for i := range files {
			it := files[i]
			infos = append(infos, &itemInfo{name: it.FileName(), size: it.Size, mod: it.AddedAt})
		}
		return &dirFile{infos: infos, name: path.Base("/" + vpath)}, nil
	}

	it := h.store.FindByName(vpath)
	if it == nil {
		return nil, os.ErrNotExist
	}
	// 只读
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, os.ErrPermission
	}
	return &remoteFile{
		h:    h,
		item: *it,
	}, nil
}

// ---- 目录 ----

type dirFile struct {
	infos []os.FileInfo
	pos   int
	name  string
}

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, io.EOF }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, nil }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (d *dirFile) Stat() (os.FileInfo, error)     { return &itemInfo{name: d.name, isDir: true}, nil }
func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		out := d.infos[d.pos:]
		d.pos = len(d.infos)
		return out, nil
	}
	if d.pos >= len(d.infos) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.infos) {
		end = len(d.infos)
	}
	out := d.infos[d.pos:end]
	d.pos = end
	return out, nil
}

// ---- 远程文件：读取时走加速引擎 ----

type remoteFile struct {
	h       *vfsHandler
	item    Item
	pos     int64
	resp    *http.Response
	opened  bool
	zeroSeq int // 连续 0 字节读取计数，防止上游反复空响应导致的死循环
}

func (f *remoteFile) Stat() (os.FileInfo, error) {
	return &itemInfo{name: f.item.FileName(), size: f.item.Size, mod: f.item.AddedAt}, nil
}

func (f *remoteFile) Readdir(int) ([]os.FileInfo, error) { return nil, io.EOF }

func (f *remoteFile) Write([]byte) (int, error) { return 0, os.ErrPermission }

func (f *remoteFile) Close() error {
	f.closeResp()
	return nil
}

func (f *remoteFile) closeResp() {
	if f.resp != nil {
		_ = f.resp.Body.Close()
		f.resp = nil
		f.opened = false
	}
}

// ensure 按当前位置发起一次带 Range 的请求。
func (f *remoteFile) ensure() error {
	if f.opened {
		return nil
	}
	req, err := http.NewRequest("GET", f.h.playURL(&f.item), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", f.pos))

	t0 := time.Now()
	resp, err := webdavClient.Do(req)
	if err != nil {
		return err
	}
	if dt := time.Since(t0); dt > 300*time.Millisecond {
		log.Printf("  [计时] WebDAV 回环 bytes=%d- 拿到响应头 %.2fs", f.pos, dt.Seconds())
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("加速层返回 %d", resp.StatusCode)
	}
	f.resp = resp
	f.opened = true
	return nil
}

func (f *remoteFile) Read(p []byte) (int, error) {
	if err := f.ensure(); err != nil {
		return 0, err
	}
	n, err := f.resp.Body.Read(p)
	f.pos += int64(n)
	// 正常读到文件末尾：明确返回 EOF。
	if f.item.Size > 0 && f.pos >= f.item.Size {
		return n, io.EOF
	}
	if err != nil {
		// 上游提前断流（直链过期 / CDN 掐断），但文件还没读完 ——
		// 关闭连接，下次 Read 从新位置重新请求续传；绝不提前返回 EOF，
		// 否则 WebDAV 客户端会以为文件到此结束，表现为「视频不存在 / 播放几秒中断」。
		f.closeResp()
		if n > 0 {
			f.zeroSeq = 0
			return n, nil
		}
		// 本次没读到字节：交给下一轮 ensure() 重连 + /play 重新解析直链。
		// 用 zeroSeq 防上游反复空响应造成的死循环。
		f.zeroSeq++
		if f.zeroSeq > 24 {
			return 0, fmt.Errorf("上游反复返回空数据，已放弃: %s", f.item.FileName())
		}
		return 0, nil
	}
	f.zeroSeq = 0
	return n, nil
}

func (f *remoteFile) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = f.pos + offset
	case io.SeekEnd:
		np = f.item.Size + offset
	default:
		return 0, fmt.Errorf("无效的 whence")
	}
	if np < 0 {
		np = 0
	}
	if np != f.pos {
		f.closeResp() // 位置变了，下次 Read 重新请求
	}
	f.pos = np
	return np, nil
}

// webdavClient 用于回环调用本机加速引擎。
// 【注意】这里不能用 Client.Timeout —— 它是「从发请求到读完响应体」的总时长，
// 大文件 stream 会被硬掐断。只限制「等响应头」的时间：真的挂了 30 秒内必失败，
// 正常拉流不受影响（响应头一到就交给 io.Copy 慢慢传）。
var webdavClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
	},
}
