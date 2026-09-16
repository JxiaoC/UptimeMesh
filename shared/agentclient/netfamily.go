package agentclient

import "net"

// DetectIPFamilies 判断本机是否具备可用的 IPv4 / IPv6 地址:遍历网络接口,
// 只要存在非回环、非未指定、非链路本地的地址,就认为该协议族可用。
//
// 为什么只看本地接口、不主动外连探测:节点常部署在内网或半离线环境,主动发探测包
// 既可能被防火墙静默丢弃,也会给监控系统自己引入外部依赖;而节点页要回答的问题
// 只是"这台机器有没有该族的地址可用"(用来解释为什么某条监控在它身上测不出 IPv6)。
//
// IPv6 特意排除链路本地地址(fe80::/10):它不能作为全球可达的源地址,
// 有它并不代表这条节点能用 IPv6 去探测目标。IPv4 的 169.254.0.0/16 同理。
func DetectIPFamilies() (ipv4, ipv6 bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, false
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip.To4() != nil {
			ipv4 = true
			continue
		}
		if ip.To16() != nil {
			ipv6 = true
		}
	}
	return ipv4, ipv6
}

// boolPtr 取布尔值的地址:协议里的可用性字段用指针区分「上报了 false」与「没上报」。
func boolPtr(b bool) *bool { return &b }

// ipFamilyPtrs 探测本机网络族并转成协议字段(hello 与心跳共用)。
func ipFamilyPtrs() (ipv4, ipv6 *bool) {
	v4, v6 := DetectIPFamilies()
	return boolPtr(v4), boolPtr(v6)
}
