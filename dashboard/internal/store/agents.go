package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 表名常量。
const (
	TableAgents        = "agents"
	TableMonitors      = "monitors"
	TableRounds        = "rounds"
	TableResults       = "results"
	TableHourlyStats   = "hourly_stats"
	TableChannels      = "channels"
	TableSettings      = "settings"
	TableUsers         = "users"
	TableMonitorStates = "monitor_states"
	// TableStateChanges 是告警状态变动记录(见 state_changes.go)。
	TableStateChanges = "monitor_state_changes"
)

// Agent 是节点接入记录。首连用接入密钥进来时为 pending,批准后签发凭据。
// CredPlain 暂存凭据明文直至节点首次凭据认证成功(证明已持有)后清除;
// 期间离线批准或送达失败都不丢凭据(自愈式下发)。
//
// Status=deleted 是伪删除态:记录保留(供历史轮次回显节点名),但默认不出现在
// 节点列表;同一台机器再用接入密钥接入时按新节点回到 pending。
type Agent struct {
	ID ID `json:"id"`
	// Name 是展示名,后台可改(见 .scratch/agent-rename/spec.md)。
	Name string `json:"name"`
	// EnrollName 是节点自报的接入名,只用于识别身份:与 SourceIP 组成唯一键,
	// 决定"这台机器再次带接入密钥连进来时是不是同一个节点"。后台改名不动它,
	// 否则节点回来领凭据时会认不出自己、凭空多出一条 pending。
	EnrollName     string `json:"enrollName,omitempty"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SourceIP       string `json:"sourceIp"`
	Status         string `json:"status"` // pending | approved | revoked | deleted
	CredentialHash string `json:"credentialHash,omitempty"`
	CredPlain      string `json:"credPlain,omitempty"`
	Country        string `json:"country,omitempty"`
	Region         string `json:"region,omitempty"`
	RegionIPCache  string `json:"regionIpCache,omitempty"`
	// Capabilities 是节点在 hello 里声明的可选能力(protocol.Cap*)。老版本 Agent
	// 不上报任何能力,据此把它与「认识 upgrade 帧」的新版本区分开。
	Capabilities []string `json:"capabilities,omitempty"`
	// IPv4Available / IPv6Available 是节点自报的本机网络族可用性(hello 与心跳都会
	// 带;见 shared/agentclient.DetectIPFamilies)。nil = 老版本 Agent 从未上报 ——
	// 页面显示"未知"而不是"不支持",两者含义完全不同。
	IPv4Available *bool     `json:"ipv4Available,omitempty"`
	IPv6Available *bool     `json:"ipv6Available,omitempty"`
	LastSeen      time.Time `json:"lastSeen"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Supports 判断节点是否声明了某项能力(如 protocol.CapSelfUpgrade)。
func (a *Agent) Supports(capability string) bool {
	for _, c := range a.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

const (
	AgentPending  = "pending"
	AgentApproved = "approved"
	AgentRevoked  = "revoked"
	// AgentDeleted 伪删除:记录保留但默认从节点列表隐藏。
	AgentDeleted = "deleted"
)

var ErrAgentRevoked = errors.New("节点已被吊销")

// agentCols agents 表的显式列顺序,与 scanAgent 一一对应。
const agentCols = `id, name, enroll_name, version, os, arch, source_ip, status, credential_hash,
	cred_plain, capabilities, ipv4_available, ipv6_available, country, region, region_ip_cache,
	last_seen, created_at, updated_at`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var (
		a         Agent
		credPlain sql.NullString
		caps      string
		ipv4      sql.NullInt64
		ipv6      sql.NullInt64
		lastSeen  int64
		createdAt int64
		updatedAt int64
	)
	err := row.Scan(&a.ID, &a.Name, &a.EnrollName, &a.Version, &a.OS, &a.Arch, &a.SourceIP, &a.Status,
		&a.CredentialHash, &credPlain, &caps, &ipv4, &ipv6, &a.Country, &a.Region,
		&a.RegionIPCache, &lastSeen, &createdAt, &updatedAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	a.CredPlain = trimNull(credPlain)
	a.Capabilities = parseCapabilities(caps)
	a.IPv4Available, a.IPv6Available = nullBool(ipv4), nullBool(ipv6)
	a.LastSeen = fromUnixSec(lastSeen)
	a.CreatedAt = fromUnixSec(createdAt)
	a.UpdatedAt = fromUnixSec(updatedAt)
	return &a, nil
}

// capabilitiesJSON 把能力列表序列化成库里的文本(空列表存空串,列是 NOT NULL DEFAULT ”)。
func capabilitiesJSON(caps []string) string {
	if len(caps) == 0 {
		return ""
	}
	b, err := json.Marshal(caps)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseCapabilities 还原能力列表;空串/坏 JSON 一律视为「没有声明能力」。
func parseCapabilities(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// InsertAgent 落库一个节点记录。首次落库时接入名即自报的展示名(改名是后台的事,
// 与接入无关);调用方若已显式填了 EnrollName 则以它为准。
func (s *Store) InsertAgent(ctx context.Context, a *Agent) error {
	now := time.Now()
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.EnrollName == "" {
		a.EnrollName = a.Name
	}
	a.CreatedAt, a.UpdatedAt, a.LastSeen = now, now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents (`+agentCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.EnrollName, a.Version, a.OS, a.Arch, a.SourceIP, a.Status, a.CredentialHash,
		nullStr(a.CredPlain), capabilitiesJSON(a.Capabilities),
		boolArg(a.IPv4Available), boolArg(a.IPv6Available),
		a.Country, a.Region, a.RegionIPCache,
		unixSec(a.LastSeen), unixSec(a.CreatedAt), unixSec(a.UpdatedAt))
	return normalizeErr(err)
}

// UpsertPendingAgent 带接入密钥的连接按 名称+来源IP 识别身份:
//   - 已批准(可能离线时被批准,还没领到凭据)⇒ 返回 approved,由调用方补发凭据;
//   - 已吊销 ⇒ 拒绝;
//   - 已删除(伪删除)⇒ 复活为 pending,重新走审批(删除即释放该身份);
//   - pending ⇒ 刷新版本/心跳并回填 ID(重连不刷屏);
//   - 首次 ⇒ 新建 pending。
func (s *Store) UpsertPendingAgent(ctx context.Context, a *Agent) (string, error) {
	existing, err := s.findAgentByIdentity(ctx, a.Name, a.SourceIP)
	if errors.Is(err, ErrNotFound) {
		// 新节点一律以 pending 落库:调用方只填自报信息,状态由存储层定。
		a.Status = AgentPending
		if err = s.InsertAgent(ctx, a); err != nil {
			return "", err
		}
		return AgentPending, nil
	}
	if err != nil {
		return "", err
	}

	now := unixSec(time.Now())
	switch existing.Status {
	case AgentRevoked:
		return "", ErrAgentRevoked
	case AgentApproved:
		_, err = s.db.ExecContext(ctx, `UPDATE agents SET version=?, os=?, arch=?, capabilities=?,
			ipv4_available=?, ipv6_available=?, last_seen=?, updated_at=? WHERE id=?`,
			a.Version, a.OS, a.Arch, capabilitiesJSON(a.Capabilities),
			boolArg(a.IPv4Available), boolArg(a.IPv6Available), now, now, existing.ID)
		if err != nil {
			return "", normalizeErr(err)
		}
		applyAgentRefresh(existing, a, now)
		*a = *existing
		return AgentApproved, nil
	case AgentDeleted:
		// 伪删除记录被同机重连复活:清凭据、回待审批。
		_, err = s.db.ExecContext(ctx, `UPDATE agents SET status=?, version=?, os=?, arch=?,
			capabilities=?, ipv4_available=?, ipv6_available=?,
			credential_hash='', cred_plain=NULL, last_seen=?, updated_at=? WHERE id=?`,
			AgentPending, a.Version, a.OS, a.Arch, capabilitiesJSON(a.Capabilities),
			boolArg(a.IPv4Available), boolArg(a.IPv6Available), now, now, existing.ID)
		if err != nil {
			return "", normalizeErr(err)
		}
		applyAgentRefresh(existing, a, now)
		existing.Status = AgentPending
		*a = *existing
		return AgentPending, nil
	default: // pending
		_, err = s.db.ExecContext(ctx, `UPDATE agents SET version=?, os=?, arch=?, capabilities=?,
			ipv4_available=?, ipv6_available=?, last_seen=?, updated_at=? WHERE id=?`,
			a.Version, a.OS, a.Arch, capabilitiesJSON(a.Capabilities),
			boolArg(a.IPv4Available), boolArg(a.IPv6Available), now, now, existing.ID)
		if err != nil {
			return "", normalizeErr(err)
		}
		applyAgentRefresh(existing, a, now)
		*a = *existing
		return AgentPending, nil
	}
}

// applyAgentRefresh 把重连带来的版本/系统信息刷到内存记录上,与刚写入库的值一致。
func applyAgentRefresh(existing, incoming *Agent, nowUnix int64) {
	existing.Version, existing.OS, existing.Arch = incoming.Version, incoming.OS, incoming.Arch
	existing.Capabilities = incoming.Capabilities
	existing.IPv4Available, existing.IPv6Available = incoming.IPv4Available, incoming.IPv6Available
	existing.LastSeen = fromUnixSec(nowUnix)
	existing.UpdatedAt = fromUnixSec(nowUnix)
}

// findAgentByIdentity 按「接入名 + 来源 IP」认身份。用 enroll_name 而不是 name:
// name 是后台可改的展示名,拿它当身份键会让改过名的节点在下次带接入密钥连进来时
// 认不出自己(见 .scratch/agent-rename/spec.md)。
func (s *Store) findAgentByIdentity(ctx context.Context, enrollName, sourceIP string) (*Agent, error) {
	row := s.dbRead.QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE enroll_name=? AND source_ip=?`, enrollName, sourceIP)
	return scanAgent(row)
}

// FindAgentByCredentialHash 凭据认证成功;revoked 记录即使 hash 命中也拒绝。
func (s *Store) FindAgentByCredentialHash(ctx context.Context, hash string) (*Agent, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	row := s.dbRead.QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE credential_hash=? AND status=?`,
		hash, AgentApproved)
	return scanAgent(row)
}

// ClearCredPlain 节点用凭据认证成功 = 证明已持有凭据,清除待发明文。
func (s *Store) ClearCredPlain(ctx context.Context, id ID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET cred_plain=NULL WHERE id=?`, id)
	return normalizeErr(err)
}

// ApproveAgent 批准:status→approved,写 credentialHash;明文暂存 credPlain,
// 经在线连接下发(每次 approved 连接都会补发,直到节点证明持有)。
func (s *Store) ApproveAgent(ctx context.Context, id ID, credential string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET status=?, credential_hash=?,
		cred_plain=?, updated_at=? WHERE id=?`,
		AgentApproved, HashSecret(credential), credential, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// RevokeAgent 吊销:清空凭据,重连即被拒。
func (s *Store) RevokeAgent(ctx context.Context, id ID) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET status=?, credential_hash='',
		cred_plain=NULL, updated_at=? WHERE id=?`,
		AgentRevoked, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// FindPendingAgents 返回待审批节点,按接入时间倒序。
func (s *Store) FindPendingAgents(ctx context.Context) ([]*Agent, error) {
	return s.findAgents(ctx, `status = ?`, AgentPending)
}

// FindAgentsByStatus 按状态过滤。
func (s *Store) FindAgentsByStatus(ctx context.Context, status string) ([]*Agent, error) {
	return s.findAgents(ctx, `status = ?`, status)
}

// FindAllAgents 返回全部未删除节点(伪删除默认隐藏)。
func (s *Store) FindAllAgents(ctx context.Context) ([]*Agent, error) {
	return s.findAgents(ctx, `status <> ?`, AgentDeleted)
}

// FindAllAgentsIncludingDeleted 返回含伪删除在内的全部节点。
// 供历史轮次/结果回显节点名:节点被删除后其历史结果仍应显示名称而非 ID。
func (s *Store) FindAllAgentsIncludingDeleted(ctx context.Context) ([]*Agent, error) {
	return s.findAgents(ctx, `1=1`)
}

func (s *Store) findAgents(ctx context.Context, where string, args ...any) ([]*Agent, error) {
	rows, err := s.dbRead.QueryContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, normalizeErr(rows.Err())
}

// FindAgentByID 按主键取节点;不存在返回 ErrNotFound。
func (s *Store) FindAgentByID(ctx context.Context, id ID) (*Agent, error) {
	row := s.dbRead.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id)
	return scanAgent(row)
}

// DeleteAgent 移除 pending 记录(硬删除;拒绝节点时使用)。
func (s *Store) DeleteAgent(ctx context.Context, id ID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)
	return normalizeErr(err)
}

// SoftDeleteAgent 伪删除:标记 status=deleted 并清空凭据。记录保留,历史结果
// 回显节点名仍可解析;默认节点列表隐藏该节点,同机重连时复活为 pending。
func (s *Store) SoftDeleteAgent(ctx context.Context, id ID) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET status=?, credential_hash='',
		cred_plain=NULL, updated_at=? WHERE id=?`,
		AgentDeleted, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// TouchAgentLastSeen 刷新最近心跳时间。
func (s *Store) TouchAgentLastSeen(ctx context.Context, id ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen=? WHERE id=?`, unixSec(at), id)
	return normalizeErr(err)
}

