package store

import (
	"context"
	"fmt"
	"time"
)

// HourlyStat 小时级预聚合(票 09):总览卡片 24h/7d/30d 可用率的数据源。
// ID = monitorHex-YYYYMMDDHH(UTC),天然幂等键;Hour 存同一桶键。
// 保留期固定为 HourlyStatsRetentionDays 天(见该常量的说明),不是长期保留。
type HourlyStat struct {
	ID           string    `json:"id"`
	MonitorID    ID        `json:"monitorId"`
	HourBucket   string    `json:"hourBucket"` // YYYYMMDDHH(UTC)
	Rounds       int       `json:"rounds"`
	Valid        int       `json:"valid"`
	Success      int       `json:"success"`
	LatencySumMs float64   `json:"latencySumMs"`
	LatencyCount int       `json:"latencyCount"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// HourID 小时桶键:monitorHex-YYYYMMDDHH(UTC)。
func HourID(monitorID ID, t time.Time) string {
	return fmt.Sprintf("%s-%s", monitorID.Hex(), HourBucket(t))
}

// HourBucket 把时间截断到 UTC 小时的桶键。
func HourBucket(t time.Time) string {
	return t.UTC().Truncate(time.Hour).Format("2006010215")
}

// ParseHourBucket 桶键还原为 UTC 时间。
func ParseHourBucket(bucket string) (time.Time, error) {
	return time.ParseInLocation("2006010215", bucket, time.UTC)
}

// AddHourlyStat 轮次定稿时累加(原子 upsert,幂等安全)。
func (s *Store) AddHourlyStat(ctx context.Context, monitorID ID, hour time.Time,
	roundsDelta, validDelta, successDelta int, latencySum float64, latencyCount int) error {
	bucket := HourBucket(hour)
	id := HourID(monitorID, hour)
	_, err := s.db.ExecContext(ctx, `INSERT INTO hourly_stats
		(id, monitor_id, hour, rounds, valid, success, latency_sum_ms, latency_count, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET rounds = rounds + excluded.rounds,
			valid = valid + excluded.valid, success = success + excluded.success,
			latency_sum_ms = latency_sum_ms + excluded.latency_sum_ms,
			latency_count = latency_count + excluded.latency_count,
			updated_at = excluded.updated_at`,
		id, monitorID.Hex(), bucket, roundsDelta, validDelta, successDelta,
		latencySum, latencyCount, unixSec(time.Now()))
	return normalizeErr(err)
}

// GetHourlyStatsSince 某监控 since 之后的小时桶,按桶升序。
func (s *Store) GetHourlyStatsSince(ctx context.Context, monitorID ID, since time.Time) ([]*HourlyStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, monitor_id, hour, rounds, valid,
		success, latency_sum_ms, latency_count, updated_at FROM hourly_stats
		WHERE monitor_id=? AND hour>=? ORDER BY hour ASC`,
		monitorID.Hex(), HourBucket(since))
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*HourlyStat{}
	for rows.Next() {
		var (
			st        HourlyStat
			updatedAt int64
		)
		if err = rows.Scan(&st.ID, &st.MonitorID, &st.HourBucket, &st.Rounds, &st.Valid,
			&st.Success, &st.LatencySumMs, &st.LatencyCount, &updatedAt); err != nil {
			return nil, normalizeErr(err)
		}
		st.UpdatedAt = fromUnixSec(updatedAt)
		out = append(out, &st)
	}
	return out, normalizeErr(rows.Err())
}

// StatsBucket 详情页趋势图的通用粒度桶。
// 数据源是 rounds 的定稿聚合(rounds 不随原始结果清理,长期保留),口径与
// hourly_stats 一致:可用率 = sum(success)/sum(valid);UNKNOWN 轮次 valid=0,
// 天然不进入分母。
type StatsBucket struct {
	BucketAt     time.Time `json:"bucketAt"`
	Rounds       int       `json:"rounds"`
	Valid        int       `json:"valid"`
	Success      int       `json:"success"`
	LatencySumMs float64   `json:"latencySumMs"`
	LatencyCount int       `json:"latencyCount"`
	// 下载速度监控的桶内速度中间量(其余类型恒为 0):平均速度 = Sum/Count。
	SpeedSumKbps float64 `json:"speedSumKbps,omitempty"`
	SpeedCount   int     `json:"speedCount,omitempty"`
}

