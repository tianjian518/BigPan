package main

import "net/http"

// ---------------------------------------------------------------------------
// 连接池
//
// 为什么必须持有【多个独立客户端】：
//   HTTP/2 会把同一个客户端的并发请求多路复用到【一条 TCP 连接】上，
//   所有 stream 共享同一个拥塞窗口 —— 这样带宽不会叠加，
//   "开 32 路"实际上还是跑了 1 条连接。
//
//   要真正多连接，必须让不同分片走各自独立的客户端（各自一条 TCP 连接）。
//   夸克 CDN 又强制要求 HTTP/2，所以不能靠降级 h1 来解决。
// ---------------------------------------------------------------------------

type connPool struct {
	clients []*http.Client
}

// newConnPool 创建 n 个相互独立的 HTTP/2 客户端。
func newConnPool(n int, ua, cookie string) *connPool {
	if n < 1 {
		n = 1
	}
	if n > 256 {
		n = 256
	}
	p := &connPool{clients: make([]*http.Client, n)}
	for i := range p.clients {
		p.clients[i] = newHTTP2Client(ua, cookie)
	}
	return p
}

// get 返回第 i 个客户端；越界则取模。pool 为空时回落到全局客户端。
func (p *connPool) get(i int) *http.Client {
	if p == nil || len(p.clients) == 0 {
		return httpClient
	}
	if i < 0 {
		i = -i
	}
	return p.clients[i%len(p.clients)]
}