// RefreshAgentHello 用重连 hello 上报的版本、系统信息、能力列表与网络族可用性刷新记录。
// 「一键升级」依赖它:节点升级后以凭据重连,版本与能力变化只有落库节点页才看得到,
// 否则会一直提示可升级。调用方负责过滤空值(未上报的字段不覆盖原值)。
func (s *Store) RefreshAgentHello(ctx context.Context, id ID, version, goos, arch string,
	capabilities []string, ipv4, ipv6 *bool) error {
	now := unixSec(time.Now())
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET version=?, os=?, arch=?, capabilities=?,
		ipv4_available=?, ipv6_available=?, last_seen=?, updated_at=? WHERE id=?`,
		version, goos, arch, capabilitiesJSON(capabilities), boolArg(ipv4), boolArg(ipv6), now, now, id)
	return normalizeErr(err)
}

// SetAgentIPAvailability 记录心跳上报的网络族可用性(nil 表示该族本次未上报,
// 保持原值)。节点侧每 10s 探测一次,调用方只在取值变化时才落库,避免心跳写放大。
func (s *Store) SetAgentIPAvailability(ctx context.Context, id ID, ipv4, ipv6 *bool) error {
	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if ipv4 != nil {
		sets = append(sets, "ipv4_available=?")
		args = append(args, boolArg(ipv4))
	}
	if ipv6 != nil {
		sets = append(sets, "ipv6_available=?")
		args = append(args, boolArg(ipv6))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	return normalizeErr(err)
}

// SetAgentAutoCountry 记录按 IP 自动解析出的国家/地区码及其对应 IP;
// 手动覆盖 Region 不受影响(展示层 Region 优先)。
func (s *Store) SetAgentAutoCountry(ctx context.Context, id ID, country, forIP string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET country=?, region_ip_cache=?,
		updated_at=? WHERE id=?`, country, forIP, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// SetAgentRegion 手动指定/清除地域覆盖码(大写 alpha-2;空串 = 清除,回到自动解析)。
func (s *Store) SetAgentRegion(ctx context.Context, id ID, region string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET region=?, updated_at=? WHERE id=?`,
		region, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// SetAgentName 改节点的展示名。只动 name:enroll_name 是身份键,改了会让节点下次
// 带接入密钥连进来时被当成新节点(见 .scratch/agent-rename/spec.md)。
func (s *Store) SetAgentName(ctx context.Context, id ID, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET name=?, updated_at=? WHERE id=?`,
		name, unixSec(time.Now()), id)
	return normalizeErr(err)
}

// SetAgentSourceIPForTest 直接改写节点的来源 IP(仅供测试:模拟同一节点换网,
// 走真实 WS 握手做不到"同一个 ID 换个 IP 再连")。country/region_ip_cache 一并保留,
// 以便验证内网地址不会继续套用历史解析结果。
func (s *Store) SetAgentSourceIPForTest(ctx context.Context, id ID, ip string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET source_ip=?, updated_at=? WHERE id=?`,
		ip, unixSec(time.Now()), id)
	return normalizeErr(err)
}
