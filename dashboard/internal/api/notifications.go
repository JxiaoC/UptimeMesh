package api

import (
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/store"
)

// registerNotificationRoutes 通知记录查询(分页 + 筛选)。
// 记录由 notifier 在每次投递结束时写入,这里只读、无删除入口:
// 数据永久保留,不受「原始结果保留(天)」清理的影响。
func (a *API) registerNotificationRoutes(group *ghttp.RouterGroup) {
	group.GET("/notifications", a.listNotifications)
}

// notificationView 组装列表页一行。标题/正文可能多行,原样返回、由前端截断展示。
func notificationView(nl *store.NotificationLog) g.Map {
	return g.Map{
		"id": nl.ID.Hex(), "channelId": nl.ChannelID.Hex(), "channelName": nl.ChannelName,
		"monitorId": nl.MonitorID.Hex(), "monitorName": nl.MonitorName,
		"event": nl.Event, "status": nl.Status, "attempts": nl.Attempts,
		"title": nl.Title, "content": nl.Content, "error": nl.Error,
		"createdAt": fmtTime(nl.CreatedAt),
	}
}

// listNotifications GET /notifications?monitorId=&channelId=&event=&status=&page=&pageSize=
// 默认第 1 页、每页 20 条,pageSize 上限 100;返回分页列表与总数。
func (a *API) listNotifications(r *ghttp.Request) {
	page := r.Get("page").Int()
	if page <= 0 {
		page = 1
	}
	pageSize := r.Get("pageSize").Int()
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	f := store.NotificationLogFilter{
		Event:  r.Get("event").String(),
		Status: r.Get("status").String(),
	}
	// 筛选 ID 非法时按"没有这条记录"处理成空结果,而不是整表照出:
	// 用户点的是一个坏链接/已不存在的引用,返回全量反而误导。
	if hexID := r.Get("monitorId").String(); hexID != "" {
		id, err := store.IDFromHex(hexID)
		if err != nil {
			r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"list": []g.Map{}, "total": 0}})
			return
		}
		f.MonitorID = id
	}
	if hexID := r.Get("channelId").String(); hexID != "" {
		id, err := store.IDFromHex(hexID)
		if err != nil {
			r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"list": []g.Map{}, "total": 0}})
			return
		}
		f.ChannelID = id
	}
	list, total, err := a.Store.ListNotificationLogs(r.Context(), f, pageSize, (page-1)*pageSize)
	if err != nil {
		g.Log().Errorf(r.Context(), "查询通知记录失败: %v", err)
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "查询失败"})
		return
	}
	out := make([]g.Map, 0, len(list))
	for _, nl := range list {
		out = append(out, notificationView(nl))
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"list": out, "total": total}})
}
