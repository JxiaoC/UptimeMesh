package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/kuma"
	"github.com/uptimemesh/dashboard/internal/store"
)

// UptimeKuma 导入支持两种认证方式:
//
//	apikey   —— 默认。用 API 密钥读 /metrics(Prometheus),最省事,但只有
//	            名称/类型/URL/主机/端口/状态,关键词/请求方法/期望状态码等取不到;
//	password —— 用账号密码登录其 Socket.IO 管理接口(与 Kuma 自己的 Web UI 同一条路),
//	            拿到 monitorList 全量配置。凭据只在本次请求内使用,不落库。
const (
	kumaAuthAPIKey   = "apikey"
	kumaAuthPassword = "password"
)

// UptimeKuma 导入(设置页「导入 UptimeKuma」标签页):
//
//	POST /settings/import/uptimekuma/preview  只读预览:填 UptimeKuma 地址与凭据,
//	                                          取回监控清单并给出可导入/已存在/不支持统计(不落库);
//	POST /settings/import/uptimekuma          一键导入:按预览同一份数据批量建监控。
//
// 数据来源与映射规则见 internal/kuma。Dashboard 在此只做一次性的配置读取,
// 不执行任何检测(ADR-0002),导入结果不进入统计口径。
type kumaImportReq struct {
	BaseURL string `json:"baseUrl"`
	// AuthMode 认证方式:apikey(默认)或 password。
	AuthMode string `json:"authMode"`
	// APIKey UptimeKuma「设置 → API 密钥」创建的密钥(apikey 模式)。
	APIKey string `json:"apiKey"`
	// Username/Password/TwoFACode 为 password 模式的一次性凭据;不落库。
	Username  string `json:"username"`
	Password  string `json:"password"`
	TwoFACode string `json:"twoFACode"`
	// AllowInsecureTLS 拉取/连接时跳过证书校验(自签证书的 UptimeKuma)。
	AllowInsecureTLS bool `json:"allowInsecureTLS"`

	// 导入后落到每个监控上的默认配置。
	// 这些字段缺省(不传)时取下面的默认值;显式传 0 视为用户的真实取值并交
	// monitorReq.validate 校验(阈值 0 是合法配置,不能被默认值悄悄改写)。
	Group       string   `json:"group"`
	Period      *int     `json:"period"`
	Timeout     *int     `json:"timeout"`
	Threshold   *float64 `json:"threshold"`
	Consecutive *int     `json:"consecutive"`
	Enabled     *bool    `json:"enabled"`

	// 指派方式与新建监控一致(selected/all/exclude),默认 all。
	AssignMode       string   `json:"assignMode"`
	AssignedAgentIds []string `json:"assignedAgentIds"`
	ExcludedAgentIds []string `json:"excludedAgentIds"`
	ChannelIds       []string `json:"channelIds"`

	// SyncPaused 为真时,已存在的同目标监控会把"启用/暂停"对齐到 UptimeKuma 的
	// 当前状态(默认关闭:导入只新建,不改动已有监控)。
	SyncPaused bool `json:"syncPaused"`
	// Overwrite 为真时,已存在的同目标监控会用 UptimeKuma 的配置整体重写
	// (关键词/JSON 断言/周期/状态码/请求方法等),保留监控 ID 与历史轮次。
	// 用于修复早先导入留下的错误配置;默认关闭。
	Overwrite bool `json:"overwrite"`
}

// 导入默认周期/超时/阈值,与新建监控表单的默认值一致。
const (
	kumaDefaultPeriod      = 60
	kumaDefaultTimeout     = 10
	kumaDefaultThreshold   = 100
	kumaDefaultConsecutive = 3
)

