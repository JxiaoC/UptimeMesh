// Package checkconfig 定义监控(Monitor)各类型的探测配置,
// Dashboard 组装 ProbeTaskPayload.Check 时序列化,Agent 反序列化执行。
package checkconfig

const (
	TypeHTTP = "http"
	TypePing = "ping"
	// TypeTCP 是 TCP 端口连通性探测(对应 UptimeKuma 的 port/TCP Port 监控):
	// 能建立 TCP 连接即算成功。
	TypeTCP = "tcp"
	// TypePush 是外部上报型监控(对应 UptimeKuma 的 push 监控):没有 Agent 探测,
	// 由外部系统调用 Dashboard 的上报地址写结果;静默超时由调度器补失败轮次。
	TypePush = "push"
	// TypeDownload 是下载速度监控:URL 指向一个文件,Agent 完整下载一遍并测速,
	// 判定由 Dashboard 侧按「下载速度阈值」做(见 .scratch/download-speed-monitor/spec.md)。
	TypeDownload = "download"
)

// 下载速度单位。速度在整个系统里统一以 KB/s 存储与比较(Agent 也只回传 KB/s),
// 只有展示与配置输入按用户选中的单位换算 —— 免得"换成 MB/s 后历史阈值含义变了"。
const (
	SpeedUnitKBps = "KB/s"
	SpeedUnitMBps = "MB/s"
)

// SpeedUnits 是全部受支持的速度单位(表单下拉与校验共用,顺序固定)。
var SpeedUnits = []string{SpeedUnitKBps, SpeedUnitMBps}

// SpeedUnitValid 单位是否受支持(空串按 KB/s 处理,兼容未配置的存量数据)。
func SpeedUnitValid(unit string) bool {
	return unit == "" || unit == SpeedUnitKBps || unit == SpeedUnitMBps
}

// ToKbps 把某单位下的速度值换算为 KB/s(空单位按 KB/s)。
func ToKbps(v float64, unit string) float64 {
	if unit == SpeedUnitMBps {
		return v * 1024
	}
	return v
}

// FromKbps 把 KB/s 换算回指定单位,供展示使用(空单位按 KB/s)。
func FromKbps(kbps float64, unit string) float64 {
	if unit == SpeedUnitMBps {
		return kbps / 1024
	}
	return kbps
}

// IP 协议族(IP Version):监控可指定探测走哪一族,由节点在执行时落实。
//
//	auto(空串同样按 auto 处理)⇒ 交给系统解析与拨号,双栈主机由系统策略决定
//	                            (与不配置时的既有行为完全一致);
//	ipv4 / ipv6               ⇒ 只解析、只拨该族,目标没有该族地址即判失败
//	                            (混合解析的主机名不会"悄悄"落到另一族)。
//
// 注意它只约束**节点到目标**这一段:节点与 Dashboard 之间的长连接不受影响。
const (
	IPVersionAuto = "auto"
	IPVersion4    = "ipv4"
	IPVersion6    = "ipv6"
)

// IPVersions 是全部受支持的取值(表单下拉与校验共用,顺序固定:默认在最前)。
var IPVersions = []string{IPVersionAuto, IPVersion4, IPVersion6}

// IPVersionValid 取值是否受支持(空串按 auto 处理,兼容未配置的存量数据)。
func IPVersionValid(v string) bool {
	return v == "" || v == IPVersionAuto || v == IPVersion4 || v == IPVersion6
}

// Network 把 IP 协议族折算成网络名:auto 原样返回 base(如 "tcp"/"udp"),
// ipv4 ⇒ "tcp4"/"udp4",ipv6 ⇒ "tcp6"/"udp6"(net.Dialer 与 icmp.ListenPacket 同款约定)。
func Network(ipVersion, base string) string {
	switch ipVersion {
	case IPVersion4:
		return base + "4"
	case IPVersion6:
		return base + "6"
	default:
		return base
	}
}

// IPFamily 返回该取值强制使用的地址族:4、6,或 0 表示不强制(auto)。
func IPFamily(ipVersion string) int {
	switch ipVersion {
	case IPVersion4:
		return 4
	case IPVersion6:
		return 6
	default:
		return 0
	}
}

