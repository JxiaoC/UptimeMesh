package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/alert"
	"github.com/uptimemesh/dashboard/internal/api"
	"github.com/uptimemesh/dashboard/internal/auth"
	"github.com/uptimemesh/dashboard/internal/geoip"
	"github.com/uptimemesh/dashboard/internal/hub"
	"github.com/uptimemesh/dashboard/internal/notifier"
	"github.com/uptimemesh/dashboard/internal/retention"
	"github.com/uptimemesh/dashboard/internal/scheduler"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/dashboard/internal/webhub"
	"github.com/uptimemesh/shared/protocol"
)

func main() {
	ctx := context.Background()
	// 环境变量优先(容器部署注入 SQLITE_PATH/SERVER_PORT),否则落回 config.yaml。
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = g.Cfg().MustGet(ctx, "sqlite.path", "data/uptimemesh.db").String()
	}

	st, err := store.New(ctx, dbPath)
	if err != nil {
		g.Log().Fatalf(ctx, "初始化存储失败: %v", err)
	}
	defer st.Close()

	key, err := st.EnsureSettings(ctx)
	if err != nil {
		g.Log().Fatalf(ctx, "初始化全局配置失败: %v", err)
	}
	if key != "" {
		fmt.Fprintf(os.Stderr, "\n=== 初始接入密钥(仅本次显示,请立即保存)===\n%s\n\n", key)
	}

	h := hub.New()
	web := webhub.New()
	jwtSecret, err := st.EnsureJWTSecret(ctx)
	if err != nil {
		g.Log().Fatalf(ctx, "初始化 JWT 密钥失败: %v", err)
	}
	// 本地地域库(GeoLite2 .mmdb):GEOIP_DB_PATH 未配置或打不开则停用自动
	// 解析(仅手动地域可用),不影响启动。日志带上库的构建日期——地域库按周更新,
	// 不报错但会慢慢过时,排查「解析不出地域」时这是第一个要看的信息。
	geo, err := geoip.Open(os.Getenv("GEOIP_DB_PATH"))
	if err != nil {
		g.Log().Warningf(ctx, "打开地域库失败(自动地域解析停用,可手动指定节点地域): %v", err)
		geo = nil
	} else if geo != nil {
		defer geo.Close()
		if built := geo.BuildDate(); built != "" {
			g.Log().Infof(ctx, "已加载地域库: %s(数据版本 %s)", os.Getenv("GEOIP_DB_PATH"), built)
		} else {
			g.Log().Infof(ctx, "已加载地域库: %s", os.Getenv("GEOIP_DB_PATH"))
		}
	} else {
		g.Log().Infof(ctx, "未配置 GEOIP_DB_PATH,节点地域仅支持手动指定")
	}
	app := &api.API{Store: st, Hub: h, JWT: auth.NewService(jwtSecret), Web: web, Geo: geo}

	// 节点上下线实时广播(票 10)。
	h.OnAgentChange = func(agentID store.ID, name string, online bool) {
		web.Broadcast(webhub.Event{Type: webhub.EvAgentChanged,
			Data: webhub.AgentChangedData{AgentID: agentID.Hex(), Name: name, Online: online}})
	}
	// 一键升级结果只落日志:成功时节点随即重启,新版本由下一次 hello 落库并在节点页
	// 体现;失败原因(无写权限、下载地址不可达、校验和不符)对排障很关键。
	h.OnUpgradeResult = func(ctx context.Context, agentID store.ID, p protocol.UpgradeResultPayload) {
		if p.OK {
			g.Log().Infof(ctx, "节点 %s 一键升级成功,当前版本 %s", agentID.Hex(), p.Version)
			return
		}
		g.Log().Warningf(ctx, "节点 %s 一键升级失败: %s", agentID.Hex(), p.Error)
	}

	sched := scheduler.New(st, h, st)
	// push(外部上报)监控的上报端点把结果交给调度器:轮次与告警只有一个写入者。
	app.PushSink = sched
	// 编辑保存 / 暂停后恢复时,让调度器立刻补一轮(不等满一个周期)。
	app.Kicker = sched
	// 节点(重新)接入后补发在途任务:重启后的首轮是在"节点还没连上"时下发的,
	// 节点随后重连回来时这一轮往往还开着,补发能把它救回成真实结果,而不是白等一个
	// 超时后按"未下发 ⇒ 不计分母"收口(见 scheduler.AgentOnline)。
	h.OnAgentConnected = func(agentID store.ID, _ string) { sched.AgentOnline(agentID) }
	n := notifier.New(st)
	sched.OnRoundFinalized = func(ctx context.Context, m *store.Monitor, agg scheduler.Aggregation, ev *alert.Event, monState *store.MonitorState) {
		// 每轮定稿推送浏览器(载荷含列表页增量更新所需的状态与状态条单元);
		// 告警翻转时另发 monitor_flipped 与 webhook。
		app.BroadcastRoundFinalized(ctx, m, agg, monState)
		if ev != nil {
			// 通知里带上这一轮的节点明细(谁失败、谁离线):告警只说"成功率 42.5%"
			// 值班的人还得自己去页面查是谁挂了。缺样名单由调度器给出(缺样没有结果行)。
			n.EmitMonitorEvent(ctx, m, *ev, notifier.MissingNodes{
				Alive: agg.MissingAlive, Dead: agg.MissingDead,
				Undispatched: agg.MissingUndispatched,
			})
			app.BroadcastMonitorFlipped(m, ev, agg, monState)
		}
	}
	go sched.Run(ctx)
	retention.New(st).Start(ctx)

	s := g.Server()
	app.Register(s)
	// 节点保活:周期下发 ping,让 Agent 能凭读空闲发现半开链路并重连;
	// 与离线扫描(心跳超时掐断)一正一反,共同维持两侧状态一致。
	h.StartKeepalive(ctx)
	app.StartOfflineMonitor(ctx)
	s.SetPort(serverPort(ctx))
	s.Run()
}

// serverPort 解析监听端口:SERVER_PORT 环境变量优先(容器由 compose 注入 8000),
// 未设置或非法时落回 config.yaml(本地 go run 可保持 5678,与容器端口互不冲突)。
func serverPort(ctx context.Context) int {
	cfgPort := g.Cfg().MustGet(ctx, "server.port", 8000).Int()
	env := os.Getenv("SERVER_PORT")
	port, ok := parsePort(env)
	if !ok {
		if env != "" {
			g.Log().Warningf(ctx, "SERVER_PORT=%q 非法,回退 config.yaml 端口 %d", env, cfgPort)
		}
		return cfgPort
	}
	return port
}

// parsePort 解析端口字符串;空串或非法(非数字、越界)返回 ok=false。
func parsePort(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}
