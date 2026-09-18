// Package notifytmpl 通知消息模板:按事件类型提供默认标题/正文,并把 {{变量}}
// 占位符渲染成实际值。纯函数包,便于测试与在 API/Notifier 间共享。
package notifytmpl

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 通知事件类型,与 alert.Event / Payload.Event 取值一致。
const (
	EventDown = "DOWN" // 触发告警
	EventUp   = "UP"   // 恢复正常
	EventTest = "TEST" // 渠道测试发送
)

// Events 设置页展示顺序,也是允许配置的事件全集。
var Events = []string{EventDown, EventUp, EventTest}

// Template 单条消息模板。标题与正文都可含 {{变量}} 占位符。
type Template struct {
	Title   string `bson:"title" json:"title"`
	Content string `bson:"content" json:"content"`
}

// Vars 渲染占位符时的取值。全部为字符串,避免模板里出现 Go 的数值格式。
type Vars struct {
	Event       string
	MonitorID   string
	MonitorName string
	MonitorType string
	URL         string
	SuccessRate string
	// Speed 是下载速度监控的平均速度(形如 "12.34 MB/s"):其余类型为空串
	// (它们的判定值是成功率,Speed 无意义 —— 模板里会渲染成空)。
	Speed string
	// Agents 是触发事件那一轮的**节点明细**多行文本(每个被指派节点一行:
	// 正常/失败/未回传/离线 + 耗时或失败原因)。见 notifier.NodeStatus。
	Agents string
	// ErrorCount 是渲染这一刻仍处于报警状态的监控数(含当前这条):告警/恢复通知
	// 顺带说清"全局还挂着几个",值班的人不必先打开页面数一遍。取值的口径(只算
	// 启用中的监控)见 notifier.countDownAlerts;统计失败时为空串。
	ErrorCount string
	// Duration 是本次报警的持续时长(形如 "5 分 30 秒"):只有恢复(UP)事件有值,
	// 由调度器回查上一条 DOWN 记录算出(见 scheduler.alarmDurationSec)。
	// DOWN/TEST 事件与查不到起点时为空串 —— 模板里渲染成空,不会残留花括号。
	Duration string
	Timestamp string
}

// Placeholders 支持的占位符名(不含花括号),供文档与前端提示复用。
var Placeholders = []string{
	"monitorName", "monitorId", "monitorType", "url", "event", "successRate", "speed",
	"agents", "errorCount", "duration", "timestamp",
}

// BodyPlaceholders 自定义 webhook 请求体模板可用占位符:
// 事件变量之外额外暴露已渲染的 title/content,便于拼装各家机器人要求的 JSON 结构。
var BodyPlaceholders = append(append([]string{}, Placeholders...), "title", "content")

// SampleVars 预览/校验用示例变量(与前端展示一致)。
func SampleVars() Vars {
	return Vars{
		Event: EventDown, MonitorID: "6aa3eaea721b7851dac0d93e", MonitorName: "官网首页",
		MonitorType: "HTTP", URL: "https://example.com/health", SuccessRate: "42.5",
		Speed:  "12.34 MB/s",
		Agents: "- 华东-1: 失败(状态码 502)\n- 华北-2: 正常(18.4 ms)",
		// 报警数取一个"还有几个没恢复"的示例值(默认标题会把它顶在最前面)。
		ErrorCount: "3",
		// Duration 是恢复事件才有的变量,示例固定给 DOWN 口径,故留空:
		// 样例文案不参与本地化(后端文案恒为中文)。
		// 时间是示例值,故直接写字面量。
		Timestamp: "2026-09-12 10:00:00",
	}
}

// SampleMessage 用默认 DOWN 模板渲染出的示例标题/正文,供请求体模板校验与预览。
func SampleMessage() (title, content string) {
	r := Render(Defaults()[EventDown], SampleVars())
	return r.Title, r.Content
}

// Defaults 各事件的出厂模板(通用/成功率口径)。用户未配置或清空时回落到这里。
// DOWN/UP 都带「节点状态」一段({{agents}}):告警只写"成功率 42.5%"看不出是谁挂的,
// 值班的人第一步要问的正是"哪个节点出了问题"。
// 标题最前方的 {{errorCount}} 是"此刻还挂着几个报警"(DOWN 含本条,UP 是恢复后剩下的):
// 面板上同时挂着十几条时,先看这个数就知道是一条新故障还是整片崩了。
// UP 正文多一行「报警持续时长」({{duration}}):恢复通知最常被追问的就是"挂了多久";
// 查不到配对的 DOWN 记录时该变量渲染成空(那一行只剩标签,可接受)。
func Defaults() map[string]Template {
	return map[string]Template{
		EventDown: {
			Title: "【{{errorCount}}报警】{{monitorName}}",
			Content: "监控「{{monitorName}}」已触发告警(DOWN)。\n" +
				"类型:{{monitorType}}\n目标:{{url}}\n" +
				"本次成功率:{{successRate}}%\n" +
				"节点状态:\n{{agents}}\n时间:{{timestamp}}",
		},
		EventUp: {
			Title: "【{{errorCount}}报警】{{monitorName}}",
			Content: "监控「{{monitorName}}」已恢复正常(UP)。\n" +
				"类型:{{monitorType}}\n目标:{{url}}\n" +
				"本次成功率:{{successRate}}%\n" +
				"报警持续时长:{{duration}}\n" +
				"节点状态:\n{{agents}}\n时间:{{timestamp}}",
		},
		EventTest: {
			Title:   "【测试】UptimeMesh 通知",
			Content: "这是一条测试消息,用于验证通知渠道是否可用。\n时间:{{timestamp}}",
		},
	}
}

