// BigPan —— 网盘多线程加速放映代理
//
// 一句话：把「分享链接里的视频」变成「可以被 Emby / 网易爆米花 / 飞牛影视 直接播放的高速流」。
//
// 工作方式：
//  1. 你把网盘分享链接丢进管理界面
//  2. 解析出直链，存成一条「条目」
//  3. 通过 WebDAV 暴露给播放器
//  4. 播放器读文件时，本服务用多路并发去网盘拉数据，拼成连续流
//     —— 网盘限的是「单条连接」，我们开几十条。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

var (
	flagPort    = flag.Int("port", 8099, "监听端口")
	flagConc    = flag.Int("concurrency", 4, "默认并发连接数（实测甜点区 4~8，过高会被网盘限流）")
	flagChunkMB = flag.Int("chunk", 0, "分片大小 (MB)，0=自动（推荐）")
	flagUA      = flag.String("ua", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "请求网盘时使用的 User-Agent")
	flagRetry   = flag.Int("retry", 6, "单个分片失败重试次数")
	flagSelf    = flag.String("self", "", "本服务对外地址（如 http://192.168.1.10:8099），留空自动探测")
	flagData    = flag.String("data", "./bigpan-data/items.json", "条目数据文件路径")
	flagUser    = flag.String("user", "admin", "管理界面用户名")
	flagPass    = flag.String("pass", "admin", "管理界面密码")
	flagRefresh = flag.Int("refresh", 60, "追更自动刷新间隔（分钟）：到点自动重扫所有追更中的分享，0=关闭")
	flagSTRM    = flag.String("strm", "/data/strm", "STRM 导出目录（生成给飞牛影视等媒体库的 .strm 目录树）")
	flagHealth  = flag.Int("health", 180, "分享失效巡检间隔（分钟）：全量探测所有已导入分享，失效则删除条目与 STRM，0=关闭")
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		ForceAttemptHTTP2:   true, // 夸克 CDN 要求 HTTP/2
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     120 * time.Second,
		DisableCompression:  true,
	},
}

// ---------------------------------------------------------------------------
// 加速引擎
// ---------------------------------------------------------------------------

func resolveTarget(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("缺少 url 参数")
	}
	if strings.HasPrefix(raw, "b64:") {
		body := strings.TrimPrefix(raw, "b64:")
		b, err := base64.URLEncoding.DecodeString(body)
		if err != nil {
			b, err = base64.StdEncoding.DecodeString(body)
		}
		if err != nil {
			return "", fmt.Errorf("base64 解码失败: %w", err)
		}
		return string(b), nil
	}
	if strings.Contains(raw, "%") {
		if dec, err := url.QueryUnescape(raw); err == nil && strings.HasPrefix(dec, "http") {
			return dec, nil
		}
	}
	return raw, nil
}

func probe(hc *http.Client, target, ua string) (size int64, supportRange bool, err error) {
	if hc == nil {
		hc = httpClient
	}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Range", "bytes=0-0")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))

	switch resp.StatusCode {
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, e := strconv.ParseInt(cr[i+1:], 10, 64); e == nil {
				return n, true, nil
			}
		}
		return -1, true, nil
	case http.StatusOK:
		return resp.ContentLength, false, nil
	default:
		return 0, false, fmt.Errorf("源站返回状态 %d", resp.StatusCode)
	}
}

func fetchRange(hc *http.Client, target, ua string, start, end int64, retry int) ([]byte, error) {
	if hc == nil {
		hc = httpClient
	}
	var lastErr error
	for attempt := 0; attempt <= retry; attempt++ {
		if attempt > 0 {
			// 退避刻意做短：分片失败绝大多数是夸克侧瞬断（unexpected EOF），
			// 立刻重试往往就成功了。原来 300ms 起步、指数翻倍、6 次累计要等近 19 秒，
			// 播放器一次 seek 撞上重试就能把起播拖到几十秒。
			time.Sleep(time.Duration(120*(1<<uint(attempt-1))) * time.Millisecond)
		}
		data, err := doFetchRange(hc, target, ua, start, end)
		if err == nil {
			return data, nil
		}
		lastErr = err
		log.Printf("  [重试 %d/%d] bytes=%d-%d: %v", attempt+1, retry, start, end, err)
	}
	return nil, fmt.Errorf("分片 bytes=%d-%d 重试耗尽: %w", start, end, lastErr)
}

func doFetchRange(hc *http.Client, target, ua string, start, end int64) ([]byte, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("状态 %d", resp.StatusCode)
	}
	buf := make([]byte, 0, end-start+1)
	tmp := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return buf, nil
}

type chunkResult struct {
	data []byte
	err  error
}

