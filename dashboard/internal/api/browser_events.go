package api

import (
	"net/http"

	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gorilla/websocket"
)

// browserUpgrader 浏览器 WS 升级器。推送数据为只读监控状态(写操作仍走 JWT),
// 故放开跨域以支持 vite dev server(:5173)直连联调。
var browserUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleBrowserWS 浏览器实时推送端点 GET /ws/browser(票 10)。
// 事件契约见 webhub 包与 docs/protocol.md。
func (a *API) handleBrowserWS(r *ghttp.Request) {
	ws, err := browserUpgrader.Upgrade(r.Response.Writer, r.Request, nil)
	if err != nil {
		r.Exit()
		return // Upgrade 已写错误响应
	}
	a.Web.Serve(ws)
	r.Exit() // Serve 返回即会话结束,阻止 GoFrame 继续渲染
}
