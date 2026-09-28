package store

import (
	"context"
	"database/sql"
	"time"
)

// NotificationLog 是一次通知投递的留痕:每次向渠道(webhook)发送告警/恢复/测试
// 消息都落一行,成功失败都记(status 区分)。数据**永久保留**,不受「原始结果
// 保留(天)」清理的影响 —— retention.go 的清理函数不碰本表。
//
// MonitorID/ChannelID 只是事发当时的关联 ID:监控与渠道都可能后来被删除,
// 因此名称以快照形式冗余存进 MonitorName/ChannelName,列表不依赖回查即可展示。
type NotificationLog struct {
	ID          ID        `json:"id"`
	ChannelID   ID        `json:"channelId"`
	ChannelName string    `json:"channelName"`
	MonitorID   ID        `json:"monitorId"`
	MonitorName string    `json:"monitorName"`
	Event       string    `json:"event"` // DOWN | UP | TEST(与 alert/notifytmpl 的事件常量一致)
	Status      string    `json:"status"`
	// Attempts 实际尝试次数:成功时是打到 2xx 为止的次数,失败时是重试耗尽的总次数
	// (1 = 首次即成/败)。测试发送只试一次。
	Attempts int `json:"attempts"`
	// Title/Content 是按「设置 → 通知模板」渲染后的最终标题与正文(与 webhook 载荷一致);
	// 渠道配了自定义请求体模板时,Content 不代表实际发出的请求体(那是模板渲染的 JSON)。
	Title   string    `json:"title"`
	Content string    `json:"content"`
	// Error 最终失败原因;成功为空串。
	Error     string    `json:"error"`
	CreatedAt time.Time `json:"createdAt"`
}

// 通知发送状态取值(接口契约,前端据此上色)。
const (
	NotifyStatusSuccess = "success" // 投递成功(收到 2xx)
	NotifyStatusFailed  = "failed"  // 投递失败(重试耗尽或单次失败)
)

const notificationLogCols = `id, channel_id, channel_name, monitor_id, monitor_name,
	event, status, attempts, title, content, error, created_at`

func scanNotificationLog(row interface{ Scan(...any) error }) (*NotificationLog, error) {
	var (
		nl      NotificationLog
		errText sql.NullString
		created int64
	)
	if err := row.Scan(&nl.ID, &nl.ChannelID, &nl.ChannelName, &nl.MonitorID, &nl.MonitorName,
		&nl.Event, &nl.Status, &nl.Attempts, &nl.Title, &nl.Content, &errText, &created); err != nil {
		return nil, normalizeErr(err)
	}
	nl.Error = trimNull(errText)
	nl.CreatedAt = fromUnixSec(created)
	return &nl, nil
}

// InsertNotificationLog 追加一条通知投递记录。
func (s *Store) InsertNotificationLog(ctx context.Context, nl *NotificationLog) error {
	if nl.ID == "" {
		nl.ID = NewID()
	}
	if nl.CreatedAt.IsZero() {
		nl.CreatedAt = time.Now()
	}
	// error 列 NOT NULL DEFAULT '':空串直接存,不走 nullStr(显式 NULL 会撞约束)。
	_, err := s.db.ExecContext(ctx, `INSERT INTO notification_logs (`+notificationLogCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		nl.ID, nl.ChannelID, nl.ChannelName, nl.MonitorID, nl.MonitorName,
		nl.Event, nl.Status, nl.Attempts, nl.Title, nl.Content, nl.Error,
		unixSec(nl.CreatedAt))
	return normalizeErr(err)
}

// NotificationLogFilter 通知记录列表的筛选条件;零值字段表示不限。
type NotificationLogFilter struct {
	MonitorID ID   // 按监控筛(含该监控后来被删的历史记录:ID 不回查 monitors 表)
	ChannelID ID   // 按渠道筛(含已删除渠道的历史记录)
	Event     string // DOWN | UP | TEST;空 = 全部
	Status    string // success | failed;空 = 全部
}

// notificationLogWhere 由筛选条件拼 WHERE 子句(返回的子句总以 " WHERE" 开头或为空串,
// 供 COUNT 与 SELECT 共用,保证分页总数与列表口径一致)。
func notificationLogWhere(f NotificationLogFilter) (string, []any) {
	where := " WHERE 1=1"
	var args []any
	if f.MonitorID != "" {
		where += " AND monitor_id=?"
		args = append(args, f.MonitorID)
	}
	if f.ChannelID != "" {
		where += " AND channel_id=?"
		args = append(args, f.ChannelID)
	}
	if f.Event != "" {
		where += " AND event=?"
		args = append(args, f.Event)
	}
	if f.Status != "" {
		where += " AND status=?"
		args = append(args, f.Status)
	}
	return where, args
}

// ListNotificationLogs 分页读通知记录(新→旧)。同一秒内的多条用 rowid 兜底,
// 保证顺序就是写入顺序的倒序;total 是当前筛选下的总条数(分页用)。
func (s *Store) ListNotificationLogs(ctx context.Context, f NotificationLogFilter, limit, offset int) ([]*NotificationLog, int64, error) {
	where, args := notificationLogWhere(f)
	var total int64
	if err := s.dbRead.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, normalizeErr(err)
	}
	query := `SELECT ` + notificationLogCols + ` FROM notification_logs` + where + `
		ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`
	rows, err := s.dbRead.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, normalizeErr(err)
	}
	defer rows.Close()
	out := []*NotificationLog{}
	for rows.Next() {
		nl, err := scanNotificationLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, nl)
	}
	return out, total, normalizeErr(rows.Err())
}

// CountNotificationLogs 某渠道的通知记录条数(诊断用)。
func (s *Store) CountNotificationLogs(ctx context.Context, channelID ID) (int64, error) {
	var n int64
	err := s.dbRead.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_logs WHERE channel_id=?`, channelID).Scan(&n)
	return n, normalizeErr(err)
}
