package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// PING 探测(spec Q18):每节点每轮 1 个 ICMP Echo,结果仅成功/失败并记录 RTT。
// 免特权优先(udp4/udp6;Linux 内核会把 Echo ID 改写为源端口并随回包还原),
// 失败回退原始套接字(容器授予 NET_RAW 即可)。不依赖 exec 系统 ping。
//
// IP 协议族(监控配置的 ipVersion,见 checkconfig.IPVersionXxx):ipv4/ipv6 时
// 只解析、只发该族;auto 由解析结果决定发 ICMPv4 还是 ICMPv6 —— 双栈目标此前
// 只能 ping IPv4,现在解析到 AAAA 也会用 ICMPv6 打一发。
func init() { Register(checkconfig.TypePing, executePing) }

// icmpProtocolNumber 是 icmp.ParseMessage 的协议号参数:ICMPv4=1、ICMPv6=58。
func icmpProtocolNumber(family int) int {
	if family == 6 {
		return 58
	}
	return 1
}

func executePing(ctx context.Context, raw json.RawMessage) (*protocol.ProbeResultPayload, error) {
	var cfg checkconfig.Ping
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析 PING 探测配置失败: %w", err)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ipVersion := cfg.IPVersion
	if !checkconfig.IPVersionValid(ipVersion) {
		ipVersion = checkconfig.IPVersionAuto
	}
	ip, err := resolveIP(callCtx, cfg.Host, ipVersion)
	if err != nil {
		return nil, err
	}
	// 地址族决定用哪套 ICMP:强制 ipv4/ipv6 时解析已经限定了族;auto 则跟随解析结果。
	family := 4
	if ip.To4() == nil {
		family = 6
	}
	conn, proto, err := listenICMP(family)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	if (proto == "udp4" || proto == "udp6") && kernelManglesEchoID() {
		// Linux/BSD 内核把 udp4/udp6 出口 ICMP 的 ID 改写为源端口,回包按端口匹配。
		if l, aerr := net.ResolveUDPAddr(proto, conn.LocalAddr().String()); aerr == nil {
			id = l.Port & 0xffff
		}
	}

	msg := &icmp.Message{
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: 1, Data: []byte("uptimemesh")},
	}
	if family == 6 {
		msg.Type = ipv6.ICMPTypeEchoRequest
	} else {
		msg.Type = ipv4.ICMPTypeEcho
	}
	body, err := msg.Marshal(nil)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	_ = conn.SetWriteDeadline(deadlineOf(callCtx))
	if _, err := conn.WriteTo(body, dstAddr(proto, ip)); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(deadlineOf(callCtx))
	reply := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(reply)
		if err != nil {
			return nil, fmt.Errorf("ICMP 无回包(超时或目标不可达): %w", err)
		}
		rtt := float64(time.Since(start).Nanoseconds()) / 1e6
		for _, m := range icmpCandidates(proto, family, reply[:n]) {
			echo, ok := m.Body.(*icmp.Echo)
			if !ok || echo.ID != id {
				continue
			}
			switch {
			case isEchoReply(family, m.Type):
				return &protocol.ProbeResultPayload{
					OK: true, LatencyMs: rtt,
					StartedAtUnix: start.Unix(), FinishedAtUnix: time.Now().Unix(),
				}, nil
			case isDestinationUnreachable(family, m.Type):
				return nil, fmt.Errorf("目标 %s 不可达(收到 ICMP 目的不可达)", cfg.Host)
			}
		}
		// 非本次探测相关的包,继续等(直到 deadline)。
	}
}

// isEchoReply / isDestinationUnreachable 按地址族判 ICMP 消息类型:
// ICMPv4 与 ICMPv6 的类型码是两套编号,不能混用。
func isEchoReply(family int, typ icmp.Type) bool {
	if family == 6 {
		return typ == ipv6.ICMPTypeEchoReply
	}
	return typ == ipv4.ICMPTypeEchoReply
}

func isDestinationUnreachable(family int, typ icmp.Type) bool {
	if family == 6 {
		return typ == ipv6.ICMPTypeDestinationUnreachable
	}
	return typ == ipv4.ICMPTypeDestinationUnreachable
}

// kernelManglesEchoID:Linux/BSD 的免特权 ICMP 套接字会做 ID↔端口改写;
// Windows 的 udp4 ICMP 走 IcmpCreateFile,不改写。
func kernelManglesEchoID() bool { return runtime.GOOS != "windows" }

