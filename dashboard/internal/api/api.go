// Package api 提供 REST 与 WebSocket HTTP 端点。
package api

import (
	"net/http"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/auth"
	"github.com/uptimemesh/dashboard/internal/geoip"
	"github.com/uptimemesh/dashboard/internal/hub"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
)

type API struct {
	Store *store.Store
	Hub   *hub.Hub
	JWT   *auth.Service
	Web   *webhub.Hub
	// Geo 为可选的本地地域库(.mmdb)解析器;nil 时节点列表只回显手动/已缓存地域,
	// 不做自动解析。进程内查询,零网络依赖。
	Geo *geoip.Resolver
	// PushSink 接收 push(外部上报)监控的上报,由调度器实现(cmd 里注入);
	// 为 nil 时上报端点返回 503。
	PushSink PushSink
	// Kicker 通知调度器"某监控该马上跑一轮"(编辑保存、暂停后恢复时调用),
	// 由调度器实现(cmd 里注入);为 nil 时忽略(轮次仍按周期正常产生)。
	Kicker RoundKicker
}

// Register 挂载路由。鉴权:除 /auth/* 与 /health 与 WS 与外部上报外,/api/v1/* 需 JWT。
func (a *API) Register(s *ghttp.Server) {
	a.registerStatic(s)

	s.BindHandler("/ws/agent", a.handleAgentWS)
	s.BindHandler("/ws/browser", a.handleBrowserWS)
	// 浏览器在 WS 上发来的请求帧(目前只有列表页「最近状态」快照)由本 API 应答。
	if a.Web != nil {
		a.Web.SetRequestHandler(a.HandleBrowserRequest)
	}
	// 外部上报入口:凭据是 URL 里的令牌,不能要求 JWT(调用方是路由器脚本等)。
	a.registerPushRoutes(s)
	s.Group("/api/v1", func(group *ghttp.RouterGroup) {
		group.GET("/health", func(r *ghttp.Request) { r.Response.Write("ok") })
		group.GET("/auth/status", a.authStatus)
		group.POST("/auth/init", a.authInit)
		group.POST("/auth/login", a.authLogin)
		// 节点安装包分发须免鉴权(目标机器 curl 拉取,无登录令牌)。
		a.registerAgentDistRoutes(group)
		group.Group("/", func(prot *ghttp.RouterGroup) {
			prot.Middleware(a.jwtAuth)
			prot.GET("/agents", a.listAgents)
			prot.POST("/agents/{id}/approve", a.approveAgent)
			prot.POST("/agents/{id}/reject", a.rejectAgent)
			prot.POST("/agents/{id}/revoke", a.revokeAgent)
			prot.POST("/agents/{id}/re-approve", a.reApproveAgent)
			// 一键升级:让节点升级到仪表盘当前分发的版本。
			prot.POST("/agents/{id}/upgrade", a.upgradeAgent)
			prot.PUT("/agents/{id}/region", a.updateAgentRegion)
			// 后台改展示名(不动身份键,见 updateAgentName)。
			prot.PUT("/agents/{id}/name", a.updateAgentName)
			prot.DELETE("/agents/{id}", a.deleteAgent)
			a.registerMonitorRoutes(prot)
			a.registerRoundRoutes(prot)
			a.registerChannelRoutes(prot)
			a.registerStatsRoutes(prot)
			a.registerSettingsRoutes(prot)
		})
	})
}

// ---- JWT 中间件 ----

const ctxUserKey = "auth.user"

func (a *API) jwtAuth(r *ghttp.Request) {
	reject := func(msg string) {
		r.Response.ClearBuffer()
		r.Response.WriteHeader(http.StatusUnauthorized)
		r.Response.WriteJson(g.Map{"code": 401, "message": msg})
		r.Exit()
	}
	token := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(token) <= len(prefix) || token[:len(prefix)] != prefix {
		reject("未登录")
	}
	user, err := a.JWT.Parse(token[len(prefix):])
	if err != nil {
		reject("登录已过期,请重新登录")
	}
	r.SetCtxVar(ctxUserKey, user)
	r.Middleware.Next()
}

// ---- auth 端点 ----

type authReq struct {
	Username string `json:"username" v:"required#用户名必填"`
	Password string `json:"password" v:"required|min-length:6#密码必填|密码至少 6 位"`
}

// authStatus 前端据此决定进登录页还是初始化引导页。
func (a *API) authStatus(r *ghttp.Request) {
	has, err := a.Store.HasAdmin(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "内部错误"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"initialized": has}})
}

// authInit 首次启动引导:创建管理员并直接返回 token。
func (a *API) authInit(r *ghttp.Request) {
	var req authReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	if has, _ := a.Store.HasAdmin(r.Context()); has {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "管理员账号已存在"})
		return
	}
	if err := a.Store.InitAdmin(r.Context(), req.Username, req.Password); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	a.writeToken(r, req.Username)
}

func (a *API) authLogin(r *ghttp.Request) {
	var req authReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	if err := a.Store.VerifyAdmin(r.Context(), req.Username, req.Password); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 401, "message": err.Error()})
		return
	}
	a.writeToken(r, req.Username)
}

func (a *API) writeToken(r *ghttp.Request, username string) {
	token, err := a.JWT.Issue(username)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "签发令牌失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"token": token}})
}

// ---- 节点接入(票 01/03)----

// handleAgentWS 升级节点连接并移交给 hub 的会话循环。
func (a *API) handleAgentWS(r *ghttp.Request) {
	ws, err := r.WebSocket()
	if err != nil {
		r.Response.ClearBuffer()
		r.Response.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	a.Hub.ServeWS(r.Context(), ws, a.Store, r.GetClientIp())
	r.Exit()
}
