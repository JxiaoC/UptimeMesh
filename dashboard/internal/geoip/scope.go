package geoip

import "net"

// Scope 描述一个来源 IP 的可达范围。
//
// 非公网地址不查地域库:库对它们本来就没有记录,而运维真正想知道的是
// 「这个节点在哪张网里」——是跟 Dashboard 同机,还是同一个内网,还是压根
// 没拿到地址的直连网段。展示层据此给出「本机 / 局域网 / 链路本地」。
type Scope string

const (
	// ScopePublic 公网地址:交给地域库解析国家/地区。
	ScopePublic Scope = "public"
	// ScopeLoopback 本机回环(127.0.0.0/8、::1):Agent 与 Dashboard 同机,
	// 是本地开发与「就地监控」最常见的形态。
	ScopeLoopback Scope = "loopback"
	// ScopeLAN 私有网段(RFC 1918 的 10/8、172.16/12、192.168/16,
	// 以及 IPv6 唯一本地地址 fc00::/7)。
	ScopeLAN Scope = "lan"
	// ScopeLinkLocal 链路本地(169.254.0.0/16、fe80::/10):通常是没拿到
	// DHCP 地址、或只在直连网段内可见的地址。
	ScopeLinkLocal Scope = "linklocal"
)

// ScopeOf 判断 ip 的可达范围。非法地址按 ScopePublic 返回——宁可让上层
// 显示「未知」,也不要把它误标成内网。
func ScopeOf(ip string) Scope {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ScopePublic
	}
	return scopeOf(parsed)
}

// scopeOf 是 ScopeOf 的已解析版本,供 Lookup 复用(避免同一个 IP 解析两次)。
func scopeOf(parsed net.IP) Scope {
	switch {
	case parsed.IsLoopback():
		return ScopeLoopback
	case parsed.IsLinkLocalUnicast():
		return ScopeLinkLocal
	case parsed.IsPrivate():
		return ScopeLAN
	}
	return ScopePublic
}
