-- UptimeMesh Dashboard 存储层 DDL(SQLite)。
-- 约定:
--   * 主键统一为 24 位 hex 字符串(TEXT),与既有 REST/WS 契约保持一致。
--   * 时间统一存 UTC Unix 秒(INTEGER);hourly_stats.hour 例外,存
--     HourID() 生成的 `YYYYMMDDHH` 桶键,便于按小时直查。
--   * 字符串数组/结构统一存 JSON 文本(NULL 表示空)。
--   * group 是 SQL 保留字,monitors 的「分组」列名为 group_name。

CREATE TABLE IF NOT EXISTS users (
  id         TEXT PRIMARY KEY,
  username   TEXT NOT NULL,
  pass_hash  TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  id                    TEXT PRIMARY KEY,
  enrollment_key_hash   TEXT NOT NULL DEFAULT '',
  enrollment_key        TEXT NOT NULL DEFAULT '',
  jwt_secret            TEXT NOT NULL DEFAULT '',
  round_grace_seconds   INTEGER NOT NULL DEFAULT 0,
  result_retention_days INTEGER NOT NULL DEFAULT 0,
  -- 监控列表页「最近状态」一列的格数;0 = 未配置,读出时回落默认值(store.DefaultStatusStripRounds)。
  status_strip_rounds   INTEGER NOT NULL DEFAULT 0,
  notify_templates      TEXT,
  created_at            INTEGER NOT NULL DEFAULT 0,
  updated_at            INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS channels (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  url           TEXT NOT NULL,
  body_template TEXT,
  -- 启用状态:0 = 禁用,该渠道不接收自动告警/恢复通知(手动「测试发送」不受限)。
  -- 存量库由 db.go 的补列迁移补成 1(默认启用),不会把已配置好的渠道静默关掉。
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS agents (
  id              TEXT PRIMARY KEY,
  -- name 是展示名,后台可改(见 .scratch/agent-rename/spec.md)。
  name            TEXT NOT NULL,
  -- enroll_name 是节点自报的接入名,只用于识别身份(与 source_ip 组成唯一键),
  -- 后台改名不动它 —— 否则节点拿接入密钥回来时会认不出自己、又建一条 pending。
  enroll_name     TEXT NOT NULL DEFAULT '',
  version         TEXT NOT NULL DEFAULT '',
  os              TEXT NOT NULL DEFAULT '',
  arch            TEXT NOT NULL DEFAULT '',
  source_ip       TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'pending',
  credential_hash TEXT NOT NULL DEFAULT '',
  cred_plain      TEXT,
  capabilities    TEXT NOT NULL DEFAULT '',
  -- 节点自报的本机网络族可用性:1/0 = 可用/不可用,NULL = 未上报(老版本 Agent)。
  -- 用 NULL 而不是默认 0,否则旧节点会被显示成"不支持 IPv6"。
  ipv4_available  INTEGER,
  ipv6_available  INTEGER,
  country         TEXT NOT NULL DEFAULT '',
  region          TEXT NOT NULL DEFAULT '',
  region_ip_cache TEXT NOT NULL DEFAULT '',
  last_seen       INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_agents_status_created ON agents (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agents_credential_hash ON agents (credential_hash);
-- 身份唯一索引建在 db.go 的 postMigrationStatements(依赖 enroll_name 列,
-- 存量库要先补列并回填,故不能放在这里)。

CREATE TABLE IF NOT EXISTS monitors (
  id                   TEXT PRIMARY KEY,
  type                 TEXT NOT NULL DEFAULT 'http',
  name                 TEXT NOT NULL,
  group_name           TEXT NOT NULL DEFAULT '',
  enabled              INTEGER NOT NULL DEFAULT 0,
  period               INTEGER NOT NULL DEFAULT 60,
  timeout              INTEGER NOT NULL DEFAULT 0,
  threshold            REAL NOT NULL DEFAULT 0,
  consecutive          INTEGER NOT NULL DEFAULT 0,
  url                  TEXT NOT NULL DEFAULT '',
  method               TEXT NOT NULL DEFAULT '',
  headers              TEXT,
  body                 TEXT NOT NULL DEFAULT '',
  expect_status_specs  TEXT,
  expect_status_codes  TEXT,
  expect_contains      TEXT,
  expect_not_contains  TEXT,
  allow_insecure_tls   INTEGER NOT NULL DEFAULT 0,
  invert_mode          INTEGER NOT NULL DEFAULT 0,
  -- ip_version 是探测使用的 IP 协议族:'' / 'auto' = 系统默认,'ipv4' / 'ipv6' = 强制该族。
  -- 只作用于节点到目标那一段,与节点↔Dashboard 的长连接无关。
  ip_version           TEXT NOT NULL DEFAULT '',
  json_path            TEXT NOT NULL DEFAULT '',
  json_path_operator   TEXT NOT NULL DEFAULT '',
  json_assert_expect   TEXT NOT NULL DEFAULT '',
  target_host          TEXT NOT NULL DEFAULT '',
  port                 INTEGER NOT NULL DEFAULT 0,
  -- speed_unit 仅「下载速度监控」(type=download)有值:KB/s 或 MB/s,
  -- 决定 threshold 的解释方式(速度在比较与统计里统一换算为 KB/s)。
  speed_unit           TEXT NOT NULL DEFAULT '',
  push_token           TEXT NOT NULL DEFAULT '',
  last_push_at         INTEGER NOT NULL DEFAULT 0,
  assign_mode          TEXT NOT NULL DEFAULT '',
  assigned_agent_ids   TEXT,
  excluded_agent_ids   TEXT,
  channel_ids          TEXT,
  created_at           INTEGER NOT NULL DEFAULT 0,
  updated_at           INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_monitors_enabled ON monitors (enabled);
-- 上报令牌唯一索引建在 db.go 的 indexMigrations(依赖 push_token 列,
-- 存量库要先补列,故不能放在这里)。

CREATE TABLE IF NOT EXISTS rounds (
  id                 TEXT PRIMARY KEY,
  monitor_id         TEXT NOT NULL,
  assigned_agent_ids TEXT,
  scheduled_at       INTEGER NOT NULL,
  deadline           INTEGER NOT NULL DEFAULT 0,
  closed_at          INTEGER NOT NULL DEFAULT 0,
  state              TEXT NOT NULL,
  success            INTEGER NOT NULL DEFAULT 0,
  valid              INTEGER NOT NULL DEFAULT 0,
  total_agents       INTEGER NOT NULL DEFAULT 0,
  success_rate       REAL NOT NULL DEFAULT 0,
  latency_sum_ms     REAL NOT NULL DEFAULT 0,
  latency_count      INTEGER NOT NULL DEFAULT 0,
  -- 下载速度监控:本轮各有效样本速度之和与样本数(平均 = sum/count,KB/s);
  -- 其余类型恒为 0。与 latency_sum_ms/latency_count 同款中间量。
  speed_sum_kbps     REAL NOT NULL DEFAULT 0,
  speed_count        INTEGER NOT NULL DEFAULT 0,
  missing_alive      TEXT,
  missing_dead       TEXT,
  -- 任务没能交到节点手上的指派节点(建轮/重连补发时节点都不在线)⇒ 同"不计分母",
  -- 但成因在派发侧而不是节点测不出目标,故单独一列(见 .scratch/restart-first-round/spec.md)。
  missing_undispatched TEXT,
  created_at         INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_rounds_monitor_scheduled ON rounds (monitor_id, scheduled_at DESC);
CREATE INDEX IF NOT EXISTS idx_rounds_monitor_created ON rounds (monitor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_rounds_closed_at ON rounds (closed_at);

CREATE TABLE IF NOT EXISTS results (
  id           TEXT PRIMARY KEY,
  round_id     TEXT NOT NULL,
  monitor_id   TEXT NOT NULL,
  agent_id     TEXT NOT NULL,
  ok           INTEGER NOT NULL DEFAULT 0,
  latency_ms   REAL NOT NULL DEFAULT 0,
  http_status  INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',
  -- 下载速度监控:该节点本轮测得的速度(KB/s);其余类型恒为 0。
  speed_kbps   REAL NOT NULL DEFAULT 0,
  late         INTEGER NOT NULL DEFAULT 0,
  scheduled_at INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_results_round_agent ON results (round_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_results_monitor_created ON results (monitor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_results_monitor_scheduled ON results (monitor_id, scheduled_at);

CREATE TABLE IF NOT EXISTS hourly_stats (
  id             TEXT PRIMARY KEY,
  monitor_id     TEXT NOT NULL,
  hour           TEXT NOT NULL,
  rounds         INTEGER NOT NULL DEFAULT 0,
  valid          INTEGER NOT NULL DEFAULT 0,
  success        INTEGER NOT NULL DEFAULT 0,
  latency_sum_ms REAL NOT NULL DEFAULT 0,
  latency_count  INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_hourly_stats_monitor_hour ON hourly_stats (monitor_id, hour DESC);
-- 保留期清理按 hour 删除(见 store.PruneOldHourlyStats):id 是 monitor-hour 拼接,
-- 前缀为监控 ID,用不上主键的范围扫;hour 是定宽 YYYYMMDDHH,字典序即时间序,
-- 单列索引即可支撑「hour < ?」。
CREATE INDEX IF NOT EXISTS idx_hourly_stats_hour ON hourly_stats (hour);

CREATE TABLE IF NOT EXISTS monitor_states (
  id                TEXT PRIMARY KEY,
  alert_state       TEXT NOT NULL DEFAULT 'UP',
  consecutive       INTEGER NOT NULL DEFAULT 0,
  last_round_state  TEXT NOT NULL DEFAULT '',
  last_success_rate REAL NOT NULL DEFAULT 0,
  -- 最近一轮的平均下载速度(KB/s,仅下载速度监控;其余类型为 0)。
  last_speed_kbps   REAL NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL DEFAULT 0
);

-- 告警状态变动记录:只在 UP↔DOWN 真的翻转时写一条(连续破线期间不写)。
-- 只存"变动本身",展示所需的计划时间/成功率/节点明细按 round_id 关联回 rounds,
-- 故详情页两块面板的口径天然一致(见 .scratch/state-change-history/spec.md)。
CREATE TABLE IF NOT EXISTS monitor_state_changes (
  id           TEXT PRIMARY KEY,
  monitor_id   TEXT NOT NULL,
  round_id     TEXT NOT NULL,
  from_state   TEXT NOT NULL DEFAULT '',
  to_state     TEXT NOT NULL DEFAULT '',
  success_rate REAL NOT NULL DEFAULT 0,
  -- 触发翻转那一轮的平均下载速度(KB/s,仅下载速度监控;其余类型为 0)。
  speed_kbps   REAL NOT NULL DEFAULT 0,
  -- 本次报警的持续秒数:只有恢复(DOWN→UP)那条有值,= 上一条报错记录到它的间隔;
  -- 其余记录恒为 0(见 .scratch/alert-duration/spec.md)。
  duration_sec INTEGER NOT NULL DEFAULT 0,
  changed_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_state_changes_monitor_changed
  ON monitor_state_changes (monitor_id, changed_at DESC);
