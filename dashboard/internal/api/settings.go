package api

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/notifytmpl"
	"github.com/uptimemesh/dashboard/internal/store"
)

// registerSettingsRoutes 设置(票 11):密钥轮换、数据保留期、最近状态格数与通知模板。
func (a *API) registerSettingsRoutes(group *ghttp.RouterGroup) {
	group.GET("/settings", a.getSettings)
	group.GET("/settings/db-stats", a.getDBStats)
	group.POST("/settings/db-compact", a.compactDB)
	group.POST("/settings/rotate-key", a.rotateEnrollmentKey)
	group.PUT("/settings/retention", a.setRetention)
	// 监控列表页「最近状态」一列的格数(后台可配,默认 50)。
	group.PUT("/settings/status-strip", a.setStatusStripRounds)
	group.PUT("/settings/notify-templates", a.setNotifyTemplates)
	// 安全(设置页「安全」标签):查看与修改后台登录账号密码。
	group.GET("/settings/account", a.getAccount)
	group.PUT("/settings/account", a.updateAccount)
	// UptimeKuma 导入(设置页「导入 UptimeKuma」标签页)。
	a.registerKumaImportRoutes(group)
	// 配置文件导入导出(设置页「配置导入导出」标签页):导出本实例的可迁移配置,
	// 或把这样一份 JSON 导进来。格式与版本策略见 internal/configfile。
	group.GET("/settings/export/config", a.exportConfig)
	group.POST("/settings/import/config/preview", a.configImportPreview)
	group.POST("/settings/import/config", a.configImport)
}

func (a *API) getSettings(r *ghttp.Request) {
	st, err := a.Store.GetSettings(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "读取失败"})
		return
	}
	// 早期创建的 settings 文档可能缺字段,读出时兜底默认值。
	grace := st.RoundGraceSeconds
	if grace <= 0 {
		grace = 5
	}
	days := st.ResultRetentionDays
	if days <= 0 {
		days = store.DefaultResultRetentionDays
	}
	// enrollmentKey 为明文,供设置页展示与安装弹窗自动填入;仅 JWT 后可读。
	// 早期版本仅有哈希时为空,前端提示轮换(哈希不可逆,无法还原明文)。
	// notifyTemplates 已合并默认值,保证三个事件都返回可编辑的完整模板。
	// statusStripRounds 一并给出取值范围:前端输入框的上下限与校验文案都据此生成,
	// 免得服务端改了区间而页面还停在旧数字上。
	// hourlyStatsRetentionDays 同理:小时聚合保留期是固定策略(不可配),页面只做
	// 只读说明,数字由后端给出,免得改常量后说明文案与实际清理口径对不上。
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"resultRetentionDays":      days,
		"hourlyStatsRetentionDays": store.HourlyStatsRetentionDays,
		"roundGraceSeconds":        grace,
		"statusStripRounds":        store.NormalizeStatusStripRounds(st.StatusStripRounds),
		"statusStripMin":           store.MinStatusStripRounds,
		"statusStripMax":           store.MaxStatusStripRounds,
		"statusStripDefault":       store.DefaultStatusStripRounds,
		"enrollmentKey":            st.EnrollmentKey,
		"notifyTemplates":          notifytmpl.Resolve(st.NotifyTemplates),
		"notifyPlaceholders":       notifytmpl.Placeholders,
	}})
}

// ---- 安全:后台登录账号与密码 ----

// getAccount 返回当前管理员用户名(供「安全」标签回显;不含任何密码信息)。
func (a *API) getAccount(r *ghttp.Request) {
	u, err := a.Store.GetAdmin(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 404, "message": "管理员账号尚未初始化,请重新登录"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"username": u.Username}})
}