func streamRange(w http.ResponseWriter, pool *connPool, target, ua string, start, end int64, conc int, chunkSize int64, retry int, reresolve func() (string, string, string, error)) error {
	t0 := time.Now()
	total := end - start + 1
	if total <= 0 {
		return nil
	}

	// 分片规划：前几片故意切小，逐级放大（256KB → 512KB → 1MB → 2MB → 后续 chunkSize）。
	// 播放器（尤其电视端 / WebDAV 客户端）是「读一小段 → 断开 → seek 再读」，
	// 如果第 2 片直接就是 4MB，客户端拿到首片后仍要干等一两秒才能继续；
	// 首字节速度比总吞吐更要紧，所以让前几片快速就绪。
	type seg struct{ off, ln int64 }
	var segs []seg
	ramp := []int64{256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024}
	pos := start
	for _, r := range ramp {
		if pos > end {
			break
		}
		ln := r
		if ln > chunkSize {
			ln = chunkSize
		}
		if pos+ln > end+1 {
			ln = end + 1 - pos
		}
		if ln <= 0 {
			break
		}
		segs = append(segs, seg{pos, ln})
		pos += ln
	}
	for pos <= end {
		ln := chunkSize
		if pos+ln > end+1 {
			ln = end + 1 - pos
		}
		segs = append(segs, seg{pos, ln})
		pos += ln
	}
	n := len(segs)
	if conc > n {
		conc = n
	}

	sem := make(chan struct{}, conc)
	outs := make([]chan chunkResult, n)
	var mu sync.Mutex
	nextToStart := 0

	startChunk := func() {
		mu.Lock()
		defer mu.Unlock()
		if nextToStart >= n {
			return
		}
		i := nextToStart
		nextToStart++
		outs[i] = make(chan chunkResult, 1)
		go func(idx int) {
			sem <- struct{}{}
			defer func() { <-sem }()
			sg := segs[idx]
			data, err := fetchRange(pool.get(idx), target, ua, sg.off, sg.off+sg.ln-1, retry)
			outs[idx] <- chunkResult{data, err}
		}(i)
	}

	window := conc * 2
	if window > n {
		window = n
	}
	for i := 0; i < window; i++ {
		startChunk()
	}

	var written int64
	reresolved := false // 一次请求内只重解析一次；其余失败直接用新直链重试
	for i := 0; i < n; i++ {
		res := <-outs[i]
		outs[i] = nil
		// 上游「砍半」返回（声明 Content-Length 比 range 小、或提前关闭）时，
		// fetchRange 可能干净返回一部分数据。若实际字节数不符，视为上游失效，
		// 触发自愈/续传，绝不静默写入残缺分片（否则视频被偷偷截断 → 「视频不存在」）。
		if res.err == nil && int64(len(res.data)) != segs[i].ln {
			res.err = fmt.Errorf("分片 %d 只收到 %d/%d 字节（上游截断）", i, len(res.data), segs[i].ln)
		}
		if res.err != nil {
			// 上游直链疑似失效：丢弃旧缓存、重新换直链，并重试当前分片。
			// 这是「播放几秒后报视频不存在 / 中途断流」的根治手段 ——
			// 网盘直链（夸克/139）常在长连接中途过期，不自愈就会整段失败。
			if reresolve != nil && isUpstreamFailure(res.err) {
				if !reresolved {
					if nurl, nua, nck, e2 := reresolve(); e2 == nil {
						target = nurl
						ua = nua
						pool = getPool(conc, nua, nck)
						reresolved = true
						log.Printf("  [自愈] 直链疑似过期，已重新解析并重试")
					}
				}
				if reresolved {
					if data2, e3 := fetchRange(pool.get(i), target, ua, segs[i].off, segs[i].off+segs[i].ln-1, retry); e3 == nil {
						res.data = data2
						res.err = nil
						log.Printf("  [自愈] 分片 %d 已用新直链重取（%s）", i, humanSize(int64(len(data2))))
					}
				}
			}
		}
		if res.err != nil {
			return fmt.Errorf("分片 %d 失败: %w", i, res.err)
		}
		if _, err := w.Write(res.data); err != nil {
			return err
		}
		if i == 0 {
			log.Printf("  [计时] 上游首片 %s 就绪 %.2fs", humanSize(int64(len(res.data))), time.Since(t0).Seconds())
		}
		written += int64(len(res.data))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		startChunk()
	}
	log.Printf("  完成：%d 片 / %.1f MB / 并发 %d（总用时 %.2fs）", n, float64(written)/1024/1024, conc, time.Since(t0).Seconds())
	return nil
}

// isUpstreamFailure 判断错误是否来自上游网盘（而不是客户端主动断开）。
// 只有前者才值得丢弃直链缓存重新解析。
func isUpstreamFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	clientGone := []string{
		"broken pipe",
		"connection reset by peer",
		"connection reset",
		"client disconnected",
		"stream closed",
		"context canceled",
		"eof",
	}
	for _, k := range clientGone {
		if strings.Contains(s, k) {
			return false
		}
	}
	return true
}

func parseClientRange(r *http.Request, size int64) (start, end int64, ok bool) {
	rh := r.Header.Get("Range")
	if rh == "" {
		return 0, size - 1, false
	}
	rh = strings.TrimPrefix(rh, "bytes=")
	parts := strings.SplitN(rh, ",", 2)
	rh = parts[0]
	p := strings.SplitN(strings.TrimSpace(rh), "-", 2)
	if len(p) != 2 {
		return 0, size - 1, false
	}
	if p[0] == "" {
		n, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, size - 1, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil {
		return 0, size - 1, false
	}
	e := size - 1
	if p[1] != "" {
		if v, err := strconv.ParseInt(p[1], 10, 64); err == nil {
			e = v
		}
	}
	if e >= size {
		e = size - 1
	}
	if s < 0 || s > e {
		return 0, size - 1, false
	}
	return s, e, true
}

// resolveDirectURL 取某条目的可直连地址（带缓存）。
// 给「302 直链模式」和 /api/link 用：只解析地址，不经过本机中转数据。
func resolveDirectURL(it *Item) (string, error) {
	if pt := getPlayTarget(it.ID); pt != nil && pt.url != "" {
		return pt.url, nil
	}
	var url, ptUA, cookie string
	if it.IsShare() {
		p := providerByName(it.Provider)
		if p == nil {
			return "", fmt.Errorf("不支持的网盘: %s", it.Provider)
		}
		rp, err := p.ResolvePlay(it)
		if err != nil {
			return "", err
		}
		url, ptUA, cookie = rp.URL, rp.UA, rp.Cookie
	} else {
		url = it.URL
	}
	if url == "" {
		return "", fmt.Errorf("没有可直连的地址")
	}
	putPlayTarget(it.ID, &playTarget{
		url: url, ua: ptUA, cookie: cookie,
		size: it.Size, ranged: true,
		exp: time.Now().Add(playTargetTTL),
	})
	return url, nil
}

// resolvePlayFresh 强制重新解析某条目的播放直链（先丢弃旧缓存再解析）。
//
// 两处用到：
//
//	① 缓存未命中时的首次解析；
//	② 流式中途上游直链失效时的「自愈」重解析（见 streamRange）。
//
// 注意：分享类条目每次都调网盘接口换直链（有 API 开销），所以只在缓存缺失
// 或确实失效时才走这条路；静态 URL 条目则无需重新解析。
func resolvePlayFresh(it *Item, urlParam string, conc int) (*playTarget, error) {
	if it != nil {
		dropPlayTarget(it.ID)
	}
	var url, ptUA, cookie string
	if it != nil && it.IsShare() {
		p := providerByName(it.Provider)
		if p == nil {
			return nil, fmt.Errorf("不支持的网盘: %s", it.Provider)
		}
		rp, err := p.ResolvePlay(it)
		if err != nil {
			return nil, err
		}
		url, ptUA, cookie = rp.URL, rp.UA, rp.Cookie
	} else if it != nil && it.URL != "" {
		url = it.URL
	} else if urlParam != "" {
		u, err := resolveTarget(urlParam)
		if err != nil {
			return nil, err
		}
		url = u
	} else {
		return nil, fmt.Errorf("没有可播放的地址")
	}
	ua := ptUA
	if ua == "" {
		ua = *flagUA
	}
	var size int64
	var ranged bool
	if it != nil && it.Size > 0 {
		size, ranged = it.Size, true
	} else {
		s2, r2, perr := probe(getPool(conc, ua, cookie).get(0), url, ua)
		if perr != nil {
			return nil, fmt.Errorf("探测源站失败: %w", perr)
		}
		size, ranged = s2, r2
	}
	pt := &playTarget{url: url, ua: ua, cookie: cookie, size: size, ranged: ranged, exp: time.Now().Add(playTargetTTL)}
	// 仅对「有 ID 的条目」写缓存；纯 url 参数（静态直链）不入缓存，避免空键碰撞。
	if it != nil && it.ID != "" {
		putPlayTarget(it.ID, pt)
	}
	return pt, nil
}

// apiLink 返回条目的直链，方便做 STRM / 排查，也用来验证直链能否被外部直接拉。
func apiLink(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		it := store.FindByID(r.URL.Query().Get("id"))
		if it == nil {
			http.Error(w, "条目不存在", http.StatusNotFound)
			return
		}
		u, err := resolveDirectURL(it)
		if err != nil {
			http.Error(w, "解析失败: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]interface{}{
			"id": it.ID, "name": it.FileName(), "url": u, "size": it.Size,
		})
	}
}

