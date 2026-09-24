// proxy.go 出站代理支持：按账号绑定的代理 URL 构造并缓存 Transport。
//
// 设计要点（参考社区 hub 的「每账号一个 opener 绑定出口 IP」）：
//   - **按代理 URL 缓存 Transport**，不是按账号 —— 多个账号共用同一代理槽位时
//     共享连接池（连接复用是代理场景下最实际的性能收益）；每账号独立 Transport
//     会在 20 个号共用同一代理时开出 20 套连接池，纯浪费。
//   - **空代理 = 直连**，回落到 New() 的那套共享 Transport（零行为变化）。
//   - http/https 走 `Transport.Proxy`（标准库原生）；socks5 走自定义 DialContext
//     （`x/net/proxy`），因为 socks5 不是 HTTP 代理，`Transport.Proxy` 不支持。
//
// 安全：代理 URL 里可能带用户名密码（`http://user:pass@host:port`）。本文件**不记录**
// 完整 URL 到日志（见 sanitizeProxyURL），只打协议与主机。
package upstream

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/proxy"

	"workbuddy2api/internal/auth"
)

// proxyConfig 归一化后的代理配置。
type proxyConfig struct {
	// raw 原始 URL（含可能的凭据），仅用于缓存键与拨号，**不打日志**。
	raw string
	// scheme 小写协议名：http / https / socks5 / socks5h
	scheme string
	u      *url.URL
}

// supportedProxySchemes 支持的代理协议。socks5h 与 socks5 等价处理
// （都让代理解析域名；Go 的 x/net/proxy 无远程解析开关，语义上更接近 socks5h）。
var supportedProxySchemes = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// parseProxy 解析并校验代理 URL。空串返回 (nil, nil) = 直连。
func parseProxy(raw string) (*proxyConfig, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 解析失败：%w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !supportedProxySchemes[scheme] {
		return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5/socks5h）", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理 URL 缺少主机：%s", sanitizeProxyURL(s))
	}
	return &proxyConfig{raw: s, scheme: scheme, u: u}, nil
}

