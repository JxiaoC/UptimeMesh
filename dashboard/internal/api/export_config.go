package api

import (
	"context"
	"fmt"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/configfile"
	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/store"
	"github.com/uptimemesh/shared/checkconfig"
)

// 配置文件导出(设置页「配置导入导出」标签页)。设计见
// .scratch/config-import-export/spec.md,格式与版本策略见 internal/configfile。
//
//	GET /settings/export/config             信封模式(前端用它下载)
//	GET /settings/export/config?download=1  原始文件流(附件头,便于 curl / 定时备份)
//
// 两种模式共用 buildConfigFile,不存在两份口径。
func (a *API) exportConfig(r *ghttp.Request) {
	file, warnings, err := a.buildConfigFile(r.Context(), time.Now())
	if err != nil {
		g.Log().Errorf(r.Context(), "导出配置文件失败: %v", err)
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "导出失败"})
		return
	}
	filename := configExportFilenameOf(time.Now())
	if r.Get("download").Bool() {
		// 原始文件流:管理员用 curl/cron 直接落盘,不必自己从信封里挑 config 字段。
		raw, err := configfile.Encode(file)
		if err != nil {
			r.Response.WriteJsonExit(g.Map{"code": 500, "message": "导出失败"})
			return
		}
		r.Response.Header().Set("Content-Type", "application/json; charset=utf-8")
		r.Response.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		r.Response.Write(raw)
		return
	}
	g.Log().Infof(r.Context(), "管理员 %s 导出配置文件:监控 %d、渠道 %d、自定义模板 %d",
		r.GetCtxVar(ctxUserKey).String(), len(file.Monitors), len(file.Channels), templateCount(file))
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"filename":      filename,
		"exportedAt":    file.ExportedAt,
		"configVersion": file.ConfigVersion,
		"counts": g.Map{
			"monitors":  len(file.Monitors),
			"channels":  len(file.Channels),
			"templates": templateCount(file),
		},
		// warnings 是「源实例本身有点小问题」的提示(悬空渠道引用、同名渠道),
		// 不影响文件可用性,但导出时如实告知。
		"warnings": warnings,
		"config":   file,
	}})
}

// templateCount 文件里自定义通知模板的个数(未自定义的事件回落内置默认,不计入)。
func templateCount(f *configfile.File) int {
	if f.Settings == nil || f.Settings.NotifyTemplates == nil {
		return 0
	}
	return len(*f.Settings.NotifyTemplates)
}

// buildConfigFile 把库内的可迁移配置组装成配置文件。返回的警告用于提示源实例里的
// 悬空引用与同名渠道(它们不影响导出,但会影响导入时的对齐结果)。
func (a *API) buildConfigFile(ctx context.Context, now time.Time) (*configfile.File, []string, error) {
	monitors, err := a.Store.ListMonitors(ctx)
	if err != nil {
		return nil, nil, err
	}
	channels, err := a.Store.ListChannels(ctx)
	if err != nil {
		return nil, nil, err
	}
	st, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil, nil, err
	}

	var (
		warnings []string
		file     = configfile.NewFile(now)
	)
	file.Channels = make([]configfile.Channel, 0, len(channels))
	nameByID := make(map[string]string, len(channels))
	seenName := make(map[string]int, len(channels))
	for _, c := range channels {
		enabled := c.Enabled
		file.Channels = append(file.Channels, configfile.Channel{
			Name: c.Name, URL: c.URL, BodyTemplate: c.BodyTemplate, Enabled: &enabled,
		})
		nameByID[c.ID.Hex()] = c.Name
		if n := seenName[c.Name]; n > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"本地存在 %d 个同名渠道「%s」,导入时按名字只会对齐到最早创建的那一个", n+1, c.Name))
		}
		seenName[c.Name]++
	}

	file.Monitors = make([]configfile.Monitor, 0, len(monitors))
	for _, m := range monitors {
		entry := monitorEntry(m)
		// 渠道引用按**名字**导出(渠道 ID 跨实例必然错位);悬空引用只提示、不阻断。
		for _, id := range m.ChannelIds {
			name, ok := nameByID[id]
			if !ok {
				warnings = append(warnings, fmt.Sprintf(
					"监控「%s」勾选了一个已不存在的通知渠道,导出时已忽略", m.Name))
				continue
			}
			entry.ChannelNames = append(entry.ChannelNames, name)
		}
		file.Monitors = append(file.Monitors, entry)
	}

	// 面板设置导出**生效值**(与 GET /settings 同口径):保留期为空/越界时落默认值,
	// 状态格数走 NormalizeStatusStripRounds。这样导入端拿到的就是"源实例现在的样子"。
	days := st.ResultRetentionDays
	if days <= 0 {
		days = store.DefaultResultRetentionDays
	}
	rounds := store.NormalizeStatusStripRounds(st.StatusStripRounds)
	// 通知模板按**库内原始 map**导出(不做 Resolve):空 map 表示"三个事件都用内置默认",
	// 导入时才能忠实还原 —— 若导出解析后的默认值,恢复时会把本地的自定义模板冲掉。
	tmpls := make(configfile.NotifyTemplates, len(st.NotifyTemplates))
	for ev, tpl := range st.NotifyTemplates {
		tmpls[ev] = notifytmpl.Template{Title: tpl.Title, Content: tpl.Content}
	}
	file.Settings = &configfile.Settings{
		ResultRetentionDays: &days,
		StatusStripRounds:   &rounds,
		NotifyTemplates:     &tmpls,
	}
	return file, warnings, nil
}

