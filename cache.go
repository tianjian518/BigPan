package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 播放路径的两层缓存
//
// 为什么必须缓存：
//   播放器（尤其安卓 / 电视端 / WebDAV 客户端）常常按「分段」读文件：
//   读 2MB → 断开 → 再读下一段。
//   如果每一段都要重新 ① 调网盘接口换直链 ② 探测源站 ③ 建 N 条 TLS 连接，
//   每段要多付 2~4 秒固定开销，实际速度会被拖到 0.5 MB/s ——
//   带宽明明够，却卡得放不动。
//
//   缓存后：第二段起几乎是「零准备」直接开始拉数据。
// ---------------------------------------------------------------------------

const playTargetTTL = 25 * time.Minute

// playTarget 一条播放目标的缓存：直链 + 取流所需信息 + 源站探测结果。
type playTarget struct {
	url    string
	ua     string
	cookie string
	size   int64
	ranged bool
	exp    time.Time
}

var (
	ptMu    sync.RWMutex
	ptCache = map[string]*playTarget{}
)

func getPlayTarget(id string) *playTarget {
	ptMu.RLock()
	defer ptMu.RUnlock()
	pt, ok := ptCache[id]
	if !ok || time.Now().After(pt.exp) {
		return nil
	}
	return pt
}

func putPlayTarget(id string, pt *playTarget) {
	ptMu.Lock()
	defer ptMu.Unlock()
	if len(ptCache) > 500 {
		now := time.Now()
		for k, v := range ptCache {
			if now.After(v.exp) {
				delete(ptCache, k)
			}
		}
	}
	ptCache[id] = pt
}

// dropPlayTarget 丢弃缓存（直链疑似过期时调用，下次请求会重新解析）。
func dropPlayTarget(id string) {
	ptMu.Lock()
	delete(ptCache, id)
	ptMu.Unlock()
}

// dropAllPlayTargets 清空全部播放直链缓存。
// 用在「换账号」时：旧凭证签发的直链属于旧号，必须全部丢弃，
// 否则用户换号后仍会拿到旧号的链接（甚至已失效的链接）。
func dropAllPlayTargets() {
	ptMu.Lock()
	ptCache = map[string]*playTarget{}
	ptMu.Unlock()
}

// ---------------------------------------------------------------------------
// 连接池复用
// ---------------------------------------------------------------------------

var (
	poolMu    sync.Mutex
	poolCache = map[string]*connPool{}
)

// getPool 按 (并发数, UA, Cookie) 复用连接池。
//
// 复用的意义：池里每个 client 都持有自己的 TCP/TLS 连接。
// 若每次请求都重建，等于每段都重新握手 N 次 —— 在夸克这种"强制 HTTP/2 +
// 海外/异地 CDN"的场景下，光握手就能吃掉一两秒。
func getPool(n int, ua, cookie string) *connPool {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", n, ua, cookie)))
	key := fmt.Sprintf("%x", sum[:8])

	poolMu.Lock()
	defer poolMu.Unlock()
	if p, ok := poolCache[key]; ok {
		return p
	}
	if len(poolCache) > 64 {
		poolCache = map[string]*connPool{}
	}
	p := newConnPool(n, ua, cookie)
	poolCache[key] = p
	return p
}

// 定期清理过期直链缓存，避免内存里堆积死链接。
func init() {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		for range t.C {
			now := time.Now()
			ptMu.Lock()
			for k, v := range ptCache {
				if now.After(v.exp) {
					delete(ptCache, k)
				}
			}
			ptMu.Unlock()
		}
	}()
}

// ---------------------------------------------------------------------------
// 文件头缓存
//
// 播放器（网易爆米花 / Filmly、电视端等）在真正开播前，普遍会从 0 偏移
// 反复请求几次：探容器格式、找关键帧、拿 MKV 的 EBML 头……
// 每次都要让夸克重新吐一遍头部数据，而夸克侧每次的首字节要 0.2~3.7 秒，
// 五六次叠加起来就是十几二十秒的「起播等待」。
//
// 这里把「从 0 开始的前几 MB」缓存下来：命中就毫秒级秒发，
// 剩下的数据从缓存末尾接着拉 —— 对客户端来说仍是一条完整的 0- 响应。
// ---------------------------------------------------------------------------

const headCacheSize = 4 * 1024 * 1024 // 每个条目最多缓存 4MB

var (
	headMu    sync.Mutex
	headCache = map[string][]byte{}
)

func getHeadCache(id string) []byte {
	headMu.Lock()
	defer headMu.Unlock()
	return headCache[id]
}

func putHeadCache(id string, b []byte) {
	headMu.Lock()
	defer headMu.Unlock()
	if len(headCache) > 24 {
		headCache = map[string][]byte{}
	}
	headCache[id] = b
}

// headTee 透传给客户端的同时把数据留存一份，用于填充文件头缓存。
// 实现了 http.Flusher，streamRange 的边写边刷不会被破坏。
type headTee struct {
	w   http.ResponseWriter
	buf []byte
	max int
}

func (t *headTee) Header() http.Header         { return t.w.Header() }
func (t *headTee) WriteHeader(code int)        { t.w.WriteHeader(code) }
func (t *headTee) Flush() {
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *headTee) Write(p []byte) (int, error) {
	if len(t.buf) < t.max {
		room := t.max - len(t.buf)
		if room > len(p) {
			room = len(p)
		}
		t.buf = append(t.buf, p[:room]...)
	}
	return t.w.Write(p)
}
