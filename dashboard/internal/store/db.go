// Package store 封装 SQLite 数据访问(ADR-0005:纯 Go 驱动 modernc.org/sqlite,
// 不走 GoFrame gdb)。表结构与约定见 schema.sql 顶部注释。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // database/sql 驱动,注册名 "sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// ErrNotFound 记录不存在(替代 mongo.ErrNoDocuments),API 层据此返回 404。
var ErrNotFound = errors.New("记录不存在")

// ErrDuplicate 唯一约束冲突(agents 的 名称+来源IP、users 的单管理员等)。
var ErrDuplicate = errors.New("记录已存在")

// ErrClosed 存储已关闭。
var ErrClosed = errors.New("存储已关闭")

// Store 是进程内唯一的数据访问入口。
//
// 并发模型(ADR-0005):Dashboard 是唯一写者,SQLite 同时只允许一个写事务,
// 故连接池固定为 1 条连接 —— 写全串行,彻底避免 SQLITE_BUSY;读取共用该连接,
// 在本项目的量级下(单实例、日增约数十万行)延迟可忽略。将来读放大时再拆
// 「单写连接 + 多读连接池」。
type Store struct {
	db   *sql.DB
	path string

	mu     sync.Mutex
	closed bool

	// compactMu 串行化数据库压缩(VACUUM 会长时间独占唯一写连接,不允许排队两个)。
	// 与 mu(关闭保护)分开,避免压缩期间阻塞 Close 之外的路径。见 compact.go。
	compactMu sync.Mutex

	// statsMu/statsCache 数据库占用统计(DBStats)的短缓存:读它要全库扫描 dbstat
	// 并逐表 COUNT(*),与写路径共用唯一连接时还会排队,没必要每次点刷新都重算。
	// 见 stats.go 的说明;压缩结束由 invalidateDBStats 主动失效。
	// statsRefreshing 标记已有一个后台重算在跑(单飞行,防 goroutine 堆积)。
	// statsEpoch 在缓存作废时 +1:让压缩前开跑的后台重算写回时自我丢弃。
	statsMu         sync.Mutex
	statsCache      *dbStatsCache
	statsRefreshing bool
	statsEpoch      uint64
}

// New 打开(必要时创建)path 处的 SQLite 库并应用 schema。
// path 为 ":memory:" 时使用内存库(仅供测试)。
func New(ctx context.Context, path string) (*Store, error) {
	dsn, err := dsnOf(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 单写连接:见 Store 注释。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite 失败(%s): %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err = s.applySchema(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	if err = s.applyColumnMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("补列失败: %w", err)
	}
	return s, nil
}

// dsnOf 组装 DSN。PRAGMA 走 DSN 参数,保证连接池里每条连接都生效
// (journal_mode 本身持久化在库文件上,其余为连接级设置)。
func dsnOf(path string) (string, error) {
	if path == "" {
		return "", errors.New("SQLite 路径不能为空")
	}
	if path == ":memory:" {
		return "file::memory:?cache=shared&_pragma=foreign_keys(1)", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析 SQLite 路径失败: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("创建数据库目录失败(%s): %w", filepath.Dir(abs), err)
	}
	q := url.Values{}
	// WAL:读写不互相阻塞;busy_timeout:兜底等待锁;NORMAL:WAL 下的推荐同步级别。
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	return "file:" + filepath.ToSlash(abs) + "?" + q.Encode(), nil
}

// applySchema 应用 schema.sql(语句以分号分隔,全部语句幂等,可重复执行)。
// 必须先去掉整行注释再按分号切分:注释里出现的分号(如「INTEGER);hourly_stats」)
// 会把注释撕成两半,残片会被当成 SQL 执行。
func (s *Store) applySchema(ctx context.Context) error {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	for _, stmt := range strings.Split(stripComments(string(raw)), ";") {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		if _, err = s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行 DDL 失败(%s): %w", firstLine(stmt), err)
		}
	}
	return nil
}

