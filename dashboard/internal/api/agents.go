package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/geoip"
	"github.com/uptimemesh/dashboard/internal/hub"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
	"github.com/uptimemesh/shared/protocol"
)

// agentView 节点对外视图(票 03 起取代 listPending 成为统一节点列表)。
type agentView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SourceIP  string `json:"sourceIp"`
	Status    string `json:"status"`
	Online    bool   `json:"online"`
	Region    string `json:"region"`   // 生效地域码:手动覆盖优先,否则 IP 自动解析
	Manual    bool   `json:"manual"`   // region 来自手动指定(前端据此提示"已手动")
	NetScope  string `json:"netScope"` // 来源 IP 可达范围(geoip.ScopeOf):非 public 时 region 恒为空
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	// LatestVersion 是仪表盘对该平台可分发的 Agent 版本;空 = 未内置该架构安装包。
	LatestVersion string `json:"latestVersion"`
	// Outdated 表示节点版本与分发包不一致(即需要升级);是否在线不影响判定,
	// 前端据此在离线节点上也提示版本落后。
	Outdated bool `json:"outdated"`
	// Upgradable 表示能一键升级:Outdated + 节点声明了自升级能力。老版本 Agent
	// 不认识 upgrade 帧(收到也只会忽略),只能在其主机上重新执行安装命令。
	Upgradable bool `json:"upgradable"`
	// IPv4Available / IPv6Available 是节点自报的本机网络族可用性(见 hello/心跳);
	// null = 老版本 Agent 从未上报,页面显示「未知」而不是「不支持」。
	IPv4Available *bool `json:"ipv4Available"`
	IPv6Available *bool `json:"ipv6Available"`
}

// listAgents 支持 ?status=pending|approved|revoked 过滤;默认返回全部未删除节点。
// ?includeDeleted=1 附带伪删除节点(仅供历史结果回显名称,节点页不展示)。
func (a *API) listAgents(r *ghttp.Request) {
	var (
		agents []*store.Agent
		err    error
	)
	switch st := r.Get("status").String(); st {
	case store.AgentPending:
		agents, err = a.Store.FindPendingAgents(r.Context())
	case "":
		if r.Get("includeDeleted").Bool() {
			agents, err = a.Store.FindAllAgentsIncludingDeleted(r.Context())
		} else {
			agents, err = a.Store.FindAllAgents(r.Context())
		}
	default:
		agents, err = a.Store.FindAgentsByStatus(r.Context(), st)
	}
	if err != nil {
		g.Log().Errorf(r.Context(), "查询节点失败: %v", err)
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]agentView, 0, len(agents))
	for _, ag := range agents {
		out = append(out, a.toView(r.Context(), ag))
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

// effectiveRegion 返回节点的生效地域码:
//   - 手动覆盖(Region)非空 ⇒ 直接生效;
//   - 内网来源(本机/局域网/链路本地)⇒ 恒为空,既不查库也不回退历史值,
//     由 netScope 告诉前端该显示「本机 / 局域网 / 链路本地」;
//   - 其余公网来源按 SourceIP 查本地地域库(进程内内存操作,无网络);命中即回填库,
//     SourceIP 变化或解析结果更新时才写,查不到(无库/库未收录)回退上次回填值。
func (a *API) effectiveRegion(ctx context.Context, ag *store.Agent) (string, bool) {
	if ag.Region != "" {
		return ag.Region, true
	}
	// 内网地址必须在这里截断:否则节点从公网搬到内网(或本地开发用 127.0.0.1
	// 顶掉公网 IP)后,会一直挂着上一次解析出来的国家旗。
	if geoip.ScopeOf(ag.SourceIP) != geoip.ScopePublic {
		return "", false
	}
	country := a.Geo.Lookup(ag.SourceIP)
	if country == "" {
		return ag.Country, false
	}
	if country != ag.Country || ag.RegionIPCache != ag.SourceIP {
		if err := a.Store.SetAgentAutoCountry(context.WithoutCancel(ctx), ag.ID, country, ag.SourceIP); err != nil {
			g.Log().Warningf(ctx, "落库节点 %s 地域失败: %v", ag.ID, err)
		}
	}
	return country, false
}

func (a *API) toView(ctx context.Context, ag *store.Agent) agentView {
	region, manual := a.effectiveRegion(ctx, ag)
	// 与 region 分开取:内网节点 region 为空但 netScope 非 public,前端据此
	// 显示「本机 / 局域网 / 链路本地」,不必再回头解析 IP。
	scope := geoip.ScopeOf(ag.SourceIP)
	dist, hasDist := findAgentDist(ctx, ag.OS, ag.Arch)
	latest := ""
	if hasDist {
		latest = dist.Version
	}
	// 「需要升级」有两种情形:
	//   1) 版本号与分发目录里的分发包不一致;
	//   2) 版本号虽然一致,但二进制比能力协商还旧 —— 镜像构建时 AGENT_VERSION 是个固定值
	//      (0.1.0),改了 Agent 侧探测行为却没 bump 版本时,老二进制会一直显示"已是最新"
	//      (线上踩过:重定向跟随修好后,09-11 装的两个旧 Agent 仍把 302 判成失败)。
	//      声明能力不需要版本号配合,而分发目录只有 Linux 包(hasDist 已保证 goos=linux),
	//      所以「Linux 节点一个能力都不声明」本身就说明它比能力协商旧,必须重装。
	// 能不能一键升级还取决于节点是否声明了自升级能力(旧二进制只会静默忽略 upgrade 帧)。
	staleBuild := hasDist && len(ag.Capabilities) == 0
	outdated := hasDist && ag.Status == store.AgentApproved && (ag.Version != dist.Version || staleBuild)
	return agentView{
		ID: ag.ID.Hex(), Name: ag.Name, Version: ag.Version,
		OS: ag.OS, Arch: ag.Arch, SourceIP: ag.SourceIP, Status: ag.Status,
		Online:        a.Hub.IsOnline(ag.ID),
		Region:        region,
		Manual:        manual,
		NetScope:      string(scope),
		FirstSeen:     fmtTime(ag.CreatedAt),
		LastSeen:      fmtTime(ag.LastSeen),
		LatestVersion: latest,
		Outdated:      outdated,
		Upgradable:    outdated && ag.Supports(protocol.CapSelfUpgrade),
		IPv4Available: ag.IPv4Available,
		IPv6Available: ag.IPv6Available,
	}
}

// updateAgentRegion 手动修改节点地域:{"region": "CN"};空串清除手动值、回到 IP 自动解析。
// 国家/地区码用 ISO 3166-1 alpha-2(大写),港澳台分别为 HK/MO/TW(展示层译名)。
func (a *API) updateAgentRegion(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status == store.AgentDeleted {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "已删除节点不可修改地域"})
		return
	}
	region := strings.ToUpper(strings.TrimSpace(r.Get("region").String()))
	if region != "" && !regionRe.MatchString(region) {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "地域码须为 2 位字母(如 CN),或留空以恢复自动解析"})
		return
	}
	if err := a.Store.SetAgentRegion(r.Context(), id, region); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "保存失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已保存", "data": g.Map{"region": region}})
}