// applyDefaults 补齐缺省字段;周期/超时/阈值仍由 monitorReq.validate 统一校验。
func (req *kumaImportReq) applyDefaults() {
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.AuthMode = strings.ToLower(strings.TrimSpace(req.AuthMode))
	if req.AuthMode != kumaAuthPassword {
		req.AuthMode = kumaAuthAPIKey
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.Username = strings.TrimSpace(req.Username)
	req.TwoFACode = strings.TrimSpace(req.TwoFACode)
	req.Group = strings.TrimSpace(req.Group)
	req.Period = orDefault(req.Period, kumaDefaultPeriod)
	req.Timeout = orDefault(req.Timeout, kumaDefaultTimeout)
	req.Threshold = orDefault(req.Threshold, kumaDefaultThreshold)
	req.Consecutive = orDefault(req.Consecutive, kumaDefaultConsecutive)
	if strings.TrimSpace(req.AssignMode) == "" {
		req.AssignMode = store.AssignModeAll
	}
	if req.AssignedAgentIds == nil {
		req.AssignedAgentIds = []string{}
	}
	if req.ExcludedAgentIds == nil {
		req.ExcludedAgentIds = []string{}
	}
	if req.ChannelIds == nil {
		req.ChannelIds = []string{}
	}
}

// orDefault 字段缺省时返回默认值的指针(泛型避免四种类型各写一遍)。
func orDefault[T any](v *T, def T) *T {
	if v == nil {
		return &def
	}
	return v
}

// registerKumaImportRoutes 挂载 UptimeKuma 导入相关的两个端点(均在 JWT 之后)。
func (a *API) registerKumaImportRoutes(group *ghttp.RouterGroup) {
	group.POST("/settings/import/uptimekuma/preview", a.kumaImportPreview)
	group.POST("/settings/import/uptimekuma", a.kumaImport)
}

// fetchKumaCandidates 按认证方式取回并映射为候选监控;
// 返回的第二项是数据来源的可读描述(/metrics 端点或 socket.io 地址),
// 第三项是 UptimeKuma 里被转成分组标签、因而不计入候选的「分组」容器数量。
func (a *API) fetchKumaCandidates(ctx context.Context, req kumaImportReq) ([]kuma.Candidate, string, int, error) {
	if req.AuthMode == kumaAuthPassword {
		return a.fetchKumaFullCandidates(ctx, req)
	}
	client := &kuma.Client{
		BaseURL:     req.BaseURL,
		APIKey:      req.APIKey,
		InsecureTLS: req.AllowInsecureTLS,
	}
	endpoint, err := client.MetricsURL()
	if err != nil {
		return nil, "", 0, err
	}
	monitors, err := client.Fetch(ctx)
	if err != nil {
		return nil, endpoint, 0, err
	}
	out := make([]kuma.Candidate, 0, len(monitors))
	for _, m := range monitors {
		out = append(out, kuma.MapToMesh(m))
	}
	// /metrics 里只有探测指标,没有分组容器。
	return out, endpoint, 0, nil
}

// fetchKumaFullCandidates 账号密码方式:登录 Socket.IO 管理接口取回完整监控配置。
func (a *API) fetchKumaFullCandidates(ctx context.Context, req kumaImportReq) ([]kuma.Candidate, string, int, error) {
	base, err := kuma.ParseBaseURL(req.BaseURL)
	if err != nil {
		return nil, "", 0, err
	}
	if req.Username == "" || req.Password == "" {
		return nil, "", 0, errors.New("账号密码方式须填写 UptimeKuma 用户名与密码")
	}
	source := base.Origin() + base.SocketPath()
	monitors, err := kuma.FetchFullViaPassword(ctx, req.BaseURL, kuma.PasswordLogin{
		Username: req.Username, Password: req.Password, TwoFACode: req.TwoFACode,
	}, req.AllowInsecureTLS)
	if err != nil {
		return nil, source, 0, err
	}
	// 批量映射:UptimeKuma 的「分组」容器会变成子监控的分组标签,容器本身不导入。
	return kuma.MapFullAll(monitors), source, kuma.CountGroupContainers(monitors), nil
}

// existingMonitorTargets 已存在监控的判重键 → 监控本身。
func (a *API) existingMonitorTargets(ctx context.Context) (map[string]*store.Monitor, error) {
	list, err := a.Store.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*store.Monitor, len(list))
	for _, m := range list {
		out[dedupeKeyOf(m)] = m
	}
	return out, nil
}

// dedupeKeyOf 已存在监控的判重键;与 kuma.Candidate.DedupeKey() 同一函数,
// 故 tcp(含端口)与 push(用名称)两边不会错位。
func dedupeKeyOf(m *store.Monitor) string {
	return kuma.DedupeKeyOf(m.Type, rawTargetOf(m), m.Port, m.Name)
}

// rawTargetOf 判重用的"裸目标":http 是 URL,ping/tcp 是主机名,push 为空。
func rawTargetOf(m *store.Monitor) string {
	if m.Type == kuma.MeshTypeHTTP {
		return m.URL
	}
	return m.TargetHost
}

