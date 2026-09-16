// Package protocol 固化 Agent 与 Dashboard 之间 WebSocket 长连接的 JSON 消息帧
// 契约(ADR-0001),由 dashboard 与 agent 双向复用。文档见 docs/protocol.md。
package protocol

import "encoding/json"

const (
	FrameHello       = "hello"
	FrameHelloAck    = "hello_ack"
	FrameCredential  = "credential"
	FrameHeartbeat   = "heartbeat"
	FrameProbeTask   = "probe_task"
	FrameProbeResult = "probe_result"
	// FrameProbeTest / FrameProbeTestResult 是「测试」链路:把弹窗里**还没保存**的探测
	// 配置交给某个在线节点跑一次。与 probe_task 的关键区别:不建轮次、不落库、
	// 不参与统计与告警,而且回传里带请求/响应明细(失败排障用)。
	FrameProbeTest       = "probe_test"
	FrameProbeTestResult = "probe_test_result"
	FramePing            = "ping"
	FramePong            = "pong"
	FrameError           = "error"
	// FrameUpgrade / FrameUpgradeResult 是「一键升级」链路:Dashboard 下发目标版本
	// 与校验和,Agent 自行下载、校验、替换自身并重启,再回一帧结果。
	FrameUpgrade       = "upgrade"
	FrameUpgradeResult = "upgrade_result"
)

// Envelope 是线帧的统一外壳。
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(frameType string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: frameType, Payload: raw}, nil
}

func Decode(payload json.RawMessage, v any) error {
	return json.Unmarshal(payload, v)
}

// Marshal 序列化一帧为线格式 JSON。
func Marshal(e Envelope) ([]byte, error) { return json.Marshal(e) }

// HelloPayload 由 Agent 在连接建立后立即发送。凭据二选一:首次接入用
// EnrollmentKey,已批准节点重连用 Credential。
type HelloPayload struct {
	EnrollmentKey string `json:"enrollment_key,omitempty"`
	Credential    string `json:"credential,omitempty"`
	AgentName     string `json:"agent_name"`
	AgentVersion  string `json:"agent_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	// Capabilities 声明本进程支持的可选能力(见 Cap* 常量)。老版本 Agent 不认识
	// upgrade 帧、收到也只会静默忽略,故「一键升级」必须靠能力协商下发,
	// 否则页面上的按钮对它们点不动。
	Capabilities []string `json:"capabilities,omitempty"`
	// IPv4Available / IPv6Available 是节点自报的本机网络族可用性:本机是否存在
	// 可用的 IPv4 / IPv6 出口地址(见 agentclient.DetectIPFamilies)。仪表盘用指针
	// 区分「上报了 false」与「老版本客户端根本没上报」——后者在节点页显示为未知,
	// 而不是"不支持"。
	IPv4Available *bool `json:"ipv4_available,omitempty"`
	IPv6Available *bool `json:"ipv6_available,omitempty"`
}

// CapSelfUpgrade 声明节点支持接收 upgrade 帧自升级(仅类 Unix 平台声明)。
const CapSelfUpgrade = "self_upgrade"

// CapProbeTest 声明节点支持接收 probe_test 帧(测试按钮)。
// 老版本节点不认识该帧、收到只会静默忽略,页面就只能干等到超时,因此仪表盘靠能力
// 协商挑节点(与自升级同理):在线节点都没这个能力时直接提示"需升级节点"。
const CapProbeTest = "probe_test"

const (
	AckPending  = "pending"  // 等待管理员批准,连接保持
	AckApproved = "approved" // 已批准节点
)

type HelloAckPayload struct {
	Status  string `json:"status"`
	AgentID string `json:"agent_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// CredentialPayload 在管理员批准后经既有连接下发,Agent 须持久化。
type CredentialPayload struct {
	AgentID    string `json:"agent_id"`
	Credential string `json:"credential"`
}

// HeartbeatPayload 每 10s 一次的存活信号。网络族可用性随心跳一起刷新(网卡/路由变了
// 不必等重连),Dashboard 只在取值变化时落库;老版本客户端不带这两个字段(指针为 nil)。
type HeartbeatPayload struct {
	Unix int64 `json:"unix"`
	// IPv4Available / IPv6Available 语义同 HelloPayload。
	IPv4Available *bool `json:"ipv4_available,omitempty"`
	IPv6Available *bool `json:"ipv6_available,omitempty"`
}

// PingPayload 由 Dashboard 发出用于探活(轮次缺样决策表);Agent 须原样回 Pong。
type PingPayload struct {
	PingID string `json:"ping_id"`
}

type PongPayload struct {
	PingID string `json:"ping_id"`
}

// ErrorPayload 在连接被拒绝等场景下发,随后服务端关闭连接。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	ErrAuthFailed = "auth_failed" // 接入密钥与凭据均无效
	ErrProtocol   = "protocol_error"
	ErrRevoked    = "credential_revoked"
)