// PingSupported 探测本机是否具备 ICMP(IPv4)能力(Windows 非管理员会失败;
// 集成测试据此跳过,真实验证在 Linux 容器 NET_RAW 下完成)。
func PingSupported() bool {
	c, _, err := listenICMP(4)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// listenICMP 返回连接、协议名与地址族;udp4/udp6 = 免特权路径,
// ip4:icmp / ip6:ipv6 = raw(需权限/NET_RAW)。
func listenICMP(family int) (*icmp.PacketConn, string, error) {
	if family == 6 {
		if c, err := icmp.ListenPacket("udp6", "::"); err == nil {
			return c, "udp6", nil
		}
		if c, err := icmp.ListenPacket("ip6:ipv6", "::"); err == nil {
			return c, "ip6:ipv6", nil
		}
		return nil, "", fmt.Errorf("ICMPv6 不可用:请确认系统允许非特权 ICMPv6(Linux 需 ping_group_range,容器需 NET_RAW)")
	}
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return c, "udp4", nil
	}
	if c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return c, "ip4:icmp", nil
	}
	return nil, "", fmt.Errorf("ICMP 不可用:请确认系统允许非特权 ICMP(Linux 需 ping_group_range,容器需 NET_RAW)")
}

func dstAddr(proto string, ip net.IP) net.Addr {
	if proto == "udp4" || proto == "udp6" {
		return &net.UDPAddr{IP: ip}
	}
	return &net.IPAddr{IP: ip}
}

// icmpCandidates 一条原始缓冲区可能给出多个 ICMP 消息解析结果:
// Linux raw(ip4:icmp / ip6:ipv6)回包含 IP 头,Windows 与 udp 路径不含;
// 全量与剥头两版都交给调用方匹配。
func icmpCandidates(proto string, family int, buf []byte) []*icmp.Message {
	protoNum := icmpProtocolNumber(family)
	var out []*icmp.Message
	if msg, err := icmp.ParseMessage(protoNum, buf); err == nil {
		out = append(out, msg)
	}
	raw := (proto == "ip4:icmp" && family == 4 && looksLikeIPv4(buf)) ||
		(proto == "ip6:ipv6" && family == 6 && looksLikeIPv6(buf))
	if raw {
		hdr := int(buf[0]&0x0f) * 4
		if family == 6 {
			hdr = 40 // IPv6 定长头
		}
		if len(buf) > hdr {
			if msg, err := icmp.ParseMessage(protoNum, buf[hdr:]); err == nil {
				out = append(out, msg)
			}
		}
	}
	return out
}

func looksLikeIPv4(buf []byte) bool {
	if len(buf) < 20 || buf[0]>>4 != 4 {
		return false
	}
	hdr := int(buf[0]&0x0f) * 4
	if hdr < 20 || len(buf) < hdr {
		return false
	}
	total := int(binaryBE16(buf[2:4]))
	return total >= hdr && total <= len(buf)+8 // 容忍截断
}

// looksLikeIPv6 只看版本号:IPv6 头定长 40 字节,不做长度校验(容忍截断)。
func looksLikeIPv6(buf []byte) bool { return len(buf) >= 40 && buf[0]>>4 == 6 }

func binaryBE16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// resolveIP 按 IP 协议族解析目标地址。
//
//	auto(或空)⇒ 与既有行为一致:字面地址原样用;域名优先取 A 记录,没有 IPv4 才用 AAAA;
//	ipv4/ipv6 ⇒ 只接受该族:字面地址族不符或域名没有该族记录都直接判失败
//	           (混合解析的主机名不会"悄悄"落到另一族,否则这条监控到底测了哪条链路说不清)。
func resolveIP(ctx context.Context, host, ipVersion string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		switch checkconfig.IPFamily(ipVersion) {
		case 4:
			if v4 := ip.To4(); v4 != nil {
				return v4, nil
			}
			return nil, fmt.Errorf("目标 %s 不是 IPv4 地址", host)
		case 6:
			if ip.To4() == nil && ip.To16() != nil {
				return ip, nil
			}
			return nil, fmt.Errorf("目标 %s 不是 IPv6 地址", host)
		default:
			return ip, nil
		}
	}
	network := "ip"
	switch checkconfig.IPFamily(ipVersion) {
	case 4:
		network = "ip4"
	case 6:
		network = "ip6"
	}
	r := net.Resolver{PreferGo: false}
	ips, err := r.LookupIP(ctx, network, host)
	if err != nil {
		return nil, fmt.Errorf("解析主机 %s 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("主机 %s 没有可用的 %s 地址", host, familyLabel(ipVersion))
	}
	// auto:优先 IPv4(与不配置时的历史行为一致),没有 IPv4 才退回 IPv6。
	if checkconfig.IPFamily(ipVersion) == 0 {
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				return v4, nil
			}
		}
	}
	return ips[0], nil
}

// familyLabel 把取值翻成给用户看的错误文本片段。
func familyLabel(ipVersion string) string {
	switch ipVersion {
	case checkconfig.IPVersion4:
		return "IPv4"
	case checkconfig.IPVersion6:
		return "IPv6"
	default:
		return "IP"
	}
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(10 * time.Second)
}