// AvgSpeedKbps 桶内平均下载速度(KB/s);没有速度样本时为 0。
func (b *StatsBucket) AvgSpeedKbps() float64 {
	if b.SpeedCount <= 0 {
		return 0
	}
	return b.SpeedSumKbps / float64(b.SpeedCount)
}

// bucketSeconds 颗粒度换算为秒;非正数返回错误(调用方保证入参来自白名单)。
func bucketSeconds(bucket time.Duration) (int64, error) {
	sec := int64(bucket / time.Second)
	if sec <= 0 {
		return 0, fmt.Errorf("颗粒度必须为正秒数,得到 %v", bucket)
	}
	return sec, nil
}

// GetStatsBuckets 按 bucket 粒度聚合 [from, to](闭区间)内的轮次。
// 分桶对 scheduled_at(UTC Unix 秒)做整数除法取桶起点,故桶边界天然与 UTC 对齐:
// 1m/5m/30m/1h/1d 都整齐;不返回没有轮次的空桶(前端可据此断线)。
func (s *Store) GetStatsBuckets(ctx context.Context, monitorID ID,
	from, to time.Time, bucket time.Duration) ([]*StatsBucket, error) {
	sec, err := bucketSeconds(bucket)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT (scheduled_at / ?) * ? AS bucket_at,
			COUNT(*), COALESCE(SUM(valid),0), COALESCE(SUM(success),0),
			COALESCE(SUM(latency_sum_ms),0), COALESCE(SUM(latency_count),0),
			COALESCE(SUM(speed_sum_kbps),0), COALESCE(SUM(speed_count),0)
		FROM rounds
		WHERE monitor_id=? AND scheduled_at>=? AND scheduled_at<=?
		GROUP BY bucket_at ORDER BY bucket_at ASC`,
		sec, sec, monitorID.Hex(), unixSec(from), unixSec(to))
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*StatsBucket{}
	for rows.Next() {
		var (
			st       StatsBucket
			bucketAt int64
		)
		if err = rows.Scan(&bucketAt, &st.Rounds, &st.Valid, &st.Success,
			&st.LatencySumMs, &st.LatencyCount, &st.SpeedSumKbps, &st.SpeedCount); err != nil {
			return nil, normalizeErr(err)
		}
		st.BucketAt = fromUnixSec(bucketAt)
		out = append(out, &st)
	}
	return out, normalizeErr(rows.Err())
}

// AgentLatencyPoint 某节点在某个粒度桶内的平均延时(详情页分节点曲线用)。
type AgentLatencyPoint struct {
	AgentID      ID        `json:"agentId"`
	BucketAt     time.Time `json:"bucketAt"`
	AvgLatencyMs float64   `json:"avgLatencyMs"`
	Count        int       `json:"count"`
}

// GetAgentLatencyBuckets 按「节点 + 任意粒度桶」聚合原始结果的延时均值。
// 分桶与过滤统一以 results.scheduled_at(所属轮次计划时间)为准,与主趋势同口径、
// 同桶边界,前端可按桶起点对齐到同一横轴。
// 只统计非晚到样本(与轮次聚合一致);原始结果受保留期约束,超出保留期的窗口
// 没有节点曲线(主趋势走 rounds,不受影响),按实际有样本的桶返回。
func (s *Store) GetAgentLatencyBuckets(ctx context.Context, monitorID ID,
	from, to time.Time, bucket time.Duration) ([]*AgentLatencyPoint, error) {
	sec, err := bucketSeconds(bucket)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,
			(scheduled_at / ?) * ? AS bucket_at, AVG(latency_ms), COUNT(*)
		FROM results
		WHERE monitor_id=? AND late=0 AND scheduled_at>=? AND scheduled_at<=?
		GROUP BY agent_id, bucket_at ORDER BY bucket_at ASC`,
		sec, sec, monitorID.Hex(), unixSec(from), unixSec(to))
	if err != nil {
		return nil, normalizeErr(err)
	}
	defer rows.Close()
	out := []*AgentLatencyPoint{}
	for rows.Next() {
		var (
			pt       AgentLatencyPoint
			bucketAt int64
		)
		if err = rows.Scan(&pt.AgentID, &bucketAt, &pt.AvgLatencyMs, &pt.Count); err != nil {
			return nil, normalizeErr(err)
		}
		pt.BucketAt = fromUnixSec(bucketAt)
		out = append(out, &pt)
	}
	return out, normalizeErr(rows.Err())
}