// kumaImportPreview 只读预览:返回监控清单、可导入/已存在/不支持的数量,不写库。
func (a *API) kumaImportPreview(r *ghttp.Request) {
	var req kumaImportReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	req.applyDefaults()

	candidates, endpoint, groupContainers, err := a.fetchKumaCandidates(r.Context(), req)
	if err != nil {
		writeKumaFetchError(r, err)
		return
	}
	existing, err := a.existingMonitorTargets(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "读取已有监控失败"})
		return
	}

	items := make([]g.Map, 0, len(candidates))
	var importable, duplicated, unsupported, paused int
	// 本次预览内已出现过的目标:同一次导入里只会建第一条,预览也要如实标成重复,
	// 否则"可导入 N 个"与真正新建的数量会对不上。
	seen := map[string]bool{}
	for _, c := range candidates {
		key := c.DedupeKey()
		_, exists := existing[key]
		dup := exists || seen[key]
		if !c.Importable() {
			unsupported++
		} else if dup {
			duplicated++
		} else {
			importable++
			seen[key] = true
			// 在 UptimeKuma 里暂停的监控,导入后同样保持暂停(不受"立即启用"影响)。
			if c.Paused {
				paused++
			}
		}
		items = append(items, kumaItemView(c, dup))
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"endpoint":   endpoint,
		"authMode":   req.AuthMode,
		"total":      len(items),
		"importable": importable,
		"duplicate":  duplicated,
		// paused 是"可导入里在 Kuma 侧处于暂停"的数量:它们会以暂停状态建出来。
		"paused": paused,
		// UptimeKuma 的「分组」容器:变成子监控的分组标签,自身不计入监控总数。
		"groupContainers": groupContainers,
		"unsupported":     unsupported,
		"items":           items,
	}})
}

// kumaItemView 单条候选的展示结构(预览与导入结果共用)。
// duplicate 表示"这个目标已经存在或本次导入中已出现过",导入时会被跳过。
func kumaItemView(c kuma.Candidate, duplicate bool) g.Map {
	warnings := c.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	// push 没有探测目标:预览里不显示 Kuma 侧的名称当"目标"(它已经在名称列里)。
	target := c.Target
	if c.MeshType == kuma.MeshTypePush {
		target = ""
	}
	return g.Map{
		"kumaId": c.ID, "kumaType": c.Type, "name": c.Name,
		"type": c.MeshType, "target": target, "port": c.Port, "status": c.StatusText(),
		"importable": c.Importable(), "duplicate": duplicate,
		// paused 表示 Kuma 侧处于暂停:导入后保持暂停。
		"paused":   c.Paused,
		"warnings": warnings, "reason": c.Reason,
		// 账号密码方式带回来的实际配置(API 密钥方式为 0/空)。
		"method": c.Method, "period": c.Period, "invertMode": c.InvertMode,
		// Kuma 侧的分组(嵌套为"父 / 子");空表示未分组,导入时用表单里的分组兜底。
		"group": c.Group,
	}
}