// regionRe 校验 alpha-2 地域码形状;具体码是否真实存在由前端下拉保证。
var regionRe = regexp.MustCompile(`^[A-Z]{2}$`)

// agentNameMaxRunes 是展示名的长度上限(按 rune 计)。节点名多半来自主机名(上限 63),
// 留一点余量;这里只是防呆,不参与身份判定。
const agentNameMaxRunes = 64

// updateAgentName 后台修改节点的展示名:{"name": "北京-机房-01"}。
//
// 只改展示名,不动接入名(enroll_name):后者是「接入名 + 来源IP」身份键的一半,
// 决定节点下次拿接入密钥连进来时算不算同一个节点。改它会让改名后的节点回连时
// 认不出自己(见 store.SetAgentName 与 .scratch/agent-rename/spec.md)。
func (a *API) updateAgentName(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status == store.AgentDeleted {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "已删除节点不可修改名称"})
		return
	}
	name := strings.TrimSpace(r.Get("name").String())
	if name == "" {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "节点名称不能为空"})
		return
	}
	if utf8.RuneCountInString(name) > agentNameMaxRunes {
		r.Response.WriteJsonExit(g.Map{
			"code": 400, "message": fmt.Sprintf("节点名称最长 %d 个字符", agentNameMaxRunes)})
		return
	}
	if err := a.Store.SetAgentName(r.Context(), id, name); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "保存失败"})
		return
	}
	// 在线节点的连接里缓存着名字,上下线广播带的正是它;改名后立刻对齐,
	// 免得广播里还在用旧名(前端收到事件会重拉列表,但事件载荷本身不该是过期值)。
	a.Hub.RenameConn(id, name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已保存", "data": g.Map{"name": name}})
}

// upgradeAgent 一键升级:向在线节点下发 upgrade 帧(目标版本 + 安装包校验和),
// 节点自行按接入地址推导下载地址、校验内容、原子替换自身并原地重启。
//
// Dashboard 只负责「告知目标版本」:二进制仍走公开的安装包分发端点(与一键安装
// 同一条链路),既不需要代传文件,也不需要在节点上开放额外端口。真正生效的是
// 节点侧对 sha256 的强制校验——校验不过就拒绝替换。
func (a *API) upgradeAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentApproved {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅已批准节点可一键升级"})
		return
	}
	dist, hasDist := findAgentDist(r.Context(), ag.OS, ag.Arch)
	if !hasDist {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仪表盘未内置该架构的 Agent 安装包,无法一键升级"})
		return
	}
	if ag.Version == dist.Version {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "节点已是当前分发版本 " + dist.Version})
		return
	}
	// 能力协商:老版本 Agent 不认识 upgrade 帧(收到只会静默忽略),点了也不会动,
	// 必须在页面上说清楚,而不是下发一条注定无效的指令。
	if !ag.Supports(protocol.CapSelfUpgrade) {
		r.Response.WriteJsonExit(g.Map{"code": 400,
			"message": "该节点运行的 Agent 版本过旧,不支持一键升级,请在节点主机上重新执行安装命令"})
		return
	}
	if !a.Hub.IsOnline(id) {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "节点不在线,升级指令无法送达"})
		return
	}
	env, err := protocol.NewEnvelope(protocol.FrameUpgrade, protocol.UpgradePayload{
		TargetVersion: dist.Version, SHA256: dist.SHA256,
	})
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "构造升级指令失败"})
		return
	}
	if err := a.Hub.SendToAgent(id, env); err != nil {
		g.Log().Warningf(r.Context(), "节点 %s 升级指令下发失败: %v", id.Hex(), err)
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "升级指令下发失败,节点可能刚离线"})
		return
	}
	g.Log().Infof(r.Context(), "节点 %s(%s)一键升级已下发: %s → %s",
		ag.Name, id.Hex(), ag.Version, dist.Version)
	r.Response.WriteJsonExit(g.Map{
		"code": 0, "message": "升级指令已下发,节点将自动重启并在数秒后以新版本重连",
		"data": g.Map{"targetVersion": dist.Version},
	})
}