// directGuard 直链端点（STRM 用，同时兼容 WebDAV）。
//
// 两条路各管各的，互不干扰：
//   - /dav    = WebDAV 挂载端点（爆米花、rclone 等客户端用）
//   - /direct = STRM 播放端点（飞牛影视 / Emby 读 .strm 用），也可当 WebDAV 用
//
// 两者共用同一份虚拟盘数据，但**配置互相独立**：STRM 的目录、对外地址、层级
// 只影响 .strm 文件，不会改变 /dav 的挂载行为；反之亦然。
func directGuard(store *Store) http.Handler {
	dav := &webdav.Handler{
		Prefix:     "/direct",
		FileSystem: &vfsHandler{store: store, self: "http://127.0.0.1:" + strconv.Itoa(*flagPort)},
		LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS：网页播放器跨域跟随 302 时需要，缺了会被浏览器拦。
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Range, Content-Type, Authorization")
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// STRM 播放令牌：媒体服务器拿到的 STRM 地址里不带 Basic Auth，
		// 用 ?k=<路径令牌> 免登录（令牌由账号密码 + 路径派生，仅对该路径有效）。
		strmOK := false
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Query().Get("k") != "" {
			np := strings.TrimPrefix(r.URL.Path, "/direct/")
			if dec, err := url.PathUnescape(np); err == nil {
				np = dec
			}
			strmOK = r.URL.Query().Get("k") == strmPathToken(np)
		}
		if !authed(r) && !strmOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			name := strings.TrimPrefix(r.URL.Path, "/direct/")
			if dec, err := url.PathUnescape(name); err == nil {
				name = dec
			}
			it := store.FindByName(name)
			if it != nil {
				// 夸克 + 开关关着：分享直链绑登录会话，客户端直连会被 412 拒，
				// 不能 302 —— 转交 /play 走本机中转（开关关着时生成的存量
				// /direct .strm 靠这条路继续播；开关开着时根本不会写 /direct 夸克）。
				if strings.EqualFold(it.Provider, "quark") && !quarkTV302Enabled() {
					v := url.Values{}
					v.Set("id", it.ID)
					if it.Size > 0 {
						v.Set("size", fmt.Sprintf("%d", it.Size))
					}
					http.Redirect(w, r, "/play/"+url.PathEscape(it.FileName())+"?"+v.Encode(), http.StatusFound)
					return
				}
				u, err := resolveDirectURL(it)
				if err != nil {
					log.Printf("  [直链] %s 解析失败: %v", it.FileName(), err)
					http.Error(w, "取直链失败: "+err.Error(), http.StatusBadGateway)
					return
				}
				// 百度 + 浏览器 + 用户开启了代取开关 → 本机转发，不回 302。
				// 见 STRMConfig.BaiduBrowserRelay 的说明（默认关闭，只影响百度）。
				if wantBrowserRelay(it, r) {
					ua, ck := *flagUA, ""
					if pt := getPlayTarget(it.ID); pt != nil {
						if pt.ua != "" {
							ua = pt.ua
						}
						ck = pt.cookie
					}
					log.Printf("  [直链] %s → 浏览器代取（数据经过本机）", it.FileName())
					proxyBaiduRelay(w, r, u, ua, ck)
					return
				}
				log.Printf("  [直链] %s → 302 跳转（数据不经过本机）", it.FileName())
				// 关键：禁止客户端缓存 302 目标。直链常在几秒~几分钟内过期，
				// 缓存后第二次播放会拿到死链，表现为「视频不存在」。
				w.Header().Set("Cache-Control", "no-store, max-age=0")
				w.Header().Set("Pragma", "no-cache")
				http.Redirect(w, r, u, http.StatusFound)
				return
			}
		}
		// 非文件请求（列目录等）交给 WebDAV 库，保持与 /dav 一致的接法。
		dav.ServeHTTP(w, r)
	})
}