// AvailabilityWindow 单个窗口的可用率;OK=false 表示窗口内无样本。
type AvailabilityWindow struct {
	Window time.Duration
	Rate   float64
	OK     bool
}

// Availability 指定窗口内的可用率(汇总计数口径,spec Q12-A):
// sum(success)/sum(valid);无样本返回 ok=false。单窗口的便捷包装。
func (s *Store) Availability(ctx context.Context, monitorID ID, window time.Duration) (rate float64, ok bool, err error) {
	got, err := s.Availabilities(ctx, monitorID, window)
	if err != nil || len(got) == 0 {
		return 0, false, err
	}
	return got[0].Rate, got[0].OK, nil
}

// Availabilities 一次读取最长窗口的小时聚合,再按各窗口切分汇总,返回顺序与入参一致。
// 总览卡片要同时展示 24h/7d/30d,三者共用一次查询,避免对同一监控重复聚合。
// 窗口起点口径与 GetHourlyStatsSince 一致(截断到小时,故不足一小时的余量也算入窗口)。
func (s *Store) Availabilities(ctx context.Context, monitorID ID,
	windows ...time.Duration) ([]AvailabilityWindow, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	longest := windows[0]
	for _, w := range windows[1:] {
		if w > longest {
			longest = w
		}
	}
	now := time.Now()
	stats, err := s.GetHourlyStatsSince(ctx, monitorID, now.Add(-longest))
	if err != nil {
		return nil, err
	}
	out := make([]AvailabilityWindow, 0, len(windows))
	for _, w := range windows {
		cutoff := HourBucket(now.Add(-w))
		var valid, success int
		for _, st := range stats {
			if st.HourBucket < cutoff {
				continue
			}
			valid += st.Valid
			success += st.Success
		}
		item := AvailabilityWindow{Window: w}
		if valid > 0 {
			item.Rate = float64(success) / float64(valid) * 100
			item.OK = true
		}
		out = append(out, item)
	}
	return out, nil
}

// TableSize 单张表的占用明细(字节)。
type TableSize struct {
	Name        string `json:"name"`
	Count       int64  `json:"count"`
	DataSize    int64  `json:"dataSize"`
	StorageSize int64  `json:"storageSize"`
	IndexSize   int64  `json:"indexSize"`
	TotalSize   int64  `json:"totalSize"`
}

// DBStats 数据库整体占用:页数与页大小换算 + 各表行数/占用(按合计降序)。
type DBStats struct {
	Name        string `json:"name"`
	Collections int    `json:"collections"`
	Objects     int64  `json:"objects"`
	DataSize    int64  `json:"dataSize"`
	StorageSize int64  `json:"storageSize"`
	IndexSize   int64  `json:"indexSize"`
	TotalSize   int64  `json:"totalSize"`
	// FreePages/FreeSize:库文件里的空闲页(freelist)及折算字节数 —— DELETE 与保留期
	// 清理留下的空洞。它是「压缩数据库」能回收的上限,0 表示库文件已是紧凑状态。
	FreePages int64       `json:"freePages"`
	FreeSize  int64       `json:"freeSize"`
	Items     []TableSize `json:"items"`
	// DailyGrowth:按当前启用的监控与其检测周期,预估每天新增的行数与字节量。
	// 字节系数来自本库的实测摊销(数据行 + 索引摊到每行),只做量级参考,
	// 不是精确值 —— 随着监控增删/调速会自动跟上,读的是当下的配置。
	DailyGrowth *DailyGrowth `json:"dailyGrowth,omitempty"`
}

// DailyGrowth 数据库每日增长预估。
type DailyGrowth struct {
	// Rounds/Results:每天将新增的轮次行数与原始结果行数(按启用监控的 period 折算)。
	Rounds  int64 `json:"rounds"`
	Results int64 `json:"results"`
	// Bytes:上述行数乘以实测摊销字节系数(含索引)的每日字节增量。
	Bytes int64 `json:"bytes"`
	// EnabledMonitors 参与统计的启用监控数(=0 时预估不可用)。
	EnabledMonitors int `json:"enabledMonitors"`
}

// 每行摊销字节(行 + 索引摊到每行),来自生产库 dbstat 实测
// (results ≈312 B/行,rounds ≈390 B/行),取整做量级估算用。
const (
	bytesPerResult = 320
	bytesPerRound  = 400
)