// monitorEntry 把一条监控配置转成文件条目。
//
// 不导出的字段都是**实例内身份或运行时数据**:id/createdAt/updatedAt(身份与时间)、
// expectStatusCodes(由 specs 在导入时重新展开)、lastPushAt(运行时)、
// assignedAgentIds/excludedAgentIds(节点 ID 跨实例无意义,指派由导入表单指定)。
func monitorEntry(m *store.Monitor) configfile.Monitor {
	period, timeout, consecutive := m.Period, m.Timeout, m.Consecutive
	threshold, enabled := m.Threshold, m.Enabled
	allowInsecure, invert := m.AllowInsecureTLS, m.InvertMode
	entry := configfile.Monitor{
		Type: m.Type, Name: m.Name, Group: m.Group,
		Enabled: &enabled, Period: &period, Timeout: &timeout,
		Threshold: &threshold, Consecutive: &consecutive,
		URL: m.URL, Method: m.Method, Headers: m.Headers, Body: m.Body,
		AllowInsecureTLS: &allowInsecure, InvertMode: &invert,
		IPVersion:  m.IPVersion,
		TargetHost: m.TargetHost, Port: m.Port, SpeedUnit: m.SpeedUnit,
		PushToken: m.PushToken, AssignMode: m.AssignMode,
	}
	// 判定项只对 HTTP 类型有意义:下载速度监控判的是速度,校验层本就不接受
	// 期望状态码/关键词/JSON 断言(见 validateDownloadMonitor),PING/TCP 同理。
	// 导出它们只会让文件里多出一堆永远不生效的配置,导入时也只会被丢掉。
	if m.Type == checkconfig.TypeHTTP {
		// 期望状态码优先用编辑态 specs,存量文档只有展开码时按同样口径折算
		// (与 api.expectStatusSpecs 同源),保证文件里始终是 specs。
		entry.ExpectStatusSpecs = expectStatusSpecs(m)
		entry.ExpectContains = m.ExpectContains
		entry.ExpectNotContains = m.ExpectNotContains
		entry.JSONPath, entry.JSONPathOperator = m.JsonPath, m.JsonPathOperator
		entry.JSONAssertExpected = m.JsonAssertExpect
	}
	return entry
}

// configExportFilenameOf 组装导出文件名(带时间戳,便于同一天多次导出区分)。
func configExportFilenameOf(t time.Time) string {
	return "uptimemesh-config-" + t.Format("20060102-150405") + ".json"
}