// apply 把代理挂到 Transport 上。
//
// http/https：用标准库的 Proxy 钩子 —— 它会把请求（含 CONNECT 隧道）交给代理，
// 认证信息由 URL 的 userinfo 自动转成 Proxy-Authorization。
//
// socks5：Transport.Proxy 不支持 socks5，必须换 DialContext —— 由 x/net/proxy
// 建到 socks5 服务端的连接，再让 HTTP 层在这条连接上跑。
func (p *proxyConfig) apply(tr *http.Transport) {
	switch p.scheme {
	case "http", "https":
		tr.Proxy = http.ProxyURL(p.u)
	default: // socks5 / socks5h
		// x/net/proxy 的 socks5 不支持带认证的 URL 直接传，需显式构造 Auth。
		var authz *proxy.Auth
		if p.u.User != nil {
			pw, _ := p.u.User.Password()
			authz = &proxy.Auth{User: p.u.User.Username(), Password: pw}
		}
		// proxy.SOCKS5 的 dialer 参数（forward）传 nil：只做 TCP 转发，
		// 不需要它代做 DNS（我们目标是 HTTPS，域名由代理侧解析）。
		d, err := proxy.SOCKS5("tcp", p.u.Host, authz, newDialer())
		if err != nil {
			// 构造失败（极少见：参数不合法）时保持直连，不让整个客户端起不来。
			// 调用方在 setProxy 时已做过 parseProxy 校验，此处属防御分支。
			return
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			// x/net/proxy 的返回通常实现 ContextDialer；非 ContextDialer 时
			// 用 Dial 包一层（丢掉 ctx 取消，但保证能连）。
			tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	}
}

// sanitizeProxyURL 把代理 URL 里的凭据抹掉，供日志/错误文案使用。
// 形如 http://user:pass@host:port → http://host:port（保留协议与主机，便于排查）。
func sanitizeProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "<非法 URL>"
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// ── Transport 缓存 ──────────────────────────────────────────────────────

// proxyTransportCache 按代理 URL 缓存 Transport（含连接池）。
//
// 缓存键用**原始 URL**（含凭据）：同一主机不同凭据是不同代理身份，不能共用连接池。
var proxyTransportCache = struct {
	mu sync.Mutex
	m  map[string]*http.Client
}{m: map[string]*http.Client{}}

// transportForProxy 返回绑定到该代理的 http.Client（带缓存）。空串 = 直连，返回 nil
// 让调用方回落到共享的 c.HTTP / c.ChatHTTP。
//
// chat 与非 chat 共用同一 Transport（与直连形态一致：ChatHTTP 与 HTTP 共享 tr，
// 只是 Timeout 不同），故这里返回的 Client 用 Timeout=0，由调用方按用途决定超时。
func transportForProxy(raw string) (*http.Client, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	proxyTransportCache.mu.Lock()
	defer proxyTransportCache.mu.Unlock()
	if c, ok := proxyTransportCache.m[s]; ok {
		return c, nil
	}
	pc, err := parseProxy(s)
	if err != nil {
		return nil, err
	}
	tr := newBaseTransport(pc)
	// Timeout=0：与 ChatHTTP 同口径（聊天无总时长上限，首字节由
	// ResponseHeaderTimeout 管）。非 chat 路径的短 RPC 总时长由调用方
	// 各自控制（upstream 的 doJSON 用 HTTP.Timeout，这里返回的 client
	// 供两用，故不设总时长；见 clientFor 的用法）。
	c := &http.Client{Transport: tr}
	proxyTransportCache.m[s] = c
	return c, nil
}

// ResetProxyTransportCache 清空代理 Transport 缓存（代理槽位变更时调用）。
// 清空后下一次出站会按新配置重建连接池；旧连接池由 Go 的 GC 回收
// （Transport 无显式 Close，闲置连接靠 IdleConnTimeout 自然断开）。
func ResetProxyTransportCache() {
	proxyTransportCache.mu.Lock()
	proxyTransportCache.m = map[string]*http.Client{}
	proxyTransportCache.mu.Unlock()
}

// ── Client 侧的代理解析 ─────────────────────────────────────────────────

// SetProxyResolver 注入「账号 → 代理 URL」的解析函数（由 main 接线到 config 的绑定表）。
// nil 或返回空串 = 该账号直连。**热重载安全**：解析函数每次出站都调用，
// 故改绑定后无需重启（配合 ResetProxyTransportCache 清掉旧连接池）。
func (c *Client) SetProxyResolver(fn func(*auth.Auth) string) {
	c.proxyMu.Lock()
	c.proxyFor = fn
	c.proxyMu.Unlock()
}

// proxyForAccount 返回该账号应使用的代理 URL（空 = 直连）。
func (c *Client) proxyForAccount(a *auth.Auth) string {
	c.proxyMu.RLock()
	fn := c.proxyFor
	c.proxyMu.RUnlock()
	if fn == nil || a == nil {
		return ""
	}
	return strings.TrimSpace(fn(a))
}

// clientFor 返回该账号本次出站应使用的 http.Client（代理优先，回落共享 client）。
//
// fallback 为直连时使用的共享 client（c.HTTP 或 c.ChatHTTP）——保持既有语义：
// 直连路径逐字不变，只有配了代理的账号才走新链路。
//
// 代理构造失败（URL 非法等）时**回落直连并打一条 WARN**，而不是让请求失败：
// 代理配错不该让整个号不可用；但必须留痕，否则「以为走了代理其实没走」极难排查。
func (c *Client) clientFor(a *auth.Auth, fallback *http.Client) *http.Client {
	raw := c.proxyForAccount(a)
	if raw == "" {
		return fallback
	}
	pc, err := transportForProxy(raw)
	if err != nil {
		log.Printf("WARN: [upstream] 账号 %s 的代理不可用，回落直连：%v（proxy=%s）",
			labelOf(a), err, sanitizeProxyURL(raw))
		return fallback
	}
	if pc == nil {
		return fallback
	}
	// 短 RPC 的 120s 总时长在此单独施加（返回的 client 是 Timeout=0 的共享形态）。
	if fallback == c.HTTP && c.HTTP != nil {
		return &http.Client{Transport: pc.Transport, Timeout: c.HTTP.Timeout}
	}
	return pc
}
