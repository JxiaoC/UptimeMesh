package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptimemesh/dashboard/internal/notifytmpl"
)

const settingsID = "global"

// DefaultResultRetentionDays 原始结果的默认保留天数。
// 取 31 天:详情页分节点延时曲线直接读原始结果,需要原始样本覆盖总览卡片的
// 30 天窗口。可用率本身走小时聚合,不依赖原始结果(见 HourlyStatsRetentionDays)。
const DefaultResultRetentionDays = 31

// 「最近状态格数」的默认值与取值范围:监控列表页「最近状态」一列画多少格色块。
//
// 上限与列表接口 ?rounds= 的上限同口径(见 MaxStatusStripRounds 的使用处):
// 一次请求最多取 200 格,再多只会把响应体与页面 DOM 撑大;
// 下限 10 是"还能看出趋势"的最小值,比这更少不如把列宽让给别的列。
// 默认 50 沿用改造前的固定格数(200+ 行时 100 格会明显拖慢渲染与传输)。
const (
	DefaultStatusStripRounds = 50
	MinStatusStripRounds     = 10
	MaxStatusStripRounds     = 200
)

// NormalizeStatusStripRounds 归一化格数:未配置(0,老库补列后的值)或越界一律回落默认值
// —— 0 格会让列表页状态条整列空白,不能直接把库里的值当结果用。
func NormalizeStatusStripRounds(rounds int) int {
	if rounds < MinStatusStripRounds || rounds > MaxStatusStripRounds {
		return DefaultStatusStripRounds
	}
	return rounds
}

// StatusStripRounds 读取「最近状态格数」设置;读不到(库异常)也回落默认值,
// 让列表页在任何情况下都能画出一条状态条。
func (s *Store) StatusStripRounds(ctx context.Context) int {
	st, err := s.GetSettings(ctx)
	if err != nil {
		return DefaultStatusStripRounds
	}
	return NormalizeStatusStripRounds(st.StatusStripRounds)
}

// SetStatusStripRounds 更新「最近状态格数」(范围校验在 API 层,见 api.setStatusStripRounds)。
func (s *Store) SetStatusStripRounds(ctx context.Context, rounds int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE settings SET status_strip_rounds=?, updated_at=? WHERE id=?`,
		rounds, unixSec(time.Now()), settingsID)
	return normalizeErr(err)
}

// Settings 是全局配置:接入密钥(票 01)、JWT 密钥(票 02)、
// 轮次收集宽限期(票 06)、数据保留期(票 09)、最近状态格数(监控列表页状态条)。
//
// EnrollmentKey 为接入密钥明文,供设置页展示与安装弹窗自动填入;
// 校验仍以 EnrollmentKeyHash 为准(哈希是唯一事实源,明文只是可读副本)。
// 早期版本仅存哈希,此时明文为空且不可逆,需轮换后才有明文。
type Settings struct {
	ID                  string `json:"id"`
	EnrollmentKeyHash   string `json:"-"`
	EnrollmentKey       string `json:"enrollmentKey,omitempty"`
	JWTSecret           string `json:"-"`
	RoundGraceSeconds   int    `json:"roundGraceSeconds"`
	ResultRetentionDays int    `json:"resultRetentionDays"`
	// StatusStripRounds 是监控列表页「最近状态」的格数;0 = 未配置(用 DefaultStatusStripRounds),
	// 读出时统一走 StatusStripRounds()/NormalizeStatusStripRounds 归一化。
	StatusStripRounds int                            `json:"statusStripRounds"`
	NotifyTemplates   map[string]notifytmpl.Template `json:"notifyTemplates,omitempty"`
	CreatedAt         time.Time                      `json:"createdAt"`
	UpdatedAt         time.Time                      `json:"updatedAt"`
}

func HashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// NewEnrollmentKey 生成高熵随机接入密钥(明文与哈希同时落库)。
func NewEnrollmentKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "umk_" + hex.EncodeToString(b), nil
}

const settingsCols = `id, enrollment_key_hash, enrollment_key, jwt_secret,
	round_grace_seconds, result_retention_days, status_strip_rounds, notify_templates,
	created_at, updated_at`

func scanSettings(row interface{ Scan(...any) error }) (*Settings, error) {
	var (
		st        Settings
		templates sql.NullString
		createdAt int64
		updatedAt int64
	)
	err := row.Scan(&st.ID, &st.EnrollmentKeyHash, &st.EnrollmentKey, &st.JWTSecret,
		&st.RoundGraceSeconds, &st.ResultRetentionDays, &st.StatusStripRounds, &templates,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, normalizeErr(err)
	}
	if templates.Valid && templates.String != "" {
		_ = json.Unmarshal([]byte(templates.String), &st.NotifyTemplates)
	}
	st.CreatedAt = fromUnixSec(createdAt)
	st.UpdatedAt = fromUnixSec(updatedAt)
	return &st, nil
}

// EnsureSettings 首次启动时建全局配置:若不存在则生成初始接入密钥(返回明文)
// 并写入各项默认值(默认保留期、默认最近状态格数)。
func (s *Store) EnsureSettings(ctx context.Context) (createdKey string, err error) {
	_, err = s.GetSettings(ctx)
	if err == nil {
		return "", nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	key, err := NewEnrollmentKey()
	if err != nil {
		return "", err
	}
	jwtSecret := make([]byte, 32)
	if _, err = rand.Read(jwtSecret); err != nil {
		return "", err
	}
	now := unixSec(time.Now())
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (`+settingsCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		settingsID, HashSecret(key), key, hex.EncodeToString(jwtSecret),
		5, DefaultResultRetentionDays, DefaultStatusStripRounds, nil, now, now)
	if errors.Is(err, ErrDuplicate) {
		return "", nil // 并发下另一实例已创建
	}
	if err != nil {
		return "", normalizeErr(err)
	}
	return key, nil
}