// accountReq 修改账号的入参。newPassword 留空 = 只改用户名。
type accountReq struct {
	Username        string `json:"username"`
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// updateAccount 修改后台登录账号与密码(PUT /settings/account)。
//
// 为什么必须验证当前密码:这条接口只凭 JWT 就能调用,一旦令牌泄露(或浏览器上有人
// 摸到已登录的会话),不验旧密码就等于把"改密码"变成一键接管账号。改密码属于凭证
// 变更,必须再证一次身份。
//
// 修改成功后重新签发 token:JWT 的 subject 是用户名,重签让前端持有的令牌与库内账号
// 保持一致。密码变更**不会**让其它已签发的令牌失效(JWT 无状态,没有令牌版本号)——
// 这一点在页面上写明,需要立即踢掉其它会话时由运维换 JWT 密钥解决。
func (a *API) updateAccount(r *ghttp.Request) {
	var req accountReq
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	username := strings.TrimSpace(req.Username)
	if msg := validateAdminUsername(username); msg != "" {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": msg})
		return
	}
	if req.CurrentPassword == "" {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "请输入当前密码以确认身份"})
		return
	}
	// 新密码留空 = 只改用户名(与表单一致);填了就必须过长度校验。
	if req.NewPassword != "" {
		if msg := validateAdminPassword(req.NewPassword); msg != "" {
			r.Response.WriteJsonExit(g.Map{"code": 400, "message": msg})
			return
		}
	}
	if err := a.Store.VerifyAdminPassword(r.Context(), req.CurrentPassword); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "当前密码不正确"})
		return
	}
	if err := a.Store.UpdateAdminAccount(r.Context(), username, req.NewPassword); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "保存失败"})
		return
	}
	token, err := a.JWT.Issue(username)
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "账号已保存,但重新签发登录令牌失败,请重新登录"})
		return
	}
	g.Log().Infof(r.Context(), "管理员账号已更新:用户名=%s,密码%s", username,
		map[bool]string{true: "已修改", false: "未改动"}[req.NewPassword != ""])
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已保存", "data": g.Map{
		"username": username, "token": token,
	}})
}

// adminUsernameMax 等长度上限:够长以容纳邮箱式用户名,又不至于把页面挤坏。
const (
	adminUsernameMin = 2
	adminUsernameMax = 32
	// bcrypt 只取前 72 字节,更长的密码会被静默截断,故显式拒绝。
	adminPasswordMin = 6
	adminPasswordMax = 72
)

// validateAdminUsername 校验用户名;返回空串表示通过(文案与登录页口径一致)。
func validateAdminUsername(name string) string {
	if name == "" {
		return "用户名必填"
	}
	n := utf8.RuneCountInString(name)
	if n < adminUsernameMin || n > adminUsernameMax {
		return "用户名长度须在 2~32 个字符之间"
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "用户名不能包含空格"
	}
	return ""
}

// validateAdminPassword 校验新密码长度(bcrypt 上限 72 字节,超长会被截断,故拒绝)。
func validateAdminPassword(pw string) string {
	n := len([]byte(pw))
	if n < adminPasswordMin || n > adminPasswordMax {
		return "密码长度须在 6~72 个字符之间"
	}
	return ""
}

// ---- 安全:后台登录账号与密码(完) ----

// getDBStats 数据库占用大小(诊断展示),含各集合明细。
func (a *API) getDBStats(r *ghttp.Request) {
	st, err := a.Store.DBStats(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "读取数据库占用失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": st})
}

// compactDB 压缩数据库(POST /settings/db-compact)。
//
// 同步执行、不等后台:压缩是管理员主动发起的一次性维护,请求期间界面按钮处于
// loading,结束后立即拿到「压缩前/后」的大小;代价是响应可能耗时数十秒(前端为此
// 单独放宽了超时),期间所有写入排队(见 store.Compact 的说明)。
//
// 只允许一个管理员触发一次:并发请求直接以 409 拒绝,不让第二个请求白白排队。
func (a *API) compactDB(r *ghttp.Request) {
	res, err := a.Store.Compact(r.Context())
	if errors.Is(err, store.ErrCompactBusy) {
		r.Response.WriteJsonExit(g.Map{"code": 409, "message": "数据库压缩正在进行中,请稍候再试"})
		return
	}
	if err != nil {
		g.Log().Errorf(r.Context(), "数据库压缩失败: %v", err)
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "数据库压缩失败"})
		return
	}
	g.Log().Infof(r.Context(), "管理员 %s 压缩数据库:%d → %d 字节(释放 %d,空闲页 %d,WAL 截断 %d,用时 %dms)",
		r.GetCtxVar(ctxUserKey).String(), res.BeforeBytes, res.AfterBytes, res.SavedBytes,
		res.FreePages, res.WalBytes, res.DurationMs)
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已压缩", "data": res})
}