// kumaImport 一键导入:不支持的类型与已存在的监控跳过,其余批量建为监控。
func (a *API) kumaImport(r *ghttp.Request) {
	var req kumaImportReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	req.applyDefaults()

	candidates, endpoint, _, err := a.fetchKumaCandidates(r.Context(), req)
	if err != nil {
		writeKumaFetchError(r, err)
		return
	}
	existing, err := a.existingMonitorTargets(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "读取已有监控失败"})
		return
	}

	enabled := req.Enabled == nil || *req.Enabled
	items := make([]g.Map, 0, len(candidates))
	var created, skipped, failed, synced, updated int
	for _, c := range candidates {
		key := c.DedupeKey()
		switch {
		case !c.Importable():
			skipped++
			items = append(items, g.Map{"name": c.Name, "ok": false, "message": c.Reason})
			continue
		case existing[key] != nil:
			old := existing[key]
			// 覆盖模式:用 Kuma 的配置整体重写已存在的同目标监控(保留 ID 与历史)。
			if req.Overwrite {
				// 指派等 UptimeMesh 侧独有设置用旧值参与校验,避免导入表单里的指派模式
				// 与旧监控不兼容(例如旧监控是"指定节点"、表单是"全部节点")而报错。
				overReq := req
				overReq.AssignMode = old.AssignMode
				overReq.AssignedAgentIds = old.AssignedAgentIds
				overReq.ExcludedAgentIds = old.ExcludedAgentIds
				m, err := a.kumaMonitor(r.Context(), overReq, c, enabled)
				if err != nil {
					failed++
					items = append(items, g.Map{"name": c.Name, "ok": false, "message": err.Error()})
					continue
				}
				// Kuma 里没有的概念(指派、通知渠道、阈值、连续轮数)保持用户本地的设置;
				// 启用状态也保持本地意图,只在 Kuma 侧暂停时改为暂停。
				m.ID, m.CreatedAt = old.ID, old.CreatedAt
				m.ChannelIds = old.ChannelIds
				m.Threshold = old.Threshold
				m.Consecutive = old.Consecutive
				m.Enabled = old.Enabled && !c.Paused
				if err = a.Store.UpdateMonitor(r.Context(), m); err != nil {
					failed++
					items = append(items, g.Map{"name": c.Name, "ok": false, "message": "覆盖更新失败"})
					continue
				}
				existing[key] = m
				updated++
				items = append(items, g.Map{"name": m.Name, "id": m.ID.Hex(), "type": m.Type,
					"target": urlOf(m), "ok": true,
					"message": "已存在,已按 UptimeKuma 的配置重写(指派/渠道/阈值保持本地设置)"})
				continue
			}
			// 对齐暂停状态:只改启用/暂停,不动其它配置。
			if req.SyncPaused && old.Enabled != !c.Paused {
				want := !c.Paused
				if err := a.Store.SetMonitorEnabled(r.Context(), old.ID, want); err != nil {
					failed++
					items = append(items, g.Map{"name": c.Name, "ok": false, "message": "同步暂停状态失败"})
					continue
				}
				synced++
				word := "已恢复为启用"
				if !want {
					word = "已改为暂停"
				}
				items = append(items, g.Map{"name": c.Name, "id": old.ID.Hex(),
					"ok": true, "message": "已存在,按 UptimeKuma 状态" + word})
				continue
			}
			skipped++
			items = append(items, g.Map{"name": c.Name, "ok": false,
				"message": "已存在相同目标的监控(或本次导入中重复),已跳过"})
			continue
		}
		m, err := a.kumaMonitor(r.Context(), req, c, enabled)
		if err != nil {
			failed++
			items = append(items, g.Map{"name": c.Name, "ok": false, "message": err.Error()})
			continue
		}
		if err = a.Store.InsertMonitor(r.Context(), m); err != nil {
			failed++
			items = append(items, g.Map{"name": c.Name, "ok": false, "message": "写入失败"})
			continue
		}
		// 同一次导入内的重复目标也要拦住(先入库的键立即生效)。
		existing[key] = m
		created++
		message := "已创建"
		if !m.Enabled {
			message = "已创建(在 UptimeKuma 中处于暂停,导入后同样暂停)"
		}
		items = append(items, g.Map{
			"name": m.Name, "id": m.ID.Hex(), "type": m.Type,
			"target": urlOf(m), "ok": true, "message": message,
		})
	}

	g.Log().Infof(r.Context(),
		"管理员 %s 从 %s 导入 UptimeKuma 监控:新建 %d、覆盖重写 %d、同步暂停状态 %d、跳过 %d、失败 %d",
		r.GetCtxVar(ctxUserKey).String(), endpoint, created, updated, synced, skipped, failed)
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"endpoint": endpoint,
		"total":    len(candidates),
		"created":  created,
		"updated":  updated,
		"synced":   synced,
		"skipped":  skipped,
		"failed":   failed,
		"items":    items,
	}})
}

// kumaMonitor 把候选映射并校验为可入库的监控文档。校验复用新建监控的
// monitorReq.validate,保证指派模式、周期/超时、URL 等口径完全一致。
func (a *API) kumaMonitor(ctx context.Context, req kumaImportReq, c kuma.Candidate, enabled bool) (*store.Monitor, error) {
	// 周期/超时:账号密码方式能取到 UptimeKuma 的实际取值,优先采用;
	// 取不到或越界(周期 10~3600、超时 ≤ 周期)时回落到导入参数。
	period, timeout := *req.Period, *req.Timeout
	if c.Period >= 10 && c.Period <= 3600 {
		period = c.Period
	}
	if c.Timeout >= 1 && c.Timeout <= period {
		timeout = c.Timeout
	}

	// 分组:UptimeKuma 侧有分组就沿用它的(Kuma 的 group 容器即 UptimeMesh 的分组),
	// 没有分组时才用导入参数里填的分组兜底。
	group := c.Group
	if strings.TrimSpace(group) == "" {
		group = req.Group
	}

	mr := monitorReq{
		Type: c.MeshType, Name: c.Name, Group: group,
		Period: period, Timeout: timeout,
		Threshold: *req.Threshold, Consecutive: *req.Consecutive,
		// UptimeKuma 的 Upside Down Mode 对应 UptimeMesh 的反转模式。
		InvertMode: c.InvertMode,
		AssignMode: req.AssignMode,
		AgentIds:   req.AssignedAgentIds, ExcludeIds: req.ExcludedAgentIds,
		ChannelIds: req.ChannelIds, Enabled: &enabled,
	}
	// 在 UptimeKuma 里暂停的监控,导入后同样保持暂停(不受导入参数里的"立即启用"影响)。
	effectiveEnabled := enabled && !c.Paused
	mr.Enabled = &effectiveEnabled
	if c.MeshType == kuma.MeshTypeHTTP {
		mr.URL = c.Target
		mr.Method = c.Method // 空则由 validate 回落 GET
		mr.Headers = c.Headers
		mr.Body = c.Body
		mr.ExpectStatusSpecs = c.ExpectStatusSpecs // 空则由 validate 回落 200
		mr.Contains = c.Contains
		mr.NotContains = c.NotContains
		mr.AllowInsecure = c.AllowInsecureTLS
		// JSON 断言:与 UptimeKuma 的 json-query 语义一致,表达式与运算符原样搬过来,
		// 由 monitorReq.validate 统一校验(表达式可编译、运算符在白名单内)。
		mr.JSONPath = c.JSONPath
		mr.JSONPathOperator = c.JSONPathOperator
		mr.ExpectedValue = c.ExpectedValue
	} else if c.MeshType == kuma.MeshTypeTCP {
		mr.TargetHost = c.Target
		mr.Port = c.Port
	} else if c.MeshType == kuma.MeshTypePush {
		// push 没有目标:令牌由 monitorReq.validate 生成,不影响其它字段。
	} else {
		mr.TargetHost = c.Target
	}
	m, err := mr.validate(ctx, a.Store)
	if err != nil {
		return nil, err
	}
	m.Enabled = effectiveEnabled
	return m, nil
}