func handlePlay(store *Store, as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CORS：网页播放器（飞牛影视/Emby/video.js 等）跨域拉流必须带这些头，
		// 缺了会被浏览器直接拦掉，表现为「每个文件都播放不了」，且服务端日志里
		// 连请求都看不到。OPTIONS 预检单独回 204。
		h0 := w.Header()
		h0.Set("Access-Control-Allow-Origin", "*")
		h0.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h0.Set("Access-Control-Allow-Headers", "Range, Content-Type, Authorization")
		h0.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		q := r.URL.Query()

		// 取播放目标：分享条目先实时解析出直链
		var it *Item
		var target string
		if id := q.Get("id"); id != "" {
			it = store.FindByID(id)
			if it == nil {
				http.Error(w, "条目不存在", http.StatusNotFound)
				return
			}
		}

		// 夸克 + 开关开着：不走本机中转，内部解析出 TV 免认证直链后回 302，
		// 客户端直连网盘 CDN，数据不经过本机（存量 /play .strm 的兼容路——
		// 开关关着时生成的 /play 地址，开关打开后照样能播）。
		//
		// 清晰度策略（2026-10-20 §23.56/§23.58/§23.59 实测定）：只走 TV
		// 转码流。原画直链单连接被 CDN 限速到 ~100KB/s（沙箱+家宽双端
		// 实锤），302 模式下连接归播放器管，而爆米花/飞牛饿起来只开 1 个
		// 连接（日志：转码流 1 分钟 12+ 个 vs 原画 6 分钟 1 个）——原画
		// 302 是死路，画质选项已下架。转码流该 CDN 不限单连接，家宽实测
		// 多连接叠加 16MB/s。
		if it != nil && strings.EqualFold(it.Provider, "quark") && quarkTV302Enabled() {
			var u string
			var uerr error
			var pt PlayTarget
			pt, uerr = quarkTVPlayTarget(it)
			u = pt.URL
			if uerr == nil {
				log.Printf("  [直链] %s → 302 跳转（夸克 TV 转码流，数据不经过本机）", it.FileName())
			}
			if uerr != nil {
				if it.IsShare() {
					log.Printf("  [%s] 取流失败: %v", it.Provider, uerr)
				}
				http.Error(w, "取流失败: "+uerr.Error(), http.StatusBadGateway)
				return
			}
			// 禁止缓存 302 目标：直链几秒~几分钟内过期，缓存后第二次播放拿死链。
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			http.Redirect(w, r, u, http.StatusFound)
			return
		}

		conc := *flagConc
		if v := q.Get("conc"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 256 {
				conc = n
			}
		}
		chunk := int64(*flagChunkMB) * 1024 * 1024
		if v := q.Get("chunk"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 64 {
				chunk = int64(n) * 1024 * 1024
			}
		}

		ua := q.Get("ua")
		if ua == "" && it != nil {
			ua = it.UA // 条目自带适合该网盘的 UA
		}
		if ua == "" {
			ua = *flagUA
		}

		// 解析播放目标（带缓存）
		// 关键：播放器常按「分段」读，若每段都重换直链 + 重探源站 + 重建 TLS，
		// 固定开销会吃掉大部分带宽。缓存后第二段起近乎零准备。
		var pool *connPool
		var pt *playTarget
		if it != nil {
			pt = getPlayTarget(it.ID)
		}

		if pt == nil {
			tResolve := time.Now()
			pt2, rerr := resolvePlayFresh(it, q.Get("url"), conc)
			if rerr != nil {
				if it != nil && it.IsShare() {
					log.Printf("  [%s] 取流失败: %v", it.Provider, rerr)
				}
				http.Error(w, "取流失败: "+rerr.Error(), http.StatusBadGateway)
				return
			}
			pt = pt2
			log.Printf("  [计时] 首次解析直链耗时 %.2fs", time.Since(tResolve).Seconds())
		}
		ua = pt.ua

		target = pt.url
		pool = getPool(conc, ua, pt.cookie)
		size, supportRange := pt.size, pt.ranged

		if !supportRange || size <= 0 {
			log.Printf("→ %s | 源站不支持 Range，单连接透传", truncate(target, 90))
			proxyPlain(w, r, target, ua, pool.get(0))
			return
		}

		start, end, hasRange := parseClientRange(r, size)

		// 读到文件末尾（播放器探测时长 / seek 到尾）：返回 0 长度 206，不回吐数据。
		// 否则会把整段文件回吐给「只想拿个时长」的客户端，造成误读。
		if hasRange && start >= size {
			h := w.Header()
			h.Set("Accept-Ranges", "bytes")
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusPartialContent)
			return
		}

		// 自愈重解析闭包：流式中途直链失效时，丢弃旧缓存并重新换直链。
		// 仅分享类条目需要（静态 URL 不会过期）；/dav 经此实现「断流续传」。
		var reresolve func() (string, string, string, error)
		if it != nil && it.IsShare() {
			reresolve = func() (string, string, string, error) {
				pt2, e := resolvePlayFresh(it, "", conc)
				if e != nil {
					return "", "", "", e
				}
				return pt2.url, pt2.ua, pt2.cookie, nil
			}
		}

		// 自动分片：一次请求至少要能切成 conc*2 片，并发才吃得满。
		// 播放器常按 1~2MB 分段读；若固定 4MB 分片，一段只有 1 片 → 等于单连接。
		if chunk <= 0 {
			span := end - start + 1
			chunk = span / int64(conc*2)
			// 分片上限刻意压到 1MB。
			// 4MB 分片在夸克单路 ~1.4MB/s 的速度下每片要等近 3 秒，
			// 客户端看到的数据节奏是「256KB → 停 3 秒 → 4MB → 停 3 秒」，
			// 播放器会在数据间隙里判定超时、把连接掐掉重来 ——
			// 日志里那串反复出现的 `GET 0-` + broken pipe 就是这么来的。
			// 压到 1MB 后每片约 0.7 秒，数据流连续得多。
			if chunk > 1*1024*1024 {
				chunk = 1 * 1024 * 1024
			}
			if chunk < 256*1024 {
				chunk = 256 * 1024
			}
		}
		uaShort := r.Header.Get("User-Agent")
		if len(uaShort) > 42 {
			uaShort = uaShort[:42]
		}
		log.Printf("→ %s | %d-%d/%d | 并发 %d × %s | UA=%s", truncate(target, 60), start, end, size, conc, humanSize(chunk), uaShort)

		h := w.Header()
		h.Set("Accept-Ranges", "bytes")
		ct := "video/mp4"
		if it != nil && it.Ext != "" {
			ct = videoContentType(it.Ext)
		}
		h.Set("Content-Type", ct)
		if hasRange {
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusOK)
		}

		// 文件头缓存：从头请求时先秒发缓存段，剩余部分再从缓存末尾接着拉。
		// 播放器开播前普遍会从 0 反复请求几次，这一步能把那几次全变成毫秒级。
		var tee *headTee
		out := w
		if it != nil && hasRange && start == 0 && size > headCacheSize {
			if c := getHeadCache(it.ID); len(c) > 0 && int64(len(c)) <= end-start+1 {
				if n, werr := w.Write(c); werr == nil {
					log.Printf("  [缓存] 命中文件头 %s，直接秒发", humanSize(int64(n)))
					start += int64(n)
				} else {
					return
				}
			} else {
				tee = &headTee{w: w, max: headCacheSize}
				out = tee
			}
		}

		if err := streamRange(out, pool, target, ua, start, end, conc, chunk, *flagRetry, reresolve); err != nil {
			log.Printf("  中断: %v", err)
			if it != nil && isUpstreamFailure(err) {
				dropPlayTarget(it.ID)
			}
		}
		if tee != nil && len(tee.buf) >= headCacheSize {
			putHeadCache(it.ID, tee.buf)
			log.Printf("  [缓存] 已缓存文件头 %s（下次开播秒发）", humanSize(int64(len(tee.buf))))
		}
	}
}