// NotifyMonitorTypeDownload 是下载速度监控的类型取值(与 checkconfig.TypeDownload 一致)。
// 本包不认识监控配置,只靠这个字符串挑默认文案,故就地重复一个常量而不是反向依赖。
const NotifyMonitorTypeDownload = "download"

// DefaultsFor 按监控类型给出厂模板:下载速度监控的判定值是速度,默认文案必须讲
// 「平均下载速度:{{speed}}」而不是「本次成功率:{{successRate}}%」(速度值后面跟一个 %
// 会读成"12 MB/s%");其余类型与 Defaults 完全相同。
// 只影响**默认**文案:用户自定义的事件模板永远优先(见 ResolveFor)。
func DefaultsFor(monitorType string) map[string]Template {
	out := Defaults()
	if monitorType != NotifyMonitorTypeDownload {
		return out
	}
	out[EventDown] = Template{
		Title: "【{{errorCount}}报警】{{monitorName}}",
		Content: "监控「{{monitorName}}」已触发告警(DOWN)。\n" +
			"类型:{{monitorType}}\n目标:{{url}}\n" +
			"平均下载速度:{{speed}}\n" +
			"节点状态:\n{{agents}}\n时间:{{timestamp}}",
	}
	out[EventUp] = Template{
		Title: "【{{errorCount}}报警】{{monitorName}}",
		Content: "监控「{{monitorName}}」已恢复正常(UP)。\n" +
			"类型:{{monitorType}}\n目标:{{url}}\n" +
			"平均下载速度:{{speed}}\n" +
			"报警持续时长:{{duration}}\n" +
			"节点状态:\n{{agents}}\n时间:{{timestamp}}",
	}
	return out
}

// Resolve 合并用户配置与默认值:未配置、或标题正文都为空白的事件回落到默认模板。
func Resolve(stored map[string]Template) map[string]Template {
	return resolve(stored, Defaults())
}

// ResolveFor 与 Resolve 同义,但默认值按监控类型挑(下载速度监控讲速度)。
func ResolveFor(stored map[string]Template, monitorType string) map[string]Template {
	return resolve(stored, DefaultsFor(monitorType))
}

// resolve 把用户模板盖在 defaults 上:两者皆空的事件视为"恢复默认",不覆盖。
func resolve(stored, defaults map[string]Template) map[string]Template {
	out := defaults
	for _, ev := range Events {
		t, ok := stored[ev]
		if !ok {
			continue
		}
		if strings.TrimSpace(t.Title) == "" && strings.TrimSpace(t.Content) == "" {
			continue
		}
		out[ev] = t
	}
	return out
}

// IsEvent 判断是否为受支持的事件类型。
func IsEvent(ev string) bool {
	for _, e := range Events {
		if e == ev {
			return true
		}
	}
	return false
}

// Render 渲染模板。未识别的占位符原样保留,便于用户发现拼写错误。
func Render(t Template, v Vars) Template {
	r := strings.NewReplacer(
		"{{event}}", v.Event,
		"{{monitorId}}", v.MonitorID,
		"{{monitorName}}", v.MonitorName,
		"{{monitorType}}", v.MonitorType,
		"{{url}}", v.URL,
		"{{successRate}}", v.SuccessRate,
		"{{speed}}", v.Speed,
		"{{agents}}", v.Agents,
		"{{errorCount}}", v.ErrorCount,
		"{{duration}}", v.Duration,
		"{{timestamp}}", v.Timestamp,
	)
	return Template{Title: r.Replace(t.Title), Content: r.Replace(t.Content)}
}

// RenderBody 渲染自定义 webhook 请求体。变量值按 JSON 字符串转义后替换,
// 因此无论放在 JSON 字符串内还是作为裸值(如数字)都能得到合法 JSON;
// title/content 中的换行会被转义为 \n,避免破坏请求体结构。
// strings.Replacer 单趟替换且不重扫替换结果,值里含 {{x}} 也不会被二次替换。
func RenderBody(body string, v Vars, title, content string) string {
	return strings.NewReplacer(
		"{{event}}", jsonEscape(v.Event),
		"{{monitorId}}", jsonEscape(v.MonitorID),
		"{{monitorName}}", jsonEscape(v.MonitorName),
		"{{monitorType}}", jsonEscape(v.MonitorType),
		"{{url}}", jsonEscape(v.URL),
		"{{successRate}}", jsonEscape(v.SuccessRate),
		"{{speed}}", jsonEscape(v.Speed),
		"{{agents}}", jsonEscape(v.Agents),
		"{{errorCount}}", jsonEscape(v.ErrorCount),
		"{{duration}}", jsonEscape(v.Duration),
		"{{timestamp}}", jsonEscape(v.Timestamp),
		"{{title}}", jsonEscape(title),
		"{{content}}", jsonEscape(content),
	).Replace(body)
}

// jsonEscape 将值转义为可嵌入 JSON 字符串的形式(不含首尾引号)。
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b[1 : len(b)-1])
}

// UnknownBodyPlaceholders 返回请求体模板里未被支持的占位符名(去重,保持出现顺序)。
func UnknownBodyPlaceholders(text string) []string {
	known := map[string]bool{}
	for _, p := range BodyPlaceholders {
		known[p] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if !known[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

var placeholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)
