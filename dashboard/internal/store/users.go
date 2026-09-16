package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var ErrNoUser = errors.New("尚未初始化管理员账号")
var ErrAdminExists = errors.New("管理员账号已存在")

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	PassHash  string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

// InitAdmin 首次初始化:仅当 users 表为空时创建成功。
func (s *Store) InitAdmin(ctx context.Context, username, password string) error {
	n, err := s.countRows(ctx, TableUsers)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrAdminExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, pass_hash, created_at) VALUES (?,?,?,?)`,
		"admin", username, string(hash), unixSec(time.Now()))
	if errors.Is(err, ErrDuplicate) {
		return ErrAdminExists
	}
	return normalizeErr(err)
}

// HasAdmin 是否已初始化管理员。
func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	n, err := s.countRows(ctx, TableUsers)
	return n > 0, err
}

// adminRowID 单管理员模型:users 表里只有这一行。
const adminRowID = "admin"

// GetAdmin 读取管理员账号(不含密码明文);未初始化返回 ErrNoUser。
func (s *Store) GetAdmin(ctx context.Context) (*User, error) {
	var (
		u       User
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, pass_hash, created_at FROM users WHERE id=?`, adminRowID).
		Scan(&u.ID, &u.Username, &u.PassHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, normalizeErr(err)
	}
	u.CreatedAt = fromUnixSec(created)
	return &u, nil
}

// VerifyAdminPassword 只校验密码、不校验用户名(设置页改账号/改密码前先证明身份)。
func (s *Store) VerifyAdminPassword(ctx context.Context, password string) error {
	u, err := s.GetAdmin(ctx)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(password)) != nil {
		return errors.New("当前密码错误")
	}
	return nil
}

// UpdateAdminAccount 更新管理员用户名与密码;newPassword 为空表示只改用户名。
// 单管理员,故直接按固定主键更新。
func (s *Store) UpdateAdminAccount(ctx context.Context, username, newPassword string) error {
	res, err := s.updateAdminRow(ctx, username, newPassword)
	if err != nil {
		return normalizeErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 影响 0 行也可能是"值没变"(SQLite 仍会报 1 行),这里只在确实没有该行时报错。
		if _, gerr := s.GetAdmin(ctx); gerr != nil {
			return gerr
		}
	}
	return nil
}

// updateAdminRow 按 newPassword 是否为空选择 UPDATE 语句(不写空密码,避免把密码清掉)。
func (s *Store) updateAdminRow(ctx context.Context, username, newPassword string) (sql.Result, error) {
	if strings.TrimSpace(newPassword) == "" {
		return s.db.ExecContext(ctx,
			`UPDATE users SET username=? WHERE id=?`, username, adminRowID)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return s.db.ExecContext(ctx,
		`UPDATE users SET username=?, pass_hash=? WHERE id=?`,
		username, string(hash), adminRowID)
}

// VerifyAdmin 校验用户名密码;返回 bcrypt 重哈希所需的 user。
func (s *Store) VerifyAdmin(ctx context.Context, username, password string) error {
	var (
		u       User
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, pass_hash, created_at FROM users WHERE id=?`, "admin").
		Scan(&u.ID, &u.Username, &u.PassHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoUser
	}
	if err != nil {
		return normalizeErr(err)
	}
	if u.Username != username ||
		bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(password)) != nil {
		return errors.New("用户名或密码错误")
	}
	return nil
}

// countRows 统计某表行数(表名来自内部常量,不接受外部输入)。
func (s *Store) countRows(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n)
	return n, normalizeErr(err)
}

// CountAgents 统计节点记录数(测试与诊断用);name 非空时按名称过滤。
func (s *Store) CountAgents(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return s.countRows(ctx, TableAgents)
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE name=?`, name).Scan(&n)
	return n, normalizeErr(err)
}

// DeleteAllUsers 清空管理员账号(测试重建引导流用)。
func (s *Store) DeleteAllUsers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM `+TableUsers)
	return normalizeErr(err)
}
