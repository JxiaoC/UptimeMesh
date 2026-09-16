// Package configfile 定义 UptimeMesh 配置文件的磁盘格式与版本策略
// (见 .scratch/config-import-export/spec.md)。
//
// 本包是**纯逻辑**:只做编解码、版本闸门与迁移,不碰数据库、不认识 store。
// 导入导出的编排(判重、覆盖、渠道按名字匹配、设置落库)在 api 层。
//
// # 版本策略(本包存在的唯一理由)
//
// ConfigVersion 是唯一的版本闸门,且**只接受 ≤ CurrentVersion 的文件**:
//
//   - 旧版本文件 ⇒ 走高版本侧的迁移链(migrations)补齐,缺字段一律回落默认值;
//   - 更高版本文件 ⇒ 直接拒绝(TooNewError),绝不静默丢掉看不懂的字段。
//
// 因此有一条必须遵守的维护规则:**导出结构任何变化(新增/删除字段、改变字段语义)
// 都必须让 CurrentVersion +1,并补一条对应的 migrations 步骤**。漏掉这一步,
// 旧仪表盘就会接受新文件并悄悄丢配置 —— 那正是这条闸门要防的事。
package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/uptimemesh/dashboard/internal/notifytmpl"
)

// Kind 是配置文件的标识(写进文件,导入时校验),避免把别的 JSON 当配置文件。
const Kind = "uptimemesh.config"

// CurrentVersion 是当前仪表盘支持的最高配置版本。新增/修改字段必须 +1(见包注释)。
const CurrentVersion = 1

// ExportedAtLayout 是 exportedAt 的时间格式,与接口里其它时间字段一致。
const ExportedAtLayout = "2006-01-02 15:04:05"

// ErrNotConfigFile 文件不是 UptimeMesh 配置文件(kind 缺失或不符)。
var ErrNotConfigFile = errors.New("不是 UptimeMesh 配置文件")

// ErrBadVersion 文件缺少可用的 configVersion(缺失、0 或负数)。
var ErrBadVersion = errors.New("配置文件缺少有效的 configVersion")

// TooNewError 文件版本高于当前仪表盘支持的版本。
// 这是「不允许导入高版本配置文件」这条规则的具体载体:宁可拒绝,不可静默丢字段。
type TooNewError struct {
	File      int // 文件声明的版本
	Supported int // 当前仪表盘支持的版本(CurrentVersion)
}

func (e *TooNewError) Error() string {
	return fmt.Sprintf("配置文件版本过高:文件为 v%d,当前仪表盘仅支持 v%d", e.File, e.Supported)
}

// File 是配置文件的顶层结构。
//
// 只导出**可迁移的配置**:监控、通知渠道、通知模板与面板设置。
// 接入密钥、JWT 密钥、管理员账号、节点凭据、轮次与结果数据一律不在其中
// (前者是实例机密,后者不是「配置」,见 .scratch/config-import-export/spec.md)。
type File struct {
	Kind          string    `json:"kind"`
	ConfigVersion int       `json:"configVersion"`
	ExportedAt    string    `json:"exportedAt,omitempty"`
	Settings      *Settings `json:"settings,omitempty"`
	Channels      []Channel `json:"channels,omitempty"`
	Monitors      []Monitor `json:"monitors,omitempty"`
}

// Settings 是面板设置(导出的是**生效值**,与 GET /settings 同口径)。
//
// 三个字段都用指针,以区分「文件里没有这一项」与「文件里显式写了 0/空」:
// 前者不动本地设置,后者是用户的真实取值(非法时才跳过)。
type Settings struct {
	ResultRetentionDays *int `json:"resultRetentionDays,omitempty"`
	StatusStripRounds   *int `json:"statusStripRounds,omitempty"`
	// NotifyTemplates 为 nil 表示「文件里没有这一节」= 不动本地模板;
	// 指向空 map 表示「源实例三个事件全用内置默认」= 本地模板整体回落默认。
	NotifyTemplates *NotifyTemplates `json:"notifyTemplates,omitempty"`
}

// NotifyTemplates 是按事件类型(DOWN/UP/TEST)索引的通知模板。
// 用指针类型包一层,是为了让「缺这一节」与「这一节是空的」可区分(见 Settings)。
type NotifyTemplates map[string]notifytmpl.Template

// Channel 是一条通知渠道配置。ID 是实例内的主键,**不导出**:跨实例必然错位,
// 监控对渠道的引用在文件里按**名称**表达(见 Monitor.ChannelNames)。
type Channel struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	BodyTemplate string `json:"bodyTemplate,omitempty"`
	// Enabled 是渠道的启用状态;指针以便「文件里没写」与「显式写了 false」可区分:
	// 前者按启用处理(旧版本导出的文件没有这一项,不该把渠道导成禁用)。
	Enabled *bool `json:"enabled,omitempty"`
}