// approveAgent 批准节点:签发专属凭据;节点在线则立即下发,离线则待其重连补发。
func (a *API) approveAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentPending {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅待审批节点可批准"})
		return
	}
	cred := hub.NewCredential()
	if err := a.Store.ApproveAgent(r.Context(), id, cred); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "批准失败"})
		return
	}
	// 在线则立即下发(凭据欠账在 session 建连时也会补发,此处主动推一次降低延迟)。
	if _, online := a.Hub.Get(id); online {
		env, err := protocol.NewEnvelope(protocol.FrameCredential, protocol.CredentialPayload{
			AgentID: id.Hex(), Credential: cred,
		})
		if err == nil {
			_ = a.Hub.SendToAgent(id, env)
		}
	}
	a.broadcastAgent(id, ag.Name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已批准"})
}

// broadcastAgent 审批/拒绝/吊销等状态变化后通知浏览器(票 10);前端收到即重拉列表。
func (a *API) broadcastAgent(id store.ID, name string) {
	if a.Web == nil {
		return
	}
	_, online := a.Hub.Get(id)
	a.Web.Broadcast(webhub.Event{Type: webhub.EvAgentChanged, Data: webhub.AgentChangedData{
		AgentID: id.Hex(), Name: name, Online: online,
	}})
}

// rejectAgent 拒绝:删除 pending 记录(节点重连会重新进 pending)。
func (a *API) rejectAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentPending {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅待审批节点可拒绝"})
		return
	}
	if err := a.Store.DeleteAgent(r.Context(), id); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "删除失败"})
		return
	}
	a.broadcastAgent(id, ag.Name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已拒绝"})
}

// revokeAgent 吊销:凭据立即失效、活动连接掐断;保留记录供审计,可重新批准。
func (a *API) revokeAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentApproved {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅已批准节点可吊销"})
		return
	}
	if err := a.Store.RevokeAgent(r.Context(), id); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "吊销失败"})
		return
	}
	a.Hub.ForceDisconnect(id)
	a.broadcastAgent(id, ag.Name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已吊销"})
}

// reApproveAgent 重新批准被吊销的节点。
func (a *API) reApproveAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentRevoked {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅已吊销节点可重新批准"})
		return
	}
	cred := hub.NewCredential()
	if err := a.Store.ApproveAgent(r.Context(), id, cred); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "批准失败"})
		return
	}
	if _, online := a.Hub.Get(id); online {
		env, err := protocol.NewEnvelope(protocol.FrameCredential, protocol.CredentialPayload{
			AgentID: id.Hex(), Credential: cred,
		})
		if err == nil {
			_ = a.Hub.SendToAgent(id, env)
		}
	}
	a.broadcastAgent(id, ag.Name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已批准"})
}

// deleteAgent 删除已吊销节点。伪删除:标记 status=deleted,默认列表不再展示,
// 但记录保留以便历史轮次/结果回显节点名;同机重连时复活为待审批。
func (a *API) deleteAgent(r *ghttp.Request) {
	id, ok := parseAgentID(r)
	if !ok {
		return
	}
	ag, err := a.Store.FindAgentByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "节点不存在"})
		return
	}
	if ag.Status != store.AgentRevoked {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "仅已吊销节点可删除"})
		return
	}
	if err := a.Store.SoftDeleteAgent(r.Context(), id); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "删除失败"})
		return
	}
	a.Hub.ForceDisconnect(id)
	a.broadcastAgent(id, ag.Name)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已删除"})
}

func parseAgentID(r *ghttp.Request) (store.ID, bool) {
	id, err := store.IDFromHex(r.Get("id").String())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "节点 ID 非法"})
		return "", false
	}
	return id, true
}

// StartOfflineMonitor 周期扫描:节点超过 OfflineAfter 未发心跳则掐断其半开连接,
// 使在线判定(心跳×丢失计数)与连接生命周期一致。
func (a *API) StartOfflineMonitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, id := range a.Hub.OnlineAgentIDs() {
					if !a.Hub.IsOnline(id) {
						a.Hub.ForceDisconnect(id)
					}
				}
			}
		}
	}()
}