// rotateEnrollmentKey 轮换全局接入密钥;明文仅此响应返回。
func (a *API) rotateEnrollmentKey(r *ghttp.Request) {
	key, err := a.Store.RotateEnrollmentKey(r.Context())
	if err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "轮换失败"})
		return
	}
	g.Log().Infof(r.Context(), "管理员 %s 已轮换接入密钥", r.GetCtxVar(ctxUserKey).String())
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{"enrollmentKey": key}})
}

func (a *API) setRetention(r *ghttp.Request) {
	var req struct {
		Days int `json:"resultRetentionDays"`
	}
	if err := r.Parse(&req); err != nil || req.Days < 1 || req.Days > 365 {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "保留天数须在 1~365 之间"})
		return
	}
	if err := a.Store.SetResultRetentionDays(r.Context(), req.Days); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "更新失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已保存"})
}

// setStatusStripRounds 保存监控列表页「最近状态」一列的格数(PUT /settings/status-strip)。
//
// 这个数直接决定列表接口与实时推送取多少格(见 api.stripRoundsFor),所以必须卡在
// store 的区间内:越界的值要么让状态条整列空白,要么把响应体与页面 DOM 撑大。
// 保存后立即生效 —— 前端监控列表页每次激活都会重读它(见 MonitorsView.refreshStripRounds)。
func (a *API) setStatusStripRounds(r *ghttp.Request) {
	var req struct {
		StatusStripRounds int `json:"statusStripRounds"`
	}
	if err := r.Parse(&req); err != nil ||
		req.StatusStripRounds < store.MinStatusStripRounds ||
		req.StatusStripRounds > store.MaxStatusStripRounds {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "最近状态格数须在 " +
			strconv.Itoa(store.MinStatusStripRounds) + "~" + strconv.Itoa(store.MaxStatusStripRounds) + " 之间"})
		return
	}
	if err := a.Store.SetStatusStripRounds(r.Context(), req.StatusStripRounds); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "更新失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "message": "已保存", "data": g.Map{
		"statusStripRounds": req.StatusStripRounds,
	}})
}

// templateMaxLen 单条模板字段的长度上限,避免误粘贴超大文本入库。
const templateMaxLen = 4000

// setNotifyTemplates 保存各事件的通知模板。只接受已知事件类型;
// 标题与正文同时为空白视为"恢复默认",该事件不落库。
func (a *API) setNotifyTemplates(r *ghttp.Request) {
	var req struct {
		Templates map[string]notifytmpl.Template `json:"templates"`
	}
	if err := r.Parse(&req); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 400, "message": "参数不合法"})
		return
	}
	out := map[string]notifytmpl.Template{}
	for ev, t := range req.Templates {
		if !notifytmpl.IsEvent(ev) {
			r.Response.WriteJsonExit(g.Map{"code": 400, "message": "未知事件类型: " + ev})
			return
		}
		t.Title = strings.TrimSpace(t.Title)
		t.Content = strings.TrimSpace(t.Content)
		if len(t.Title) > templateMaxLen || len(t.Content) > templateMaxLen {
			r.Response.WriteJsonExit(g.Map{"code": 400, "message": "模板长度超过 " + strconv.Itoa(templateMaxLen) + " 字符"})
			return
		}
		// 两者皆空 = 恢复默认,不写入(读取时按默认模板回落)。
		if t.Title == "" && t.Content == "" {
			continue
		}
		out[ev] = t
	}
	if err := a.Store.SetNotifyTemplates(r.Context(), out); err != nil {
		r.Response.WriteJsonExit(g.Map{"code": 500, "message": "更新失败"})
		return
	}
	r.Response.WriteJsonExit(g.Map{"code": 0, "data": g.Map{
		"notifyTemplates": notifytmpl.Resolve(out),
	}})
}
