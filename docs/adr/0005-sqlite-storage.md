# 数据存储改用 SQLite 单文件

Status: Accepted(取代 ADR-0004)

## 背景

原方案(ADR-0004、票 01)为 Dashboard 配了一个专用 MongoDB 容器(`mongo:7`、独立卷、密码认证、宿主端口 27018),存储层用官方 `mongo-driver` 手写访问代码。落地后发现实际用量与 MongoDB 的能力严重不匹配:

- **只用到文档 KV 与少量更新算子**:`$set`/`$unset`/`$inc`/`$setOnInsert` + upsert、复合索引、`$in`、4 条聚合管道、`$pull`,以及 `dbStats`/`$collStats` 诊断。从未使用多文档事务、副本集、change stream、嵌套文档查询、GridFS。
- **写者只有 Dashboard 一个进程**:ADR-0002 规定 Dashboard 永不执行检测,Agent 只经 WebSocket 回传结果,不直连数据库;spec 又把「集群化 Dashboard」列为 Out of Scope(单实例、重启中断调度的窗口期接受)。MongoDB 的分布式能力完全没有落点。
- **成本全在运维面**:常驻 1GB 内存限额的容器、端口映射、健康检查(冷启动 `mongosh` 约 2-4s 满核)、凭据管理、`depends_on: service_healthy` 启动依赖,以及**全部集成测试必须先起这个容器**才能跑。

## 决策

Dashboard 的持久化改为**单文件 SQLite**,驱动用 `modernc.org/sqlite`(纯 Go),仍不经 GoFrame gdb。

理由:

1. **部署塌缩为一个容器 + 一个卷**:去掉数据库容器、端口、健康检查与启动依赖。
2. **测试不再依赖外部数据库**:每个用例一个 `t.TempDir()` 下的临时库,CI 无需 docker service。
3. **数据量本就适合**:按 50 监控 × 10 节点估算,31 天原始结果约 2200 万行、数百 MB,SQLite 游刃有余。
4. **纯 Go 驱动是硬约束**:Dockerfile 与 Agent 构建都是 `CGO_ENABLED=0`(`golang:1.27-alpine`),不能用需要 CGO 的 `mattn/go-sqlite3`。

延续 ADR-0004 的精神:数据访问是直写直查 + 少量 SQL,不引入 ORM。

## 关键约定

- **单写连接 + 独立读连接池**:`sql.DB` 写池固定 `SetMaxOpenConns(1)`,所有写语句与事务走它。SQLite 同时只允许一个写事务,串行化后彻底消除 `SQLITE_BUSY`。读走独立的 `dbRead` 池(4 条连接):WAL 模式下读写不互斥,慢读(如占用统计的 dbstat 全库扫描,1.7GB 库实测 40s+)不再把业务读堵在唯一连接上 —— 线上踩过:占用重算期间 `GET /settings` 反复挂起 20s+,反代 10s 无响应掐成 502(2026-09-18)。**写连接必须保持 1,不要动**;新增查询时按读写归类选连接。
- **PRAGMA 走 DSN 参数**(保证连接池里每条连接都生效):`journal_mode(WAL)`、`busy_timeout(5000)`、`synchronous(NORMAL)`、`foreign_keys(1)`。
- **主键保持 24 位 hex 字符串**(`TEXT`):与既有 REST/WS 契约、前端、`shared` 协议一致,避免把改动面扩散到前后端接口。`store.ID` 取代 `primitive.ObjectID`,提供 `IDFromHex`/`Hex()`/`NewID()`。
- **时间统一 UTC Unix 秒**(`INTEGER`);`hourly_stats.hour` 例外,存 `HourID()` 的 `YYYYMMDDHH` 桶键,便于按小时直查与字符串比较。
- **字符串数组/结构化字段存 JSON 文本**(`assigned_agent_ids`、`channel_ids`、`notify_templates` 等),空值存 `NULL`。
- **表结构在 `internal/store/schema.sql`**(`go:embed`,全部 `IF NOT EXISTS`,可重复执行)。无历史数据与历史版本,暂不引入迁移框架;schema 首次演进时再加 `PRAGMA user_version` 顺序迁移。
- **保留期清理分批执行**(每批 5000 行):单条大事务会长时间持写锁,阻塞轮次与结果的写入路径。
- **口径修正**:`results` 新增 `scheduled_at` 列(所属轮次计划时间),窗口过滤与小时分桶统一以它为准。原实现用 `createdAt` 过滤、用 `rounds.scheduledAt` 分桶,两个时间口径不一致。

## 已知后果与退出条件

- 单实例 Dashboard 的存储不再具备水平扩展能力。**若将来要做多实例 Dashboard 或多租户部署,本决策必须重估**——届时回到 MongoDB,或换 PostgreSQL,而不是在 SQLite 上叠分布式逻辑。
- 原生文件锁依赖本地文件系统:部署时不要把库文件放在 NFS/SMB 等网络文件系统上。
- 设置页「数据库占用」的呈现能力弱于 MongoDB:`dbstat` 虚表不可用时逐表字节列回落为 0(库文件总占用仍准确),索引占用不做单独拆解。
- 迁移没有历史数据负担:项目尚无生产数据,切换即丢弃原 `uptimemesh-mongo-data` 卷(不提供 MongoDB → SQLite 的搬迁脚本)。
