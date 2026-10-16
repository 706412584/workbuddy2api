// proxy.go updater 出站代理：显式配置，不依赖启动环境变量。
//
// 为什么单独做（而不是用 net/http 的 ProxyFromEnvironment）：
// 启动脚本为避免「残留代理端口让 login 失败」会清空 HTTP_PROXY/HTTPS_PROXY
// （见 start.cmd 注释），而 updater 原先走 http.DefaultClient 只认 env ——
// GitHub 直连不通的机器上自更新必然卡在下载超时。
//
// 实测 2026-10-15：网关进程环境里只剩 HTTP_PROXY + ALL_PROXY（无 HTTPS_PROXY），
// 而 Go 的 ProxyFromEnvironment 对 https:// 只认 HTTPS_PROXY、**不认 ALL_PROXY**，
// 于是走直连，GitHub 下载 20s 超时；同一 URL 用 curl 走代理 1s 通。
//
// 语义与 upstream 的代理支持保持一致（http/https 走 Transport.Proxy，socks5 换
// DialContext），但本包**不 import upstream**：那个包带着 auth/pool 的传递依赖，
// 为解析一个代理 URL 把整条链拉进来不划算。这里的实现是自包含的精简版，只覆盖
// updater 需要的能力（无连接池缓存 —— 更新是低频操作，不需要）。
package updater

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// supportedProxySchemes 支持的代理协议。socks5h 与 socks5 等价处理（都让代理
// 解析域名；x/net/proxy 无远程解析开关，语义上更接近 socks5h）。
var supportedProxySchemes = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// newTransport 按代理配置构造 Transport；proxyRaw 为空 = 直连（Proxy 字段保持 nil）。
//
// noProxy 为逗号分隔的主机后缀白名单，命中则绕过代理。仅 http/https 代理支持
// 按主机绕过（Transport.Proxy 是「按请求」决策的）；socks5 走 DialContext，无法
// 按主机区分，故 socks5 形态下 noProxy 不生效（配置了也只在日志里提示）。
func newTransport(proxyRaw, noProxy string) (*http.Transport, error) {
	tr := &http.Transport{
		// 更新是低频操作，连接池保持默认即可；显式设置超时避免异常网络下无限挂起。
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	raw := strings.TrimSpace(proxyRaw)
	if raw == "" {
		return tr, nil // 直连
	}
	pc, err := parseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	switch pc.scheme {
	case "http", "https":
		// 标准库原生支持：Transport.Proxy 按请求决定是否走代理。
		matcher := newNoProxyMatcher(noProxy)
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			if matcher != nil && matcher(req.URL.Hostname()) {
				return nil, nil // 命中白名单 → 直连
			}
			return pc.u, nil
		}
	default: // socks5 / socks5h
		d, err := proxy.SOCKS5("tcp", pc.host, pc.auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("构造 socks5 代理失败：%w", err)
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	}
	return tr, nil
}

// proxySpec 解析后的代理描述。
type proxySpec struct {
	u      *url.URL
	scheme string
	host   string // host:port，socks5 用
	auth   *proxy.Auth
}

// parseProxyURL 解析并校验代理 URL。
func parseProxyURL(raw string) (*proxySpec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 非法：%w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !supportedProxySchemes[scheme] {
		return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5/socks5h）", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理 URL 缺少主机：%s", raw)
	}
	sp := &proxySpec{u: u, scheme: scheme, host: u.Host}
	if u.User != nil {
		pw, _ := u.User.Password()
		sp.auth = &proxy.Auth{User: u.User.Username(), Password: pw}
	}
	return sp, nil
}

// noProxyMatcher 报告主机是否命中 no_proxy 白名单。
// 支持精确主机名与后缀通配（"*.example.com" / ".example.com" → 匹配 example.com
// 及其子域，大小写不敏感）。
type noProxyMatcher func(host string) bool

func newNoProxyMatcher(raw string) noProxyMatcher {
	entries := splitNoProxy(raw)
	if len(entries) == 0 {
		return nil
	}
	return func(host string) bool {
		h := strings.ToLower(strings.TrimSuffix(host, "."))
		for _, e := range entries {
			if e == "*" {
				return true
			}
			// 去掉前导 "*." 或 "." 后按后缀比较；精确项则要求全等。
			if strings.HasPrefix(e, "*.") || strings.HasPrefix(e, ".") {
				suffix := strings.TrimPrefix(strings.TrimPrefix(e, "*"), ".")
				if h == suffix || strings.HasSuffix(h, "."+suffix) {
					return true
				}
				continue
			}
			if h == e {
				return true
			}
		}
		return false
	}
}

func splitNoProxy(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