// EstimateDailyGrowth 按启用监控的检测周期预估每天新增的轮次与结果行数、字节量。
// 各类监控(含 push)统一按 period 建轮(见 scheduler),结果行数 ≈ 轮次数 × 指派节点数;
// 节点数取当前在线/全部节点数的较大口径不可得,按全部注册节点数估(上界口径)。
func (s *Store) EstimateDailyGrowth(ctx context.Context) (*DailyGrowth, error) {
	var (
		enabled   int
		periodSum int64
		agents    int64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(period),0) FROM monitors WHERE enabled=1`,
	).Scan(&enabled, &periodSum); err != nil {
		return nil, normalizeErr(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&agents); err != nil {
		return nil, normalizeErr(err)
	}
	if enabled == 0 {
		return &DailyGrowth{}, nil
	}
	// 平均周期(秒)→ 每监控每天轮数 86400/period,× 启用监控数。
	roundsPerDay := int64(86400) * int64(enabled) / (periodSum / int64(enabled))
	resultsPerDay := roundsPerDay * agents
	return &DailyGrowth{
		Rounds:          roundsPerDay,
		Results:         resultsPerDay,
		Bytes:           roundsPerDay*bytesPerRound + resultsPerDay*bytesPerResult,
		EnabledMonitors: enabled,
	}, nil
}

// 业务表清单(诊断展示按此顺序遍历;不含 SQLite 内部表)。
var statTables = []string{
	TableAgents, TableMonitors, TableRounds, TableResults, TableHourlyStats,
	TableChannels, TableSettings, TableUsers, TableMonitorStates, TableStateChanges,
}

// dbStatsCacheTTL 占用统计的缓存时长。够短:后台重算完成后最多 5 秒即读到新值
// (且刷新会主动失效);够长:轮询期间不重复全库扫描。
const dbStatsCacheTTL = 5 * time.Second

// dbStatsCache DBStats 的缓存值(带时间戳)。
type dbStatsCache struct {
	at    time.Time
	stats *DBStats
}

// DBStats 读取数据库占用(只读缓存):总占用走 page_count*page_size,逐表行数走
// COUNT(*),逐表字节数走 dbstat 虚表(dbstat 不可用时字节列回落 0,不影响接口可用)。
// 只读诊断用途,供设置页展示。
//
// 缓存语义(供 API 层的「异步刷新 + 前端轮询」使用),任何路径都不阻塞在慢查询上:
//   - 缓存未过期:立即返回缓存值。
//   - 缓存过期但有旧值:立即返回旧值,同时起一个后台 goroutine 重算
//     (RefreshDBStats 同样入口,重入安全)。
//   - 无任何缓存(首次访问/压缩后):返回 nil 并触发后台重算,前端拿到
//     stats=null + computing=true 后轮询到重算落地再展示。
//     首次点击「查看占用」曾在这里同步扫全库,库大或赶上写排队时把 GET
//     拖过前端 15s 超时 —— 统计必须永远在后台跑。
//   - 压缩(Compact)结束会作废缓存并让 epoch+1:仍在跑的后台重算写回时发现自己
//     epoch 落后即丢弃,避免把压缩前的旧口径数字写回缓存。
func (s *Store) DBStats() *DBStats {
	s.statsMu.Lock()
	if s.statsCache != nil && time.Since(s.statsCache.at) < dbStatsCacheTTL {
		out := s.statsCache.stats
		s.statsMu.Unlock()
		return out
	}
	haveStale := s.statsCache != nil
	s.statsMu.Unlock()

	if haveStale {
		// 返回旧值顶住页面,重算交给后台:dbstat 全库扫描赶上写排队可能很久,
		// 不能让 GET 请求一直挂着(15s 级别的前端超时会把它掐死)。
		s.RefreshDBStats()
		s.statsMu.Lock()
		defer s.statsMu.Unlock()
		return s.statsCache.stats
	}
	// 无任何缓存:同样交给后台,立即返回 nil。页面打开暂时没数字,由前端
	// 占位区 + 轮询接管(DBStatsComputing 会为 true)。
	s.RefreshDBStats()
	return nil
}

// RefreshDBStats 在后台重算数据库占用并更新缓存。多次调用只会有一个重算在跑,
// 其余直接返回(结果反正会进同一份缓存)。
func (s *Store) RefreshDBStats() {
	s.statsMu.Lock()
	if s.statsRefreshing {
		s.statsMu.Unlock()
		return
	}
	s.statsRefreshing = true
	epoch := s.statsEpoch
	s.statsMu.Unlock()

	go func() {
		// 后台重算用独立的超时上下文,与触发它的请求生命周期解耦。
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := s.readDBStats(ctx)
		s.statsMu.Lock()
		// epoch 变了 = 中途发生过压缩作废:本次读数已是压缩前口径,写回会覆盖成旧值,丢弃。
		if err == nil && epoch == s.statsEpoch {
			s.statsCache = &dbStatsCache{at: time.Now(), stats: out}
		}
		s.statsRefreshing = false
		s.statsMu.Unlock()
		// 失败不重试也不上报:下次 TTL 过期/手动刷新自然重来,缓存里还有旧值可读。
		_ = err
	}()
}

// DBStatsComputing 是否正有占用统计在计算(后台重算在跑,或还没有任何缓存值)。
// 前端轮询 GET /settings/db-stats 以此为退出条件:返回 false 即拿到的是新鲜数据。
func (s *Store) DBStatsComputing() bool {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.statsRefreshing || s.statsCache == nil
}

// invalidateDBStats 清掉占用缓存(压缩结束时调用)。清空后下一次 DBStats 走
// 「无缓存值」路径同步重算,保证压缩弹窗里「压缩后刷新」立即对上。
func (s *Store) invalidateDBStats() {
	s.statsMu.Lock()
	s.statsCache = nil
	s.statsEpoch++
	s.statsMu.Unlock()
}

// readDBStats 实际执行占用统计(不写缓存;写回由调用方决定,后台路径要过 epoch 检查)。
func (s *Store) readDBStats(ctx context.Context) (*DBStats, error) {
	out := &DBStats{Name: s.path}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&out.TotalSize); err != nil {
		return nil, normalizeErr(err)
	}
	var pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return nil, normalizeErr(err)
	}
	out.TotalSize *= pageSize
	out.StorageSize = out.TotalSize
	out.DataSize = out.TotalSize
	out.Collections = len(statTables)
	// 空闲页(可回收空间):读不到不影响其余统计。
	_ = s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&out.FreePages)
	out.FreeSize = out.FreePages * pageSize

	sizes, dbstatOK := s.tableBytes(ctx)

	out.Items = make([]TableSize, 0, len(statTables))
	for _, name := range statTables {
		item := TableSize{Name: name}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+name).Scan(&item.Count); err != nil {
			return nil, normalizeErr(err)
		}
		if dbstatOK {
			item.DataSize = sizes[name]
			// dbstat 的 pgsize 已含该表的索引页,索引项独立统计需额外拆解,
			// 这里按「可见占用 = 表页」呈现,索引列保持 0。
			item.TotalSize = item.DataSize
		}
		item.StorageSize = item.DataSize
		out.Objects += item.Count
		out.Items = append(out.Items, item)
	}
	// 每日增长预估失败不影响占用明细本身(诊断信息,能出多少出多少)。
	if g, err := s.EstimateDailyGrowth(ctx); err == nil {
		out.DailyGrowth = g
	}
	return out, nil
}

// tableBytes 读取各表占用字节(dbstat 是 SQLite 内置虚表);不可用时返回 ok=false。
func (s *Store) tableBytes(ctx context.Context) (map[string]int64, bool) {
	out := map[string]int64{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, SUM(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return out, false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name string
			size int64
		)
		if err = rows.Scan(&name, &size); err == nil {
			out[name] = size
		}
	}
	return out, true
}

// pruneBatchSize 保留期清理的单批删除行数。
// SQLite 同时只允许一个写事务,一次性删除整表会让写路径长时间等待,
// 故分批删除并让出连接(ADR-0005)。
const pruneBatchSize = 5000

// pruneMaxBatches 单次清理的批数上限,避免极端情况下长时间占用调度协程;
// 剩余部分由下一轮(每小时)继续清理。
const pruneMaxBatches = 200

// HourlyStatsRetentionDays 小时级聚合的固定保留天数。
//
// 31 = 最长可用率窗口(总览卡片的 30 天)+ 1 天余量。窗口起点按小时截断
// (见 Availabilities),留一天余量是为了边界那个桶不因清理而缺角。
//
// 这是**固定策略,不是可配项**:唯一读者 Availabilities 最长只查 30 天,而
// GetHourlyStatsSince 本身就带 hour>=since 过滤,更早的桶没有任何读取路径;
// 但反过来,一旦保留期短于最长可用率窗口,30 天可用率会静默失真(不是报错,
// 而是分母变小),所以不交给用户去调。
//
// 注意:它和 DefaultResultRetentionDays 是**两条独立的保留策略**,只是当前取值
// 恰好相同 —— 前者管 hourly_stats,后者管 results,改其一时不要顺手同步另一个。
const HourlyStatsRetentionDays = 31

// PruneOldRounds 删除 cutoff(scheduled_at 口径)之前的已定稿轮次。
// 轮次曾设计为永不清理(趋势图唯一数据源),但 60 秒粒度的老龄点没有阅读场景——
// 长窗口可用率与趋势由 hourly_stats 的小时桶承接,故纳入与 results 相同的保留期。
// 仅删非 OPEN 行:残留 OPEN 轮次由启动收口兜底,不与清理竞争。分批删除并让出
// 唯一写连接(ADR-0005)。返回删除条数。
func (s *Store) PruneOldRounds(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for i := 0; i < pruneMaxBatches; i++ {
		res, err := s.db.ExecContext(ctx, `DELETE FROM rounds WHERE id IN (
			SELECT id FROM rounds WHERE scheduled_at < ? AND state<>? LIMIT ?)`,
			unixSec(cutoff), RoundStateOpen, pruneBatchSize)
		if err != nil {
			return total, normalizeErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, normalizeErr(err)
		}
		total += n
		if n < pruneBatchSize {
			break
		}
	}
	return total, nil
}

// PruneOldHourlyStats 删除 cutoff 之前的小时聚合桶(票 09 后台清理)。
//
// hour 是 YYYYMMDDHH(UTC)的定宽文本键,字典序即时间序,故直接按字符串比较;
// 与 PruneOldResults 一样分批删除并让出唯一写连接(ADR-0005)。
// 返回删除条数。
func (s *Store) PruneOldHourlyStats(ctx context.Context, cutoff time.Time) (int64, error) {
	before := HourBucket(cutoff)
	var total int64
	for i := 0; i < pruneMaxBatches; i++ {
		res, err := s.db.ExecContext(ctx, `DELETE FROM hourly_stats WHERE id IN (
			SELECT id FROM hourly_stats WHERE hour < ? LIMIT ?)`,
			before, pruneBatchSize)
		if err != nil {
			return total, normalizeErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, normalizeErr(err)
		}
		total += n
		if n < pruneBatchSize {
			break
		}
	}
	return total, nil
}

// PruneOldResults 删除 cutoff 之前的原始结果(票 09 后台清理,不碰聚合)。
// 聚合的清理是独立的一条策略,见 PruneOldHourlyStats。
// 返回删除条数。
func (s *Store) PruneOldResults(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for i := 0; i < pruneMaxBatches; i++ {
		res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE id IN (
			SELECT id FROM results WHERE created_at < ? LIMIT ?)`,
			unixSec(cutoff), pruneBatchSize)
		if err != nil {
			return total, normalizeErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, normalizeErr(err)
		}
		total += n
		if n < pruneBatchSize {
			break
		}
	}
	return total, nil
}

// CountResults 统计结果行数(测试与诊断用);cutoff 非零时只统计 createdAt 早于它的行。
func (s *Store) CountResults(ctx context.Context, cutoff time.Time) (int64, error) {
	var (
		n   int64
		err error
	)
	if cutoff.IsZero() {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM results`).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM results WHERE created_at < ?`, unixSec(cutoff)).Scan(&n)
	}
	return n, normalizeErr(err)
}

// BackdateResultForTest 把某条结果的 createdAt 改到指定时间(测试构造过期数据用)。
func (s *Store) BackdateResultForTest(ctx context.Context, id ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE results SET created_at=? WHERE id=?`, unixSec(at), id)
	return normalizeErr(err)
}

// EraseAll 清空全部业务数据,供测试做用例间隔离(settings 保留,便于复用接入密钥)。
func (s *Store) EraseAll(ctx context.Context) error {
	for _, table := range []string{
		TableAgents, TableMonitors, TableRounds, TableResults, TableHourlyStats,
		TableChannels, TableUsers, TableMonitorStates,
	} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return normalizeErr(err)
		}
	}
	return nil
}