// GetSettings 读取全局配置;不存在返回 ErrNotFound。
func (s *Store) GetSettings(ctx context.Context) (*Settings, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+settingsCols+` FROM settings WHERE id=?`, settingsID)
	return scanSettings(row)
}

// EnsureJWTSecret 返回全局 JWT 密钥,缺失时生成并持久化(兼容早期版本的 settings 行)。
func (s *Store) EnsureJWTSecret(ctx context.Context) (string, error) {
	st, err := s.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if st.JWTSecret != "" {
		return st.JWTSecret, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	_, err = s.db.ExecContext(ctx, `UPDATE settings SET jwt_secret=?, updated_at=? WHERE id=?`,
		secret, unixSec(time.Now()), settingsID)
	return secret, normalizeErr(err)
}

// CheckEnrollmentKey 校验接入密钥。
func (s *Store) CheckEnrollmentKey(ctx context.Context, key string) (bool, error) {
	st, err := s.GetSettings(ctx)
	if err != nil {
		return false, err
	}
	return st.EnrollmentKeyHash == HashSecret(key), nil
}

// RotateEnrollmentKey 生成并落库新接入密钥,返回明文(旧密钥立即失效;
// 已批准节点的凭据重连不受影响,不受新密钥约束)。
func (s *Store) RotateEnrollmentKey(ctx context.Context) (string, error) {
	key, err := NewEnrollmentKey()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE settings SET enrollment_key_hash=?,
		enrollment_key=?, updated_at=? WHERE id=?`,
		HashSecret(key), key, unixSec(time.Now()), settingsID)
	return key, normalizeErr(err)
}

// SetEnrollmentKeyForTest 直接写入接入密钥明文与哈希(仅供测试隔离使用)。
func (s *Store) SetEnrollmentKeyForTest(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE settings SET enrollment_key_hash=?,
		enrollment_key=?, updated_at=? WHERE id=?`,
		HashSecret(key), key, unixSec(time.Now()), settingsID)
	return normalizeErr(err)
}

// SetResultRetentionDays 更新原始结果保留天数(1~365)。
func (s *Store) SetResultRetentionDays(ctx context.Context, days int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE settings SET result_retention_days=?, updated_at=? WHERE id=?`,
		days, unixSec(time.Now()), settingsID)
	return normalizeErr(err)
}

// SetRoundGraceSeconds 更新轮次收集宽限期(测试与运维用)。
func (s *Store) SetRoundGraceSeconds(ctx context.Context, seconds int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE settings SET round_grace_seconds=?, updated_at=? WHERE id=?`,
		seconds, unixSec(time.Now()), settingsID)
	return normalizeErr(err)
}

// SetNotifyTemplates 整体替换通知模板配置(空 map 即全部回落默认)。
func (s *Store) SetNotifyTemplates(ctx context.Context, tmpls map[string]notifytmpl.Template) error {
	if tmpls == nil {
		tmpls = map[string]notifytmpl.Template{}
	}
	raw, err := json.Marshal(tmpls)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE settings SET notify_templates=?,
		updated_at=? WHERE id=?`, string(raw), unixSec(time.Now()), settingsID)
	return normalizeErr(err)
}
