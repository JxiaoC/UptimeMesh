// 探测用的 HTTP Transport 池:同一份拨号参数共享一份 Transport,让 keep-alive 连接
// 真正被复用,并把「一次探测一条连接、用完不关」的连接堆积连根去掉。
//
// 为什么必须有它:此前 runHTTP/runDownload 每次探测都新建零值 http.Transport ——
// 它没有 IdleConnTimeout(0 = 永不回收空闲连接)、也没人调 CloseIdleConnections;
// 响应读完连接退回该 Transport 的空闲池,而池随探测结束没人再管,readLoop 协程阻塞
// 在连接上把 fd 一直钉住(GC 收不走),只能等对端关空闲连接。结果:连接数与 goroutine
// 数随探测次数线性增长,线上表现为节点 fd 一路涨到数千、ss 里同一 host:port 几十条
// ESTABLISHED(见 .scratch/http-conn-reuse/spec.md)。
//
// 复用的分界 = transportParams 的四个字段,任一不同都不能共用同一份连接池:
//   - insecure:TLS 校验策略不同(跳过校验的连接不能给严格校验的监控用);
//   - network:IP 协议族不同(tcp/tcp4/tcp6),池必须分开,否则"强制 IPv6"的监控
//     可能拿到 IPv4 连接;
//   - vhost/dialAddr:「IP 直连 + Host 选源」的改写是逐目标的,混用会把 A 监控的
//     请求拨到 B 监控配置的那个 IP 上。
//
// 绝大多数监控(URL 是域名、auto/ipv4/ipv6)的参数只有 协议族×证书校验 几种组合,
// 因此实际缓存的 Transport 数量极小;缓存上限(maxCachedTransports)只是防御:
// 万一键空间异常膨胀,宁可退回"每次新建、用完即关",也不能让池无界增长。
package probe

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// transportParams 是「哪些探测可以共用连接池」的完整分界(可直接作 map 键)。
type transportParams struct {
	insecure bool   // TLS 跳过证书校验(监控的 allow_insecure_tls)
	network  string // 拨号网络名:tcp / tcp4 / tcp6(checkconfig.Network)
	vhost    string // Host 选源的虚拟主机名(URL 是字面 IP + 配了 Host 时非空)
	dialAddr string // vhost 对应的真实拨号地址(IP:端口)
}

// maxCachedTransports 是缓存上限:超过即不再缓存(调用方对返回的 Transport 自行
// CloseIdleConnections)。正常部署远到不了:键空间 ≈ 协议族×2 + 配了 Host 选源的监控数。
const maxCachedTransports = 256

var (
	transportMu    sync.Mutex
	transportCache = map[transportParams]*http.Transport{}
)

// transportFor 返回该参数组合可复用的 Transport。cached=false 表示缓存已满、这份
// 是一次性实例:调用方用完必须 CloseIdleConnections(否则退化回旧的连接堆积)。
func transportFor(p transportParams) (t *http.Transport, cached bool) {
	transportMu.Lock()
	defer transportMu.Unlock()
	if t, ok := transportCache[p]; ok {
		return t, true
	}
	t = newProbeTransport(p)
	if len(transportCache) >= maxCachedTransports {
		return t, false
	}
	transportCache[p] = t
	return t, true
}

// newProbeTransport 按参数构造一份 Transport。
//
// 参数口径与旧实现逐项对齐(除复用本身外不改任何行为):
//   - TLSClientConfig 仍只带 InsecureSkipVerify;另外挂 ClientSessionCache 让 HTTPS
//     在连接被关掉后也能用会话恢复跳过完整握手(旧实现每次新建 tls.Config,永远全握手);
//   - ForceAttemptHTTP2 保持不开:旧实现显式设了 TLSClientConfig 而没开它,Go 因此
//     不自动升 HTTP/2(net/http Issue 14275 的保守分支)。复用是这次的目标,
//     是否上 h2 属于另一个行为变更,不夹带;
//   - 拨号超时不在这里设:共享 Transport 不能带"某次探测"的超时,由请求的
//     context(每个探测自己的 callCtx)兜底,与旧实现的约束等价。
func newProbeTransport(p transportParams) *http.Transport {
	t := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: p.insecure, // 监控配置语义:自签证书可选放行
			// 同参数的所有探测共享一个会话缓存:HTTPS 目标重连时按 ticket 恢复会话。
			ClientSessionCache: tls.NewLRUClientSessionCache(0), // 0 = 默认容量(64)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		// 空闲连接 30s 没被复用就关:监控周期普遍 ≥30s,跨轮复用本来就是顺带的好处,
		// 这条主要是把「对端迟迟不关空闲连接」的残留上限死(配合下面的池上限)。
		IdleConnTimeout: 30 * time.Second,
		// 空闲池上限:单 host 4 条够覆盖一轮里并发打同一目标的场景,64 条封顶全局;
		// 在途连接不受限(MaxConnsPerHost 不设),不能让池上限把探测排队拖到超时,
		// 那会把"目标慢"误判成探测失败。
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 4,
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Host 头选源(curl --resolve 语义):只把"发往虚拟主机名"的连接换到配置里的
		// IP,跟随到别的域名的跳转照常解析(与旧实现同款,见 vhostTarget)。
		if p.dialAddr != "" {
			if host, _, err := net.SplitHostPort(addr); err == nil && strings.EqualFold(host, p.vhost) {
				addr = p.dialAddr
			}
		}
		// IP 协议族:auto 走 "tcp",ipv4/ipv6 收窄成 tcp4/tcp6。
		return dialer.DialContext(ctx, p.network, addr)
	}
	return t
}