// stripComments 去掉整行注释(-- 开头)与空行,保留 SQL 正文。
func stripComments(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// firstLine 取 SQL 首个非空行,用于报错定位。
func firstLine(stmt string) string {
	if idx := strings.IndexByte(stmt, '\n'); idx >= 0 {
		return stmt[:idx]
	}
	return stmt
}

// columnMigration 描述一个必须存在的列。schema.sql 的 CREATE TABLE IF NOT EXISTS
// 对已存在的表是空操作,新增列不会自动补上,故这里按「目标列结构」幂等补列:
// 缺哪列补哪列,已存在则跳过。新建库(clone 列结构与目标一致)整段无操作,
// 存量库只跑到真正缺的那几列。
type columnMigration struct {
	table  string
	column string
	// ddl 是 ALTER TABLE ADD COLUMN 的列定义(含类型与默认值),须与 schema.sql 保持一致。
	ddl string
}

// columnMigrations 是各表的目标列结构增量:agents 的能力协商(一键升级,
// 见 .scratch/agent-self-upgrade/spec.md);monitors 的反转模式
// (.scratch/invert-mode/spec.md)、JSON 断言(.scratch/json-assert/spec.md)
// 与 TCP/Push 监控类型(.scratch/tcp-push-monitors/spec.md);
// 下载速度监控的速度列(.scratch/download-speed-monitor/spec.md)。
var columnMigrations = []columnMigration{
	{table: TableAgents, column: "capabilities", ddl: "capabilities TEXT NOT NULL DEFAULT ''"},
	// 节点自报的接入名(身份键);name 变成纯展示名,后台可改。
	// 见 .scratch/agent-rename/spec.md。
	{table: TableAgents, column: "enroll_name", ddl: "enroll_name TEXT NOT NULL DEFAULT ''"},
	// 节点自报的本机网络族可用性(可空:NULL = 老版本 Agent 未上报)。
	{table: TableAgents, column: "ipv4_available", ddl: "ipv4_available INTEGER"},
	{table: TableAgents, column: "ipv6_available", ddl: "ipv6_available INTEGER"},
	{table: TableMonitors, column: "invert_mode", ddl: "invert_mode INTEGER NOT NULL DEFAULT 0"},
	{table: TableMonitors, column: "ip_version", ddl: "ip_version TEXT NOT NULL DEFAULT ''"},
	{table: TableMonitors, column: "json_path", ddl: "json_path TEXT NOT NULL DEFAULT ''"},
	{table: TableMonitors, column: "json_path_operator", ddl: "json_path_operator TEXT NOT NULL DEFAULT ''"},
	{table: TableMonitors, column: "json_assert_expect", ddl: "json_assert_expect TEXT NOT NULL DEFAULT ''"},
	{table: TableMonitors, column: "port", ddl: "port INTEGER NOT NULL DEFAULT 0"},
	{table: TableMonitors, column: "push_token", ddl: "push_token TEXT NOT NULL DEFAULT ''"},
	{table: TableMonitors, column: "last_push_at", ddl: "last_push_at INTEGER NOT NULL DEFAULT 0"},
	{table: TableMonitors, column: "speed_unit", ddl: "speed_unit TEXT NOT NULL DEFAULT ''"},
	{table: TableRounds, column: "speed_sum_kbps", ddl: "speed_sum_kbps REAL NOT NULL DEFAULT 0"},
	{table: TableRounds, column: "speed_count", ddl: "speed_count INTEGER NOT NULL DEFAULT 0"},
	// 任务从未交到节点手上的指派节点(重启后首轮的补发兜底,见
	// .scratch/restart-first-round/spec.md)。
	{table: TableRounds, column: "missing_undispatched", ddl: "missing_undispatched TEXT"},
	{table: TableResults, column: "speed_kbps", ddl: "speed_kbps REAL NOT NULL DEFAULT 0"},
	{table: TableMonitorStates, column: "last_speed_kbps", ddl: "last_speed_kbps REAL NOT NULL DEFAULT 0"},
	{table: TableStateChanges, column: "speed_kbps", ddl: "speed_kbps REAL NOT NULL DEFAULT 0"},
	// 本次报警的持续秒数(仅恢复那条有值,见 .scratch/alert-duration/spec.md)。
	// 存量库补出来是 0 = 无持续时长,前端按"不显示"处理 —— 历史翻转没有配对信息可回填。
	{table: TableStateChanges, column: "duration_sec", ddl: "duration_sec INTEGER NOT NULL DEFAULT 0"},
	// 监控列表页「最近状态」的格数(后台可配,见 store.settings.go):
	// 存量库补出来是 0 = 未配置,读出时回落默认值。
	{table: TableSettings, column: "status_strip_rounds", ddl: "status_strip_rounds INTEGER NOT NULL DEFAULT 0"},
	// 通知渠道的启用状态(见 .scratch/channel-enabled/spec.md):存量渠道补出来是 1 = 启用,
	// 语义是"升级不改变现有告警行为",要停用得由用户显式关掉。
	{table: TableChannels, column: "enabled", ddl: "enabled INTEGER NOT NULL DEFAULT 1"},
}

// postMigrationUpdates 是依赖"新增列"的幂等数据回填,必须在补列之后、
// postMigrationStatements 之前执行 —— 后者的唯一索引依赖这里的回填结果。
var postMigrationUpdates = []string{
	// 接入名从 name 回填(见 .scratch/agent-rename/spec.md):两个名字分开之前,
	// name 既是展示名也是身份键,故存量记录的接入名就是当时的 name。
	//
	// 这一步不能省:补出来的 enroll_name 默认是空串,而同一出口 IP 后面常挂着多个
	// 节点(内网多台机器共用一个 NAT 出口),空串会让它们在新唯一索引上直接撞车,
	// 迁移本身就把 Dashboard 卡在启动失败上。
	`UPDATE agents SET enroll_name = name WHERE enroll_name = ''`,
	// push(外部上报)监控的阈值恒定为 PushThreshold(见该常量):存量里填成 0 的那些
	// 实际是"永不告警"的暗门,填成别的数字的则只是白填 —— 一次升级统一扶正,而不是
	// 等用户编辑监控才发现。条件幂等:跑过一次之后就没有任何行再满足它。
	fmt.Sprintf(`UPDATE monitors SET threshold = %d WHERE type = 'push' AND threshold <> %d`,
		PushThreshold, PushThreshold),
}

// postMigrationStatements 是依赖"新增列"的幂等 DDL,必须在 applyColumnMigrations
// 之后执行:schema.sql 每次启动都会整体执行,存量库此时还没有这些列,
// 把这类语句写进 schema.sql 会直接报 no such column。
var postMigrationStatements = []string{
	// 上报令牌是外部系统调用 /api/push/{token} 的唯一凭据,必须唯一(空串不参与)。
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_monitors_push_token
		ON monitors (push_token) WHERE push_token <> ''`,
	// 节点身份从「展示名 + 来源 IP」换成「接入名 + 来源 IP」。先 DROP 再建:
	// 存量库里旧索引正占着这个名字,且建在 (name, source_ip) 上 —— 留着它会让
	// 后台改名直接撞唯一约束(改成已存在的名字就失败)。
	`DROP INDEX IF EXISTS uq_agents_identity`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_identity ON agents (enroll_name, source_ip)`,
}

// applyColumnMigrations 逐个补齐缺失列,回填依赖新列的数据,再执行依赖新列的幂等 DDL。
func (s *Store) applyColumnMigrations(ctx context.Context) error {
	for _, mig := range columnMigrations {
		if err := s.ensureColumn(ctx, s.db, mig); err != nil {
			return err
		}
	}
	for _, stmt := range postMigrationUpdates {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行数据回填失败: %w", err)
		}
	}
	for _, stmt := range postMigrationStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行补列后的 DDL 失败: %w", err)
		}
	}
	return nil
}

