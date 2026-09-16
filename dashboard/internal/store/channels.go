package store

import (
	"context"
	"database/sql"
	"time"
)

// Channel(通知渠道)全局 webhook 列表,监控勾选使用(票 07)。
// BodyTemplate 为可选的自定义请求体模板(含 {{变量}});为空则发送默认通用 JSON。
//
// Enabled 为 false 表示渠道已禁用:自动告警/恢复通知不再投递给它,但它仍留在
// 监控的勾选里(启用后立即恢复投递,不必重新勾选)。手动「测试发送」不受此限制。
type Channel struct {
	ID           ID        `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	BodyTemplate string    `json:"bodyTemplate,omitempty"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

const channelCols = `id, name, url, body_template, enabled, created_at, updated_at`

func scanChannel(row interface{ Scan(...any) error }) (*Channel, error) {
	var (
		c                  Channel
		bodyTemplate       sql.NullString
		createdAt, updated int64
	)
	if err := row.Scan(&c.ID, &c.Name, &c.URL, &bodyTemplate, &c.Enabled, &createdAt, &updated); err != nil {
		return nil, normalizeErr(err)
	}
	c.BodyTemplate = trimNull(bodyTemplate)
	c.CreatedAt, c.UpdatedAt = fromUnixSec(createdAt), fromUnixSec(updated)
	return &c, nil
}

func (s *Store) InsertChannel(ctx context.Context, c *Channel) error {
	now := time.Now()
	if c.ID == "" {
		c.ID = NewID()
	}
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (`+channelCols+`) VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.URL, nullStr(c.BodyTemplate), c.Enabled,
		unixSec(c.CreatedAt), unixSec(c.UpdatedAt))
	return normalizeErr(err)
}

func (s *Store) ListChannels(ctx context.Context) ([]*Channel, error) {
	return s.queryChannels(ctx, `SELECT `+channelCols+` FROM channels ORDER BY created_at DESC`)
}

// FindChannelByID 按主键取渠道;不存在返回 ErrNotFound。
func (s *Store) FindChannelByID(ctx context.Context, id ID) (*Channel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id=?`, id)
	return scanChannel(row)
}

// FindChannelsByIDs 按监控勾选的渠道 ID(hex)批量取回,忽略非法/已删除 ID。
func (s *Store) FindChannelsByIDs(ctx context.Context, hexIDs []string) []*Channel {
	ids := make([]any, 0, len(hexIDs))
	for _, h := range hexIDs {
		if _, err := IDFromHex(h); err == nil {
			ids = append(ids, h)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out, err := s.queryChannels(ctx,
		`SELECT `+channelCols+` FROM channels WHERE id IN (`+inPlaceholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil
	}
	return out
}

func (s *Store) queryChannels(ctx context.Context, query string, args ...any) ([]*Channel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, normalizeErr(rows.Err())
}

func (s *Store) UpdateChannel(ctx context.Context, c *Channel) error {
	c.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `UPDATE channels SET name=?, url=?, body_template=?,
		enabled=?, updated_at=? WHERE id=?`,
		c.Name, c.URL, nullStr(c.BodyTemplate), c.Enabled, unixSec(c.UpdatedAt), c.ID)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetChannelEnabled 单独切换渠道的启用状态(列表页的开关/弹窗保存都走这里)。
// 只改 enabled 与 updated_at:URL、模板等配置保持原样,也不动监控侧的勾选 ——
// 禁用是"暂时不投递",不是"解除勾选"(见 .scratch/channel-enabled/spec.md)。
func (s *Store) SetChannelEnabled(ctx context.Context, id ID, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET enabled=?, updated_at=? WHERE id=?`,
		enabled, unixSec(time.Now()), id)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChannel(ctx context.Context, id ID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteChannelDetach 删除渠道并同步从各监控的勾选里摘除(单事务),避免悬空引用。
func (s *Store) DeleteChannelDetach(ctx context.Context, id ID) error {
	if err := s.DeleteChannel(ctx, id); err != nil {
		return err
	}
	return s.DetachChannelFromMonitors(ctx, id)
}
