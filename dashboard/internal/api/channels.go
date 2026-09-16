package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/notifier"
	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/store"
)

// registerChannelRoutes 通知渠道 CRUD + 测试发送(票 07)。
func (a *API) registerChannelRoutes(group *ghttp.RouterGroup) {
	group.GET("/channels", a.listChannels)
	// 放在 /channels/{id} 之前,避免被当作 id 解析。
	group.GET("/channels/webhook-vars", a.channelWebhookVars)
	group.POST("/channels", a.createChannel)
	group.PUT("/channels/{id}", a.updateChannel)
	group.DELETE("/channels/{id}", a.deleteChannel)
	group.POST("/channels/{id}/test", a.testChannel)
	// 启用/禁用开关(渠道页列表行),与监控的 pause/resume 同款写法。
	group.POST("/channels/{id}/enable", func(r *ghttp.Request) { a.setChannelEnabled(r, true) })
	group.POST("/channels/{id}/disable", func(r *ghttp.Request) { a.setChannelEnabled(r, false) })
	// 把渠道加到所有监控的勾选里(渠道页的「设置为所有监控的渠道」)。
	group.POST("/channels/{id}/apply-all", a.applyChannelToAllMonitors)
}

type channelReq struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	BodyTemplate string `json:"bodyTemplate"`
	// Enabled 用指针区分「请求里没带这一项」与「显式传了 false」:
	// 新建时缺省 = 启用,编辑时缺省 = 保持原状态(不是"顺手禁用")。
	Enabled *bool `json:"enabled"`
}

func (req *channelReq) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	req.BodyTemplate = strings.TrimSpace(req.BodyTemplate)
	if req.Name == "" {
		return errors.New("渠道名称必填")
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("webhook URL 须为合法的 http(s) 地址")
	}
	if req.BodyTemplate == "" {
		return nil // 留空 = 发送默认通用 JSON
	}
	if unknown := notifytmpl.UnknownBodyPlaceholders(req.BodyTemplate); len(unknown) > 0 {
		return fmt.Errorf("不支持的模板变量: {{%s}};可用变量: {{%s}}",
			strings.Join(unknown, "}}、{{"), strings.Join(notifytmpl.BodyPlaceholders, "}}、{{"))
	}
	// 用示例值渲染后必须是合法 JSON,提前拦掉括号/引号/逗号错误。
	title, content := notifytmpl.SampleMessage()
	rendered := notifytmpl.RenderBody(req.BodyTemplate, notifytmpl.SampleVars(), title, content)
	if !json.Valid([]byte(rendered)) {
		return errors.New("请求体模板渲染后不是合法 JSON,请检查引号、逗号与括号是否配对")
	}
	return nil
}

func channelView(c *store.Channel) g.Map {
	return g.Map{
		"id": c.ID.Hex(), "name": c.Name, "url": c.URL,
		"bodyTemplate": c.BodyTemplate,
		"enabled":      c.Enabled,
		"createdAt":    fmtTime(c.CreatedAt),
	}
}

// channelWebhookVars 供渠道弹窗展示:可用模板变量、变量说明与示例渲染上下文。
// 变量清单以后端 notifytmpl 为准,避免前后端各写一份而漂移。
func (a *API) channelWebhookVars(r *ghttp.Request) {
	title, content := notifytmpl.SampleMessage()
	sample := notifytmpl.SampleVars()
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"placeholders": notifytmpl.BodyPlaceholders,
		"sample": g.Map{
			"event": sample.Event, "monitorId": sample.MonitorID, "monitorName": sample.MonitorName,
			"monitorType": sample.MonitorType, "url": sample.URL,
			"successRate": sample.SuccessRate, "errorCount": sample.ErrorCount,
			"timestamp": sample.Timestamp,
			"title":     title, "content": content,
		},
	}})
}

func (a *API) listChannels(r *ghttp.Request) {
	list, err := a.Store.ListChannels(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(list))
	for _, c := range list {
		out = append(out, channelView(c))
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": out})
}

func (a *API) createChannel(r *ghttp.Request) {
	var req channelReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	if err := req.validate(); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	c := &store.Channel{Name: req.Name, URL: req.URL, BodyTemplate: req.BodyTemplate,
		Enabled: req.Enabled == nil || *req.Enabled}
	if err := a.Store.InsertChannel(r.Context(), c); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "创建失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": channelView(c)})
}

func (a *API) updateChannel(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	c, err := a.Store.FindChannelByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "渠道不存在"})
		return
	}
	var req channelReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	if err := req.validate(); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": err.Error()})
		return
	}
	c.Name, c.URL, c.BodyTemplate = req.Name, req.URL, req.BodyTemplate
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if err := a.Store.UpdateChannel(r.Context(), c); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "更新失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": channelView(c)})
}

// setChannelEnabled 启用/禁用渠道(列表页开关)。禁用后自动告警与恢复通知不再投递,
// 监控侧的勾选保留(启用即刻恢复);「测试发送」不受状态影响。
func (a *API) setChannelEnabled(r *ghttp.Request, enabled bool) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	err := a.Store.SetChannelEnabled(r.Context(), id, enabled)
	if errors.Is(err, store.ErrNotFound) {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "渠道不存在"})
		return
	}
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "操作失败"})
		return
	}
	word := "已启用"
	if !enabled {
		word = "已禁用"
	}
	if c, e := a.Store.FindChannelByID(r.Context(), id); e == nil {
		r.Response.WriteJsonExit(g.Map{"code": 0, "message": word, "data": channelView(c)})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": word})
}

func (a *API) deleteChannel(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	// 删除渠道并同步从所有监控的勾选里摘除,避免悬空引用。
	err := a.Store.DeleteChannelDetach(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "渠道不存在"})
		return
	}
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "删除失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已删除"})
}

// applyChannelToAllMonitors 把渠道加到**所有**监控的通知渠道里
// (POST /channels/{id}/apply-all,渠道页的「设置为所有监控的渠道」)。
//
// 语义是"追加"不是"替换":已包含该渠道的监控跳过,各监控原有的其它渠道不受影响 ——
// 一次点击把每个监控的告警路由清成只剩这一个,是没人能接受的副作用。
// 改动要推给浏览器(EvMonitorsChanged):列表页把监控行缓存在内存里,编辑弹窗的渠道勾选
// 就来自那份缓存,不推的话用户在 30s 内编辑某个监控会把这次批量设置覆盖回去。
func (a *API) applyChannelToAllMonitors(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	c, err := a.Store.FindChannelByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "渠道不存在"})
		return
	}
	changed, total, err := a.Store.AttachChannelToMonitors(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "设置失败"})
		return
	}
	a.BroadcastMonitorsChanged(r.Context(), changed)
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"channelId": c.ID.Hex(), "channelName": c.Name,
		// affected 是真正改动的监控数;already 是本来就含该渠道、这次没动的。
		"affected": len(changed), "total": total, "already": total - len(changed),
	}})
}

// testChannel 同步发送一条 TEST 事件并回传结果。
func (a *API) testChannel(r *ghttp.Request) {
	id, ok := parseHexID(r, "id")
	if !ok {
		return
	}
	c, err := a.Store.FindChannelByID(r.Context(), id)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "渠道不存在"})
		return
	}
	// 测试发送不等重试,单次即回。
	fast := notifier.New(a.Store)
	fast.RetryDelays = nil
	if err := fast.EmitTest(r.Context(), c); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 502, "message": "测试发送失败: " + err.Error()})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "测试发送成功"})
}