// execer 兼容 *sql.DB 与 *sql.Tx,便于单测直接注入。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ensureColumn 在某表缺 column 时执行 ADD COLUMN。
// 用 PRAGMA table_info 先探测而不是「执行后忽略 duplicate column 错误」:
// 后者会把真正的 DDL 错误(如表不存在)一起吞掉。
func (s *Store) ensureColumn(ctx context.Context, db execer, mig columnMigration) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+mig.table+`)`)
	if err != nil {
		return fmt.Errorf("读取 %s 列结构失败: %w", mig.table, err)
	}
	exists := false
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       any
			primaryKey int
		)
		if err = rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("读取 %s 列结构失败: %w", mig.table, err)
		}
		if name == mig.column {
			exists = true
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("读取 %s 列结构失败: %w", mig.table, err)
	}
	if exists {
		return nil
	}
	if _, err = db.ExecContext(ctx, `ALTER TABLE `+mig.table+` ADD COLUMN `+mig.ddl); err != nil {
		return fmt.Errorf("为 %s 补列 %s 失败: %w", mig.table, mig.column, err)
	}
	return nil
}

// Close 关闭底层连接。可重复调用。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// Exec 执行写语句(供包内与测试使用)。
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

// QueryRow 查询单行(供包内与测试使用)。
func (s *Store) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// ---- ID ----

// ID 是记录主键:24 位 hex 字符串(与既有的 ObjectID 文本形态一致,
// 故 REST/WS 契约与前端无需改动)。
type ID string

// NewID 生成新的随机主键。
func NewID() ID {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败时退回时间戳,保证仍能生成可用 ID。
		return ID(hex.EncodeToString([]byte(time.Now().UTC().Format("060102150405"))))
	}
	return ID(hex.EncodeToString(b))
}

// IDFromHex 解析主键字符串(仅校验长度与 hex 形态)。
func IDFromHex(hexStr string) (ID, error) {
	if len(hexStr) != 24 {
		return "", fmt.Errorf("非法 ID: %q", hexStr)
	}
	if _, err := hex.DecodeString(hexStr); err != nil {
		return "", fmt.Errorf("非法 ID: %q", hexStr)
	}
	return ID(strings.ToLower(hexStr)), nil
}

// Hex 返回 hex 文本形态(与既有 primitive.ObjectID.Hex() 调用点兼容)。
func (id ID) Hex() string { return string(id) }

func (id ID) String() string { return string(id) }

// ---- 时间/标量辅助 ----

// unixSec 时间转 UTC Unix 秒;零值转 0。
func unixSec(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

// fromUnixSec Unix 秒转 UTC 时间;0 转零值。
func fromUnixSec(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// nullStr 空串存 NULL(便于 $unset 语义的字段:cred_plain 等)。
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