// Monitor 是一条监控配置。
//
// 与 store.Monitor 的差异都是刻意的:
//   - 不导出 id/createdAt/updatedAt(实例内身份与时间);
//   - 不导出 expectStatusCodes(它由 expectStatusSpecs 在导入时展开,保持单一事实源);
//   - 不导出 lastPushAt(运行时数据);
//   - 不导出 assignedAgentIds/excludedAgentIds(节点 ID 是实例内引用);
//   - 用 channelNames 取代 channelIds(同理,按名字跨实例对齐)。
//
// 数值字段用指针,让「文件里没写」与「显式写了 0」可区分:前者回落默认值
// (周期 60s / 超时 10s / 阈值 100 / 连续 3 轮 / 启用),后者按真实取值校验。
type Monitor struct {
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Group       string   `json:"group,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Period      *int     `json:"period,omitempty"`
	Timeout     *int     `json:"timeout,omitempty"`
	Threshold   *float64 `json:"threshold,omitempty"`
	Consecutive *int     `json:"consecutive,omitempty"`

	// HTTP / 下载速度监控的请求侧配置。
	URL               string            `json:"url,omitempty"`
	Method            string            `json:"method,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	Body              string            `json:"body,omitempty"`
	ExpectStatusSpecs []string          `json:"expectStatusSpecs,omitempty"`
	ExpectContains    []string          `json:"expectContains,omitempty"`
	ExpectNotContains []string          `json:"expectNotContains,omitempty"`
	AllowInsecureTLS  *bool             `json:"allowInsecureTLS,omitempty"`
	InvertMode        *bool             `json:"invertMode,omitempty"`
	IPVersion         string            `json:"ipVersion,omitempty"`
	// JSON 断言(JSONata 表达式取值后与期望值比较)。
	JSONPath           string `json:"jsonPath,omitempty"`
	JSONPathOperator   string `json:"jsonPathOperator,omitempty"`
	JSONAssertExpected string `json:"jsonAssertExpected,omitempty"`

	// PING / TCP 的目标主机与端口。
	TargetHost string `json:"targetHost,omitempty"`
	Port       int    `json:"port,omitempty"`
	// SpeedUnit 仅 download 类型有值(阈值单位)。
	SpeedUnit string `json:"speedUnit,omitempty"`
	// PushToken 是 push(外部上报)监控的上报令牌。同产品迁移时**沿用**它,
	// 已部署的上报脚本才不用改指向;与库内其它监控冲突或为空时导入侧会重新生成。
	PushToken string `json:"pushToken,omitempty"`

	// ChannelNames 是该监控勾选的通知渠道名(按名字对齐,见 Channel)。
	ChannelNames []string `json:"channelNames,omitempty"`
	// AssignMode 只作记录,导入时**不生效**:节点 ID 属于各实例,指派一律由导入表单
	// 统一指定(默认「所有节点」),与 UptimeKuma 导入口径一致。
	AssignMode string `json:"assignMode,omitempty"`
}

// Encode 序列化为配置文件字节(两空格缩进 + 结尾换行,便于人工查看与版本对比)。
func Encode(f *File) ([]byte, error) {
	if f == nil {
		return nil, errors.New("配置文件为空")
	}
	if f.Kind == "" {
		f.Kind = Kind
	}
	if f.Kind != Kind {
		return nil, ErrNotConfigFile
	}
	if f.ConfigVersion <= 0 {
		f.ConfigVersion = CurrentVersion
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// NewFile 组装一个待导出的文件骨架(版本与标识由本包负责,调用方只填内容)。
func NewFile(exportedAt time.Time) *File {
	return &File{
		Kind:          Kind,
		ConfigVersion: CurrentVersion,
		ExportedAt:    exportedAt.Format(ExportedAtLayout),
	}
}

// Decode 解析并校验配置文件:严格解码(未知字段直接报错)→ 版本闸门 → 迁移链。
//
// 严格解码是刻意的:版本闸门已经保证「文件版本 ≤ 当前」,此时出现未知字段只可能是
// 文件被手改或版本号被伪造。按「宁可拒绝,不可静默丢配置」的要求,这里直接报错,
// 而不是忽略看不懂的字段。
func Decode(raw []byte) (*File, error) {
	// 容忍 Windows 记事本等编辑器写入的 UTF-8 BOM。
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("配置文件内容为空")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	// 对象后面还跟着东西(两个 JSON 拼在一起)也当格式错误:再解一次应当直接 EOF。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("配置文件尾部有多余内容")
	}
	if f.Kind != Kind {
		return nil, ErrNotConfigFile
	}
	if f.ConfigVersion <= 0 {
		return nil, ErrBadVersion
	}
	if f.ConfigVersion > CurrentVersion {
		return nil, &TooNewError{File: f.ConfigVersion, Supported: CurrentVersion}
	}
	if err := Migrate(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// migrations 把 configVersion == key 的文件就地升级到 key+1。
//
// 首次发版(CurrentVersion = 1)没有任何历史版本,故这里是空的 —— 但机制必须在,
// 且被单测覆盖(用 migrateTo 直接跑到指定版本)。将来每加一条,就同时把
// CurrentVersion +1。缺失的步骤视为无操作(只升版本号)。
var migrations = map[int]func(*File) error{}

// Migrate 把文件逐级升到 CurrentVersion。
func Migrate(f *File) error { return migrateTo(f, CurrentVersion) }

// migrateTo 逐级执行迁移,直到文件版本达到 target。
// 抽出来是为了可测:单测注册假迁移后直接迁到 v3,验证「逐级执行」与「缺步即无操作」。
func migrateTo(f *File, target int) error {
	if f == nil {
		return errors.New("配置文件为空")
	}
	for v := f.ConfigVersion; v < target; v++ {
		if step, ok := migrations[v]; ok {
			if err := step(f); err != nil {
				return fmt.Errorf("配置迁移(v%d → v%d)失败: %w", v, v+1, err)
			}
		}
		// 无论这一步有没有迁移实现,版本号都要推进:否则缺失的步骤会让循环停住。
		f.ConfigVersion = v + 1
	}
	return nil
}