// ProbeTaskPayload 为一次探测轮次向单个 Agent 的下发(轮次语义见 ADR-0003)。
type ProbeTaskPayload struct {
	RoundID      string          `json:"round_id"`
	MonitorID    string          `json:"monitor_id"`
	MonitorName  string          `json:"monitor_name"`
	MonitorType  string          `json:"monitor_type"` // http | ping
	Check        json.RawMessage `json:"check"`        // checkconfig.HTTP 或 checkconfig.Ping
	DeadlineUnix int64           `json:"deadline_unix"`
}

// UpgradePayload 由 Dashboard 下发:要求 Agent 把自身升级到 TargetVersion。
// 不含下载地址——Agent 按自己的 --server 接入地址推导分发端点(与 install.sh 同规则),
// 因此该地址在 Agent 自身网络位置一定可达;SHA256 是安装包内容校验和,
// Agent 必须校验通过才允许替换自身(空值视为非法指令,拒绝执行)。
type UpgradePayload struct {
	TargetVersion string `json:"target_version"`
	SHA256        string `json:"sha256"`
}

// UpgradeResultPayload 由 Agent 回传自升级结果(在下发后立即返回;成功后 Agent
// 随即重启,新版本会以一次新的 hello 重新接入)。
type UpgradeResultPayload struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"` // Agent 当前版本(成功即目标版本)
	Error   string `json:"error,omitempty"`
}

type ProbeResultPayload struct {
	RoundID        string  `json:"round_id"`
	MonitorID      string  `json:"monitor_id"`
	OK             bool    `json:"ok"`
	LatencyMs      float64 `json:"latency_ms"`
	HTTPStatus     int     `json:"http_status,omitempty"`
	Error          string  `json:"error,omitempty"`
	StartedAtUnix  int64   `json:"started_at_unix"`
	FinishedAtUnix int64   `json:"finished_at_unix"`
	// SpeedKbps 是下载速度监控测得的平均速度(KB/s,统一单位);其余类型为 0。
	// 下载失败时为 0(判定失败),Dashboard 侧按 0 计入本轮平均速度。
	SpeedKbps float64 `json:"speed_kbps,omitempty"`
	// Bytes 是本次下载的字节数(失败时是已读到的部分,排障用)。
	Bytes int64 `json:"bytes,omitempty"`
}

// ProbeTestPayload 一次测试下发。没有 round_id/monitor_id:测试不建轮次、不落库,
// 也不受监控是否已保存影响(弹窗里未保存的配置也要能测)。
type ProbeTestPayload struct {
	TestID      string          `json:"test_id"`      // 应答配对用(Dashboard 生成)
	MonitorType string          `json:"monitor_type"` // http | ping | tcp
	Check       json.RawMessage `json:"check"`        // checkconfig.HTTP / Ping / TCP
}

// ProbeTestResultPayload 测试结果。Detail 是"详细的请求与返回内容",由节点侧采集
// (判定只看 OK/Error,但排障时要有响应码、响应头与响应体片段)。
type ProbeTestResultPayload struct {
	TestID     string           `json:"test_id"`
	OK         bool             `json:"ok"`
	LatencyMs  float64          `json:"latency_ms"`
	HTTPStatus int              `json:"http_status,omitempty"`
	Error      string           `json:"error,omitempty"`
	Detail     *ProbeTestDetail `json:"detail,omitempty"`
	// SpeedKbps / Bytes 仅下载速度监控有值(平均速度 KB/s 与下载字节数):
	// 测试面板要能当场告诉用户"这条链路下这个文件有多快"。
	SpeedKbps float64 `json:"speed_kbps,omitempty"`
	Bytes     int64   `json:"bytes,omitempty"`
}

// ProbeTestDetail 一次测试的请求与响应明细。请求字段对 ping/tcp 只填 Target;
// 响应字段只在真拿到了 HTTP 响应时才齐全(连不上、TLS 失败、超时都只有 Error)。
type ProbeTestDetail struct {
	// ---- 请求 ----
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Target 是真正建连的目标:HTTP 走 Host 头选源(IP 直连 + 虚拟主机)时是那个 IP,
	// 其余情况与 URL 主机一致故留空;ping/tcp 则是"主机"或"主机:端口"。
	Target string `json:"target,omitempty"`
	// ---- 响应 ----
	Status        int               `json:"status,omitempty"`
	StatusText    string            `json:"status_text,omitempty"`
	RespHeaders   map[string]string `json:"resp_headers,omitempty"`
	BodyExcerpt   string            `json:"body_excerpt,omitempty"`
	BodyBytes     int64             `json:"body_bytes,omitempty"`
	BodyTruncated bool              `json:"body_truncated,omitempty"`
	// BodyNote 说明为什么没有正文(二进制内容、读取失败等)。
	BodyNote  string   `json:"body_note,omitempty"`
	FinalURL  string   `json:"final_url,omitempty"`
	Redirects []string `json:"redirects,omitempty"`
}