// HTTP 探测配置。ExpectStatusCodes 为空表示接受 2xx;
// ExpectContains/ExpectNotContains 任一规则不满足即判失败。
//
// JSON 断言(与 UptimeKuma 的 "HTTP(s) - JSON 查询" 语义一致,见 .scratch/json-assert/spec.md):
// 用 JSONata 表达式从响应中取值,再与期望值比较。JsonPath 为空表示不做该断言。
type HTTP struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers,omitempty"`
	Body              string            `json:"body,omitempty"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	ExpectStatusCodes []int             `json:"expect_status_codes,omitempty"`
	ExpectContains    []string          `json:"expect_contains,omitempty"`
	ExpectNotContains []string          `json:"expect_not_contains,omitempty"`
	AllowInsecureTLS  bool              `json:"allow_insecure_tls,omitempty"`
	// IPVersion 是本次探测使用的 IP 协议族(auto/ipv4/ipv6,见 IPVersionXxx)。
	IPVersion string `json:"ip_version,omitempty"`
	// JSONPath 是 JSONata 表达式(如 "data.status");"$" 表示原始响应。
	JSONPath string `json:"json_path,omitempty"`
	// JSONPathOperator 是取值与期望值的比较方式,见 OperatorXxx 常量;空按 == 处理。
	JSONPathOperator string `json:"json_path_operator,omitempty"`
	// ExpectedValue 是与取值比较的期望值(字符串比较)。
	ExpectedValue string `json:"expected_value,omitempty"`
}

// JSON 断言的比较运算符(与 UptimeKuma jsonPathOperator 取值一致)。
const (
	OperatorEqual    = "=="
	OperatorNotEqual = "!="
	OperatorContains = "contains"
	OperatorGT       = ">"
	OperatorGTE      = ">="
	OperatorLT       = "<"
	OperatorLTE      = "<="
)

// JSONAssertOperators 是全部受支持的比较运算符(展示与校验共用,顺序固定)。
var JSONAssertOperators = []string{
	OperatorEqual, OperatorNotEqual, OperatorContains,
	OperatorGT, OperatorGTE, OperatorLT, OperatorLTE,
}

// JSONAssertEnabled 是否需要执行 JSON 断言。
func (h HTTP) JSONAssertEnabled() bool { return h.JSONPath != "" }

// Ping 探测配置:每轮单 ICMP 包,成功/失败二值。
type Ping struct {
	Host           string `json:"host"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// IPVersion 见 IPVersionXxx:ipv4/ipv6 时只解析并只发该族(ICMP/ICMPv6)。
	IPVersion string `json:"ip_version,omitempty"`
}

// TCP 探测配置:与目标建立 TCP 连接即算成功(不做任何应用层交互)。
// 语义与 UptimeKuma 的「TCP Port」一致。
type TCP struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// IPVersion 见 IPVersionXxx:ipv4/ipv6 时只用该族建连。
	IPVersion string `json:"ip_version,omitempty"`
}

// Download 下载速度监控的探测配置:请求侧参数与 HTTP 完全相同(方法/请求头/请求体/
// 超时/忽略证书校验 + Host 头选源 + 跟随重定向),但**不下发判定**:节点只负责把 URL
// 指向的文件完整下载一遍并回报速度与字节数,是否达标由 Dashboard 按本轮平均速度与
// 监控配置的下载速度阈值比较(见 .scratch/download-speed-monitor/spec.md)。
//
// 不含期望状态码/关键字/JSON 断言:判定对象是速度而不是响应内容;节点侧按
// 「2xx 且完整读完响应体」判下载成功。
type Download struct {
	Method           string            `json:"method"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	Body             string            `json:"body,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	AllowInsecureTLS bool              `json:"allow_insecure_tls,omitempty"`
	// IPVersion 见 IPVersionXxx:ipv4/ipv6 时只解析并只拨该族。
	IPVersion string `json:"ip_version,omitempty"`
}