// writeKumaFetchError 把取数/登录失败翻译成可操作的中文提示。
func writeKumaFetchError(r *ghttp.Request, err error) {
	code := 400
	msg := "读取 UptimeKuma 失败: " + err.Error()
	switch {
	case errors.Is(err, kuma.ErrUnauthorized):
		msg = "认证失败:请检查 API 密钥。密钥需在 UptimeKuma「设置 → API 密钥」中创建," +
			"并确认未被禁用或过期"
	case errors.Is(err, kuma.ErrNotFound):
		msg = "未找到 UptimeKuma 的 /metrics 端点:请确认地址指向 UptimeKuma 本体" +
			"(如 http://192.168.1.10:3001)"
	case errors.Is(err, kuma.ErrNoMonitors):
		msg = "已连上 UptimeKuma,但 /metrics 里没有任何监控:请确认已创建 API 密钥," +
			"且至少有一个监控产生过检测数据(暂停中的监控不会出现在指标里)"
	case errors.Is(err, kuma.ErrNoBetterAuth):
		msg = "未找到 UptimeKuma 的账号密码登录接口:请确认地址(含子路径)正确," +
			"反向代理需同时放行 /api/auth/* 与 /socket.io/*"
	case errors.Is(err, kuma.ErrSessionRejected):
		msg = "UptimeKuma 未接受本次登录会话:请确认用户名/密码与动态验证码正确后重试;" +
			"反复失败时可改用 API 密钥方式"
	case errors.Is(err, kuma.ErrLoginFailed):
		msg = "登录失败:" + err.Error() + ";请确认 UptimeKuma 的用户名与密码"
	case errors.Is(err, kuma.ErrTwoFARequired):
		msg = "该账号开启了两步验证,请填写动态验证码后重试"
	case errors.Is(err, kuma.ErrTwoFAInvalid):
		msg = "两步验证码不正确,请用当前动态验证码重试"
	case errors.Is(err, kuma.ErrLoginTimeout):
		msg = "已连上 UptimeKuma,但账号密码登录没有得到响应(login 事件与 better-auth " +
			"登录接口都不可用):请确认地址指向 UptimeKuma 本体、版本受支持,或改用 API 密钥方式"
	case errors.Is(err, kuma.ErrNoMonitorList):
		msg = "登录成功但没有收到监控列表:请确认该账号下有监控,且 UptimeKuma 版本受支持(1.x/2.x)"
	case isKumaNetworkError(err):
		code = 502
	}
	r.Response.WriteJsonExit(g.Map{"code": code, "message": msg})
}

// isKumaNetworkError 连接失败/上游非 200 时按网关错误返回,便于前端区分。
func isKumaNetworkError(err error) bool {
	return strings.HasPrefix(err.Error(), "无法连接 UptimeKuma") ||
		strings.HasPrefix(err.Error(), "UptimeKuma 返回 HTTP") ||
		strings.HasPrefix(err.Error(), "读取 UptimeKuma 指标失败") ||
		strings.HasPrefix(err.Error(), "连接或登录 UptimeKuma 超时")
}