func proxyPlain(w http.ResponseWriter, r *http.Request, target, ua string, hc *http.Client) {
	if hc == nil {
		hc = httpClient
	}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := hc.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ---------------------------------------------------------------------------
// 百度「浏览器播放」代取（默认关闭，见 STRMConfig.BaiduBrowserRelay）
//
// 背景：百度 dlink 按 UA 分流，浏览器 UA 请求 appall01 会被拒，纯 302 在
// 浏览器里必然播不了。开关打开后，仅当「百度 + 浏览器 + GET/HEAD」时，
// 改由本机带 netdisk UA 代取并转发。其余情况一律保持原来的 302。
//
// ⚠️ 不加这个开关时，本文件其余逻辑与改动前完全一致。
// ---------------------------------------------------------------------------

// wantBrowserRelay 判断本次请求是否要走百度代取。
// 三个条件缺一不可：配置开启、条目属于百度、请求方是浏览器。
func wantBrowserRelay(it *Item, r *http.Request) bool {
	if it == nil || !strmCfg.BaiduBrowserRelay {
		return false
	}
	if !strings.EqualFold(it.Provider, "baidu") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return isBrowserUA(r.Header.Get("User-Agent"))
}

// isBrowserUA 判断 UA 是否来自浏览器。
//
// 必须排除媒体服务器（飞牛影视 / Emby / Jellyfin / Kodi / Infuse 等）：
// 它们能自己处理 302，让它们走代取只会白白吃掉本机带宽。
func isBrowserUA(ua string) bool {
	if ua == "" {
		return false
	}
	low := strings.ToLower(ua)
	// 先排除：UA 里常带 Mozilla 字样的非浏览器客户端
	for _, kw := range []string{
		"kodi", "infuse", "vlc", "emby", "jellyfin", "plex",
		"okhttp", "dalvik", "python-", "curl/", "wget", "go-http-client",
		"libmpv", "exoplayer", "ffmpeg",
	} {
		if strings.Contains(low, kw) {
			return false
		}
	}
	if !strings.Contains(ua, "Mozilla") {
		return false
	}
	for _, kw := range []string{"Chrome/", "Firefox/", "Safari/", "Edg/", "AppleWebKit"} {
		if strings.Contains(ua, kw) {
			return true
		}
	}
	return false
}

// proxyBaiduRelay 带网盘 UA 代取百度直链并把字节转发给调用方。
// 透传 Range / 206 / Content-Range，保证拖动进度条和断点续传正常。
func proxyBaiduRelay(w http.ResponseWriter, r *http.Request, target, ua, cookie string) {
	req, err := http.NewRequest(r.Method, target, nil)
	if err != nil {
		http.Error(w, "代取失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	req.Header.Set("Accept", "*/*")
	// 百度 dlink 要求带 Referer，缺了也会被拒。
	req.Header.Set("Referer", "https://pan.baidu.com/")

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "代取失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		log.Printf("  [代取] 上游返回 %d", resp.StatusCode)
		http.Error(w, fmt.Sprintf("上游返回 %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// 只透传对播放有意义的头，避免把上游的 CORS / Set-Cookie 一起漏出去
	// （CORS 头在 directGuard 开头已按本项目策略设好）。
	for _, k := range []string{
		"Content-Length", "Content-Range", "Accept-Ranges",
		"Last-Modified", "ETag",
	} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	// 百度整片常回 application/octet-stream，浏览器会因 MIME 非视频而黑屏，
	// 所以按真实扩展名纠正 Content-Type（而不是照抄上游）。
	ct := videoContentType(filepath.Ext(target))
	if ct == "" {
		ct = resp.Header.Get("Content-Type")
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

// videoContentType 按真实扩展名给出 Content-Type。
// 关键：MKV 若被标成 video/mp4，播放器会用 MP4 解析器去解 —— 直接播放失败。
func videoContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".mov":
		return "video/quicktime"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".webm":
		return "video/webm"
	case ".rmvb", ".rm":
		return "application/vnd.rn-realmedia"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	default:
		return "video/mp4"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// 管理界面 & API
// ---------------------------------------------------------------------------

// requireAuth 给 API 套上登录校验。
// 之前只有 /api/items 做了校验，其余接口（含"导入网盘凭证""账号列表"）裸奔 ——
// 同一局域网内任何人都能读取/写入凭证。这里统一堵上。
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func authed(r *http.Request) bool {
	// ① Basic Auth：播放器 / WebDAV 客户端走这条路
	if u, p, ok := r.BasicAuth(); ok && u == *flagUser && p == *flagPass {
		return true
	}
	// ② 登录页签发的长会话 Cookie：浏览器免得每次都输密码
	if ck, err := r.Cookie("bigpan_session"); err == nil && ck.Value == sessionToken() {
		return true
	}
	return false
}

// sessionToken 无状态会话令牌：由账号密码派生，改密码即全部失效，重启也不丢。
func sessionToken() string {
	sum := sha256.Sum256([]byte("bigpan-session|" + *flagUser + "|" + *flagPass))
	return hex.EncodeToString(sum[:16])
}

// loginHandler GET 出登录页，POST 校验并发 30 天 Cookie。
func loginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				User string `json:"user"`
				Pass string `json:"pass"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.User == *flagUser && req.Pass == *flagPass {
				http.SetCookie(w, &http.Cookie{
					Name: "bigpan_session", Value: sessionToken(),
					Path: "/", MaxAge: 30 * 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode,
				})
				writeJSON(w, map[string]interface{}{"ok": true})
				return
			}
			writeJSON(w, map[string]interface{}{"ok": false, "error": "账号或密码不对"})
			return
		}
		if authed(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, loginPageHTML(defUserHint()))
	}
}

// defUserHint 密码仍是出厂默认 "admin" 时返回当前用户名（供登录页明示默认账号密码），
// 否则返回空串——密码改过之后，登录页不出现任何账号相关信息。
func defUserHint() string {
	if *flagPass == "admin" {
		return *flagUser
	}
	return ""
}

// davGuard 给 WebDAV 端点套上登录校验，并让 /dav 与 /dav/ 两种写法都能直接用。
// 两个坑：
//  1. 之前 /dav/ 完全没走认证 —— 外网任何人拿到地址就能列目录、拉取夸克直链；
//  2. ServeMux 只注册了 /dav/，客户端填 /dav（不带尾斜杠）会吃到 307，
//     部分安卓/电视端 WebDAV 客户端不跟随 PROPFIND 的跳转，直接报"网络异常"。
//
// 无论客户端填哪种，这里都放行（自己播/播放器都兼容）。
func davGuard(h http.Handler, store *Store, self string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 排查播放器"连不上"时全靠这行日志：能看到对方到底发了什么方法、什么 UA。
		who := "无"
		if _, _, ok := r.BasicAuth(); ok {
			who = "有"
		}
		log.Printf("WebDAV ← %s %s 认证=%s UA=%q", r.Method, r.URL.Path, who, r.Header.Get("User-Agent"))

		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		// 【按网盘智能路由】支持直连的网盘（如移动云盘），GET/HEAD 文件时
		// 直接 302 跳到网盘直链，客户端自己去 CDN 拉 —— 数据不经过本机，
		// 不占家宽上行，高码率原盘也能流畅。夸克默认绑会话走加速中转；
		// 用户打开「夸克 TV 302」开关后 Wants302() 变 true，这里自动跟随，
		// 同样走 302（TV 免认证直链）。
		//
		// 全程 302 模式（2026-10-16 应用户要求）：Wants302 网盘（百度/移动）
		// 解析失败时直接返回 502，不再回落本地中转 —— 中转会把全部视频流量
		// 灌进 CF 隧道 + N1 盒子，高码率必卡，且违背"数据不过本机"的初衷
		// （实测见 Bigpan备忘录 §18：Pixel 7 放 139 回落中转后 broken pipe 一片）。
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			vpath := strings.TrimPrefix(r.URL.Path, "/dav/")
			if vpath != "" && !strings.HasSuffix(vpath, "/") {
				if it := store.FindByName(vpath); it != nil && it.IsShare() {
					if p := providerByName(it.Provider); p != nil && p.Wants302() {
						u, uerr := resolveDirectURL(it)
						if uerr == nil {
							log.Printf("  [302] %s（%s）→ 客户端直连网盘 CDN", it.FileName(), it.Provider)
							http.Redirect(w, r, u, http.StatusFound)
							return
						}
						log.Printf("  [302] %s 直链解析失败，全程302模式返回错误（不中转）", it.FileName())
						http.Error(w, "取直链失败: "+uerr.Error(), http.StatusBadGateway)
						return
					}
				}
			}
		}
		// 【2026-10-17 修复「子目录点击后没效果」】目录的 GET/HEAD 一律给 200 可读页。
		// 原来只有 /dav 和 /dav/ 根路径有 davBrowse 兜底，子目录的 GET 落到 webdav 库
		// 一律回 405 —— 而部分文件 App 点目录前会先发 GET 探活，看到 405 就判定
		// 这个地址不可达，直接放弃，连 PROPFIND 都不发。根目录一直回 200 所以
		// 点得动，子目录回 405 所以点了没反应 —— 症状完全吻合。
		// 文件类请求在上面已按 302/中转处理，这里只兜目录（含根）。
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			vpath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dav/"), "/")
			if vpath == "" || store.IsDirPath(vpath) {
				davBrowsePath(w, r, store, self, vpath)
				return
			}
		}
		if r.URL.Path == "/dav" {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/dav/"
			h.ServeHTTP(w, r2)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// davBrowsePath 浏览器 / GET 探活时返回的可读列表页（纯展示，不影响 PROPFIND 客户端）。
// vpath 为空表示根目录；按请求路径分层渲染：面包屑 + 当前层目录/文件，
// 目录项链接一律带尾斜杠（与 PROPFIND 的 href 形态一致，客户端怎么点都不会 404）。
func davBrowsePath(w http.ResponseWriter, r *http.Request, store *Store, self, vpath string) {
	dirs, files := store.Children(vpath)
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>BigPan WebDAV</title><style>body{font-family:-apple-system,sans-serif;max-width:880px;margin:20px auto;padding:0 14px;color:#222}a{color:#2563eb;text-decoration:none}table{width:100%;border-collapse:collapse;margin-top:10px}td{padding:7px 8px;border-bottom:1px solid #eee;font-size:14px;word-break:break-all}.crumb{margin:12px 0;color:#666;font-size:14px}h3{font-size:17px}</style>`)
	b.WriteString(`<div class="crumb"><a href="/dav/">根目录</a>`)
	if vpath != "" {
		acc := ""
		for _, s := range strings.Split(vpath, "/") {
			if s == "" {
				continue
			}
			acc += s
			b.WriteString(` / <a href="/dav/` + url.PathEscape(acc) + `/">` + html.EscapeString(s) + `</a>`)
			acc += "/"
		}
	}
	b.WriteString(`</div>`)
	if vpath == "" {
		fmt.Fprintf(&b, "<h3>BigPan WebDAV · 共 %d 个条目</h3>", len(store.List()))
		fmt.Fprintf(&b, "<p>服务正常。播放器请用 WebDAV 客户端连接：<code>%s/dav</code></p>", html.EscapeString(self))
	} else {
		fmt.Fprintf(&b, "<h3>%s</h3>", html.EscapeString(strings.ReplaceAll(vpath, "/", " / ")))
	}
	b.WriteString("<table>")
	prefix := ""
	if vpath != "" {
		prefix = vpath + "/"
	}
	for _, d := range dirs {
		b.WriteString(`<tr><td>📁 <a href="/dav/` + url.PathEscape(prefix+d) + `/">` + html.EscapeString(d) + `</a></td><td style="text-align:right;color:#999">目录</td></tr>`)
	}
	for i := range files {
		it := files[i]
		b.WriteString(`<tr><td>🎬 <a href="/dav/` + url.PathEscape(prefix+it.FileName()) + `">` + html.EscapeString(it.FileName()) + `</a></td><td style="text-align:right;color:#999">` + humanSize(it.Size) + `</td></tr>`)
	}
	if len(dirs)+len(files) == 0 {
		b.WriteString(`<tr><td colspan="2" style="color:#999">（空目录）</td></tr>`)
	}
	b.WriteString("</table>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
		return
	}
	io.WriteString(w, b.String())
}

func apiItems(store *Store, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{
				"items": store.List(),
				"self":  self,
			})
		case http.MethodPost:
			var body struct {
				Title    string `json:"title"`
				URL      string `json:"url"`
				Password string `json:"password"`
				Provider string `json:"provider"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "参数错误: "+err.Error(), http.StatusBadRequest)
				return
			}
			body.URL = strings.TrimSpace(body.URL)
			if body.URL == "" {
				http.Error(w, "直链不能为空", http.StatusBadRequest)
				return
			}
			if body.Title == "" {
				body.Title = "未命名_" + time.Now().Format("0102_150405")
			}
			// 探测大小（失败也不阻断）
			size := int64(0)
			if s, ok, err := probe(httpClient, body.URL, *flagUA); err == nil && ok && s > 0 {
				size = s
			}
			it := Item{
				Title:    body.Title,
				URL:      body.URL,
				Provider: body.Provider,
				Size:     size,
			}
			store.Add(it)
			writeJSON(w, map[string]interface{}{"ok": true, "size": size})
		default:
			http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
		}
	}
}

func apiItemDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/items/")
		if r.Method != http.MethodDelete {
			http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
			return
		}
		if store.Delete(id) {
			writeJSON(w, map[string]interface{}{"ok": true})
		} else {
			http.Error(w, "未找到", http.StatusNotFound)
		}
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func indexPage(store *Store, as *AccountStore, self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !authed(r) {
			// 浏览器场景：跳登录页，而不是弹 Basic 框 —— 免得每次都输密码
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, renderIndex(store, as, self))
	}
}

// ---------------------------------------------------------------------------
// 内置播放页
// ---------------------------------------------------------------------------

func watchPage(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		id := r.URL.Query().Get("id")
		it := store.FindByID(id)
		if it == nil {
			http.NotFound(w, r)
			return
		}
		// 分享类条目没有静态直链，必须用 id 交给服务端实时解析。
		// （曾经这里固定拼 ?url=，分享条目拿到的是空地址 —— 于是"自检页能播、播放页永远播不了"）
		v := url.Values{}
		if it.IsShare() || it.URL == "" {
			v.Set("id", it.ID)
		} else {
			v.Set("url", it.URL)
		}
		if it.Size > 0 {
			v.Set("size", strconv.FormatInt(it.Size, 10))
		}
		srcJSON, _ := json.Marshal("/play?" + v.Encode())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, renderWatch(it, string(srcJSON)))
	}
}

func renderWatch(it *Item, srcJSON string) string {
	sizeStr := "未知"
	if it.Size > 0 {
		sizeStr = fmt.Sprintf("%.2f GB", float64(it.Size)/1024/1024/1024)
	}
	return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + html.EscapeString(it.Title) + ` · BigPan</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","Microsoft YaHei",sans-serif;
 background:#000;color:#e2e8f0;margin:0;padding:16px}
.wrap{max-width:1000px;margin:0 auto}
h1{font-size:17px;margin:0 0 10px;font-weight:600}
video{width:100%;background:#000;border-radius:8px;outline:none}
.bar{display:flex;gap:18px;flex-wrap:wrap;font-size:13px;color:#94a3b8;margin-top:12px;
 background:#111827;border:1px solid #1f2937;border-radius:8px;padding:10px 14px}
.bar b{color:#4ade80}
a{color:#38bdf8;text-decoration:none;font-size:13px}
</style></head><body><div class="wrap">
<h1>` + html.EscapeString(it.Title) + `</h1>
<video id="v" controls autoplay playsinline></video>
<div id="fmtTip" style="display:none;margin-top:12px;background:#7f1d1d;border:1px solid #991b1b;border-radius:8px;padding:12px 14px;font-size:13px;line-height:1.9">
  <b>⚠️ 视频没能播放</b>
  <div id="errDetail" style="margin:9px 0;padding:9px;background:#450a0a;border-radius:6px;font-size:12px;color:#fecaca"></div>
  常见原因：
  <div style="margin-top:4px">
  · <b>格式</b>：浏览器解不开 MKV 容器 / H.265(HEVC) —— 4K HDR 片源常见<br>
  · <b>网络</b>：手机不在同一 WiFi，取不到 <code>本机IP:8099</code> 的数据<br>
  · <b>夸克侧</b>：直链失效或被限流
  </div>
  <div style="margin-top:8px;color:#fca5a5">
  先用 <a href="/test" style="color:#fbbf24">浏览器能力自检</a> 看一眼：如果那里显示「H.265 不支持」，就是格式问题，
  请改用 <b>网易爆米花 / 飞牛影视 / Infuse</b> 通过 WebDAV 播放。
  </div>
</div>
<div class="bar">
  <span>文件大小：<b>` + sizeStr + `</b></span>
  <span>前方缓冲：<b id="buf">0</b> 秒</span>
  <span>已缓冲：<b id="tot">0</b> 秒</span>
</div>
<div style="margin-top:12px"><a href="/">← 返回管理界面</a></div>
</div>
<script>
var v=document.getElementById('v');
// 浏览器不支持该容器/编码时，立刻给出人话提示，而不是停在 0%
v.addEventListener('error',function(){
  var c=v.error?v.error.code:0;
  var m={1:'加载被中止(aborted)',2:'网络错误(network)',3:'解码失败(decode)',4:'源不可用/格式不支持(src not supported)'}[c]||('错误码 '+c);
  var src=v.currentSrc||'';
  var shown=src.length>70?src.slice(0,70)+'…':src;
  document.getElementById('errDetail').innerHTML=
    '实测错误：<b>code '+c+'</b> — '+m+
    '<br>networkState='+v.networkState+
    '<br>请求地址：'+shown;
  document.getElementById('fmtTip').style.display='block';
  document.querySelector('.bar').style.opacity='0.5';
});
v.src=` + srcJSON + `;
v.load();
var _p=v.play();
if(_p&&_p.catch){_p.catch(function(e){
  var d=document.getElementById('errDetail');
  if(d){d.innerHTML='自动播放被浏览器拦截（'+e.name+'）—— 点一下下面的播放按钮即可。<br>请求地址：'+(v.currentSrc||'');}
  document.getElementById('fmtTip').style.display='block';
});}
setInterval(function(){
  var buf=0,tot=0;
  try{
    for(var i=0;i<v.buffered.length;i++){
      if(v.buffered.start(i)<=v.currentTime&&v.buffered.end(i)>=v.currentTime){
        buf=v.buffered.end(i)-v.currentTime;
      }
    }
    if(v.buffered.length){tot=v.buffered.end(v.buffered.length-1);}
  }catch(e){}
  document.getElementById('buf').textContent=buf.toFixed(1);
  document.getElementById('tot').textContent=tot.toFixed(1);
},500);
</script></body></html>`
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

func detectSelf(port int) string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return fmt.Sprintf("http://%s:%d", ip4.String(), port)
				}
			}
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func main() {
	// 【关键】务必在发第一个网络请求之前装好自带的 DNS 解析器。
	//
	// 盒子上容器默认用路由器 DNS（192.168.0.1:53，来自 /etc/resolv.conf）。
	// 但盒子同时跑着 sing-box（tun 模式，singtun0），容器发出的 UDP 53
	// 会偶发被吞掉。Go 的纯解析器按 resolv.conf 逐台服务器、每台 5 秒、
	// 重试若干次 —— 实测一次超时正好卡满 20 秒，日志里那句
	//   lookup drive-pc.quark.cn on 192.168.0.1:53: i/o timeout
	// 就是它。解析直链一卡 20 秒，播放器看着就是"十几二十秒不出画面"。
	//
	// 换成公共 DNS，并把单次超时压到 3 秒（最坏 6 秒，而不是 20 秒）。
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var lastErr error
			for _, addr := range []string{"223.5.5.5:53", "119.29.29.29:53"} {
				d := net.Dialer{Timeout: 3 * time.Second}
				if c, err := d.DialContext(ctx, network, addr); err == nil {
					return c, nil
				} else {
					lastErr = err
				}
			}
			return nil, lastErr
		},
	}

	flag.Parse()
	log.SetFlags(log.Ltime)
	if *flagPass == "admin" {
		log.Printf("[安全] 管理界面默认账号 %s / 密码 admin —— 首次登录后请务必用 -pass 改成强密码", *flagUser)
	}

	if *flagSTRM != "" {
		strmRoot = *flagSTRM
	}
	// STRM 导出设置（与 items.json 同目录，网页可改）
	loadSTRMConfig(filepath.Join(filepath.Dir(*flagData), "strm_config.json"))

	store, err := NewStore(*flagData)
	if err != nil {
		log.Fatalf("加载数据失败: %v", err)
	}

	// 账号库与条目库放在同一目录
	acctPath := filepath.Join(filepath.Dir(*flagData), "accounts.json")
	as, err := NewAccountStore(acctPath)
	if err != nil {
		log.Fatalf("加载账号库失败: %v", err)
	}
	globalAccounts = as // 供各网盘 provider 取凭证
	globalStore = store // 供播放链路回写条目字段（如夸克转存后的 TVFID）

	// 全局播放设置（settings.json，与条目库同目录）
	loadAppSettings(settingsPath(*flagData))

	// 分享追更登记（shares.json，与条目同目录）
	if gs, err := NewShareStore(filepath.Join(filepath.Dir(*flagData), "shares.json")); err != nil {
		log.Fatalf("加载分享登记失败: %v", err)
	} else {
		globalShares = gs
	}

	// 追更定时器：到点自动重扫所有「追更中」的分享（默认 60 分钟一轮，-refresh=0 关闭）。
	// 手动刷新按钮不受影响，随时可点；两者共用同一套增量入库逻辑。
	self := *flagSelf
	if self == "" {
		self = detectSelf(*flagPort)
		log.Printf("自动探测本机地址: %s（若不对请用 -self 指定）", self)
	}
	startShareRefresher(store, time.Duration(*flagRefresh)*time.Minute, self)
	// 分享失效巡检：定时全量探测，确认失效后删除条目 + 登记 + STRM（默认 3 小时一轮）。
	startShareHealthChecker(store, time.Duration(*flagHealth)*time.Minute, self)

	// WebDAV 层每次读文件都要回环请求本机的加速引擎。
	// 这里必须用 127.0.0.1，而不是对外展示用的 self 地址（192.168.x.x）：
	// 容器内访问宿主机局域网 IP 要绕一圈 docker0 → 宿主 NAT（有代理 tun 时更绕），
	// 实测每次多花 0.2~1.6 秒且抖得厉害，播放器做几次 seek 就白等好几秒。
	// 对外展示的播放页地址仍用 self，不受影响。
	davSelf := "http://127.0.0.1:" + strconv.Itoa(*flagPort)

	dav := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: &vfsHandler{store: store, self: davSelf},
		LockSystem: webdav.NewMemLS(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/play", handlePlay(store, as))
	// 带文件名的形式（/play/电影名.mkv?id=xxx）：媒体服务器按 URL 后缀挑解码器，
	// 纯 /play?id= 没有后缀会被判成「未知格式」拒绝播放。路径部分仅作装饰。
	mux.HandleFunc("/play/", handlePlay(store, as))
	mux.HandleFunc("/watch", watchPage(store))
	mux.HandleFunc("/test", testPage())
	mux.HandleFunc("/api/items", apiItems(store, self))
	mux.HandleFunc("/api/items/", requireAuth(apiItemDelete(store)))
	mux.HandleFunc("/api/login/115/start", requireAuth(apiLoginStart))
	mux.HandleFunc("/api/login/115/poll", requireAuth(apiLoginPoll(as)))
	// 全局播放设置（夸克 TV 302 开关等）：GET 读 / POST 改，改完立即生效
	mux.HandleFunc("/api/settings", requireAuth(apiSettings()))
	mux.HandleFunc("/api/login/baidu/start", requireAuth(apiLoginStartBaidu))
	mux.HandleFunc("/api/login/baidu/poll", requireAuth(apiLoginPollBaidu(as)))
	// 夸克 TV 扫码登录（与夸克普通 Cookie 登录并存，同一 provider 两种登录方式）
	mux.HandleFunc("/api/login/quark/start", requireAuth(apiLoginStartQuark(as)))
	mux.HandleFunc("/api/login/quark/poll", requireAuth(apiLoginPollQuark(as)))
	mux.HandleFunc("/api/quark/import", requireAuth(apiQuarkImport(as)))
	mux.HandleFunc("/api/quark/list", requireAuth(apiQuarkList(as)))
	mux.HandleFunc("/api/quark/add", requireAuth(apiQuarkAdd(store, as)))
	mux.HandleFunc("/api/share/parse", requireAuth(apiShareParse(as)))
	mux.HandleFunc("/api/shares", requireAuth(apiShares(store)))
	mux.HandleFunc("/api/shares/toggle", requireAuth(apiShareToggle(store)))
	// 事后改分类：导入后发现分错了，搬目录 + 换追更登记 + 增量同步 STRM
	mux.HandleFunc("/api/shares/category", requireAuth(apiShareCategory(store, self)))
	mux.HandleFunc("/api/shares/", requireAuth(apiShareDelete(store)))
	mux.HandleFunc("/api/shows/delete", requireAuth(apiShowDelete(store, self)))
	mux.HandleFunc("/api/shares/refresh", requireAuth(apiShareRefresh(store, self)))
	// 分享失效巡检：GET 看最近一轮结果，POST 立即全量扫一轮
	mux.HandleFunc("/api/shares/health", requireAuth(apiShareHealth(store, self)))
	// 人工确认失效：立即清理该分享（不等巡检两轮）
	mux.HandleFunc("/api/shares/purge", requireAuth(apiSharePurge(store, self)))
	mux.HandleFunc("/api/share/add", requireAuth(apiShareAdd(store, self)))
	// STRM 导出：把虚拟盘镜像成 /data/strm 下的 .strm 目录树（飞牛影视媒体库用）
	mux.HandleFunc("/api/strm/rebuild", requireAuth(apiSTRMRebuild(store, self)))
	mux.HandleFunc("/api/strm/config", requireAuth(apiSTRMConfig(store, self)))
	mux.HandleFunc("/api/accounts/import", requireAuth(apiAccountImport(as)))
	mux.HandleFunc("/api/accounts", requireAuth(apiAccounts(as)))
	mux.HandleFunc("/api/accounts/", requireAuth(apiAccounts(as)))
	mux.Handle("/dav/", davGuard(dav, store, self))
	mux.Handle("/dav", davGuard(dav, store, self))
	// 302 直链模式：数据由客户端直接去网盘取，不经过本机
	mux.Handle("/direct/", directGuard(store))
	mux.Handle("/direct", directGuard(store))
	mux.HandleFunc("/api/link", requireAuth(apiLink(store)))
	mux.HandleFunc("/", indexPage(store, as, self))
	mux.HandleFunc("/login", loginHandler())

	addr := fmt.Sprintf(":%d", *flagPort)
	log.Printf("BigPan 启动 → %s", addr)
	log.Printf("管理界面 : %s", self)
	log.Printf("WebDAV  : %s/dav", self)
	log.Printf("直链302 : %s/direct （数据不过本机，适合流量/带宽受限的服务器）", self)
	log.Printf("默认参数 : 并发 %d 路 / 分片 %dMB / 重试 %d 次", *flagConc, *flagChunkMB, *flagRetry)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
