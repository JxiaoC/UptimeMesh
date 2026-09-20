package store

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/gogf/gf/v2/frame/g"
)

// ErrCompactBusy 已有一轮压缩正在执行。压缩会长时间独占唯一写连接,
// 排队第二次只会让写入等待翻倍,故直接拒绝(设置页按 409 提示)。
var ErrCompactBusy = errors.New("数据库压缩正在进行中")

// CompactResult 数据库压缩的结果。字节口径与 DBStats 一致(库文件 = 页数 × 页大小),
// 故界面上「压缩前」正是点按钮那一刻显示的库文件占用,压缩后刷新即可对上。
type CompactResult struct {
	// BeforeBytes/AfterBytes 压缩前/后的库文件占用(page_count × page_size)。
	BeforeBytes int64 `json:"beforeBytes"`
	AfterBytes  int64 `json:"afterBytes"`
	// SavedBytes = BeforeBytes - AfterBytes。正常 ≥ 0;并发写入恰好落在两次读数之间时
	// 可能为负(压缩本身不产生数据,仅表示这一刻没变小)。
	SavedBytes int64 `json:"savedBytes"`
	// FreePages 压缩前库文件里的空闲页数(freelist_count):即"这次能回收多少"的依据,
	// 0 表示库文件已是紧凑状态。
	FreePages int64 `json:"freePages"`
	// WalBytes 顺带截断掉的 -wal 字节数。它不计入 Before/AfterBytes(那两个是主库页数口径),
	// 故磁盘实际腾出的空间可能是 SavedBytes + WalBytes。
	WalBytes int64 `json:"walBytes"`
	// DurationMs 本次压缩耗时(毫秒),压缩期间写路径都在排队。
	DurationMs int64 `json:"durationMs"`
}

// 压缩进行到的阶段(状态接口的 stage 字段,供排查与前端展示)。
const (
	CompactStagePreparing = "preparing" // 停占用统计、读压缩前口径、WAL 落盘
	CompactStageVacuum    = "vacuum"    // VACUUM 重建库文件(最耗时的一段)
	CompactStageFinishing = "finishing" // WAL 收尾截断、读压缩后口径
)

// compactMaxDuration 后台压缩的超时兜底:压缩脱离请求生命周期后,没有这个上限,
// 异常(如磁盘假死)会让 goroutine 永远持有唯一写连接。正常压缩远快于此
// (线上 1.7GB 库为数十秒级),10 分钟只兜极端情况。
const compactMaxDuration = 10 * time.Minute

// CompactStatus 异步压缩的状态快照:POST /settings/db-compact 触发后台压缩后,
// 前端轮询 GET /settings/db-compact/status 读它,直到 Running=false 再读 Result/Err。
type CompactStatus struct {
	// Running 压缩正在后台执行。
	Running bool `json:"running"`
	// Progress 估算进度 0~100;-1 表示暂无法估算(前端转不确定动画)。
	// VACUUM 本身不提供进度回调,这里按压缩期间 WAL 的增长量对比压缩前数据页
	// 体积折算(见 runCompact),只是估算,仅供界面参考。
	Progress int `json:"progress"`
	// Stage 当前阶段(CompactStage* 常量);未在压缩时为空。
	Stage string `json:"stage,omitempty"`
	// StartedAt/FinishedAt 本次压缩的开始/结束时间(异步轮询据此判断是不是同一轮)。
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Result 最近一次成功压缩的结果;失败时为 nil。
	Result *CompactResult `json:"result,omitempty"`
	// Err 最近一次失败的原因(已结束且失败时非空)。
	Err string `json:"error,omitempty"`
}

// CompactStatus 返回当前压缩状态的快照(值拷贝;Result 指针只读共享)。
func (s *Store) CompactStatus() CompactStatus {
	s.compactStatusMu.Lock()
	defer s.compactStatusMu.Unlock()
	return s.compactStatus
}

// setCompactStatus 更新状态快照。
func (s *Store) setCompactStatus(fn func(*CompactStatus)) {
	s.compactStatusMu.Lock()
	defer s.compactStatusMu.Unlock()
	fn(&s.compactStatus)
}

// setCompactProgress 更新阶段与估算进度。
func (s *Store) setCompactProgress(stage string, progress int) {
	s.setCompactStatus(func(st *CompactStatus) {
		st.Stage = stage
		st.Progress = progress
	})
}

// StartCompact 触发后台压缩并立即返回(POST /settings/db-compact 的后端语义)。
//
// 与 db-stats 同款的异步语义:压缩可能耗时数十秒以上,同步等待会被反代/前端超时
// 掐断 —— 请求被掐了服务端其实还在压缩,管理员却只看到报错。这里 POST 只负责触发,
// 执行在后台 goroutine;前端轮询 CompactStatus 拿阶段、估算进度与结果。
//
// 并发语义不变:已在压缩时返回 ErrCompactBusy,不让第二个请求白白排队。
func (s *Store) StartCompact() error {
	if !s.compactMu.TryLock() {
		return ErrCompactBusy
	}
	// 压缩与触发它的请求生命周期解耦,不能跟着请求 ctx 一起被取消;
	// 超时兜底防异常时 goroutine 常驻并永久占住唯一写连接。
	ctx, cancel := context.WithTimeout(context.Background(), compactMaxDuration)
	go func() {
		defer cancel()
		defer s.compactMu.Unlock()
		if _, err := s.runCompactTracked(ctx); err != nil {
			// 没有请求上下文可记,用包级 logger:失败必须留痕,否则前端只会看到
			// 「压缩失败」,无从排查。
			g.Log().Errorf(ctx, "数据库压缩失败: %v", err)
		}
	}()
	return nil
}

// Compact 同步压缩(测试与内部使用):等待压缩完成后返回结果。
// 异步路径走 StartCompact;两者共用 runCompactTracked,状态快照口径一致。
func (s *Store) Compact(ctx context.Context) (*CompactResult, error) {
	if !s.compactMu.TryLock() {
		return nil, ErrCompactBusy
	}
	defer s.compactMu.Unlock()
	return s.runCompactTracked(ctx)
}

// runCompactTracked 在 compactMu 已被调用方持有的前提下执行压缩并维护状态快照。
func (s *Store) runCompactTracked(ctx context.Context) (*CompactResult, error) {
	s.setCompactStatus(func(st *CompactStatus) {
		*st = CompactStatus{
			Running:   true,
			Progress:  0,
			Stage:     CompactStagePreparing,
			StartedAt: time.Now(),
		}
	})
	res, err := s.runCompact(ctx)
	s.setCompactStatus(func(st *CompactStatus) {
		st.Running = false
		st.FinishedAt = time.Now()
		st.Progress = 100
		if err != nil {
			st.Err = err.Error()
		} else {
			st.Result = res
		}
	})
	// 触发者(管理员名)在 API 层的触发日志里;这里补压缩本身的落账,
	// 与旧同步版 API 层的日志口径一致,审计与排查都需要。
	if err == nil {
		g.Log().Infof(ctx, "数据库压缩完成:%d → %d 字节(释放 %d,空闲页 %d,WAL 截断 %d,用时 %dms)",
			res.BeforeBytes, res.AfterBytes, res.SavedBytes, res.FreePages, res.WalBytes, res.DurationMs)
	}
	return res, err
}

// runCompact 实际执行压缩:VACUUM 按当前数据整体重写库文件,回收 DELETE 与保留期清理
// 留下的空闲页、消除碎片,并把 WAL 截断为零字节。
//
// 为什么"压缩"是 VACUUM:SQLite 没有内置的行级/页级数据压缩 —— 官方 ZIPVFS 属收费的
// SEE(加密扩展家族),第三方方案(如 sqlite-zstd)是可加载扩展,既需要 CGO 或
// `load_extension`,又与本项目 CGO_ENABLED=0 + 纯 Go 驱动(ADR-0005)的硬约束冲突。
// 在不动存储格式、不引入外部依赖的前提下,官方能把库文件变小的手段只有 VACUUM。
// 见 ADR-0006。
//
// 代价与边界:VACUUM 期间持有唯一写连接(SQLite 的独占锁),轮次与结果的写入会排队,
// 库很大时可能持续数十秒;因此只由管理员在设置页显式触发,不做任何自动执行。
// 压缩不删除任何数据、不改表结构、不改查询结果,只是把空闲页还回去。
//
// 进度估算:VACUUM 不提供进度回调,但 WAL 模式下 VACUUM 是单个事务,新页全部写入
// WAL 且中途不自动 checkpoint,故 WAL 单调增长;执行前的 wal_checkpoint(TRUNCATE)
// 已把 WAL 截为零,增长量从零起算。按「WAL 增长量 / 压缩前数据页体积」折算即得
// 够用的估算值。旁路 goroutine 定期 os.Stat -wal 文件 —— 不碰连接,与 VACUUM 无锁冲突。
func (s *Store) runCompact(ctx context.Context) (*CompactResult, error) {
	start := time.Now()
	// 先停掉在跑的占用统计重算:统计的慢查询在读池上握着读锁,
	// WAL 下 VACUUM 需要独占,不取消的话压缩会撞锁失败(统计之后会自动重来)。
	s.cancelDBStatsRefresh()
	before, free, pageSize, err := s.pageBytes(ctx)
	if err != nil {
		return nil, err
	}
	walBefore := s.walBytes()

	// 先把 WAL 落盘并截断再 VACUUM:WAL 里可能还压着上一次清理的空闲页记录,
	// 不落盘的话 VACUUM 看到的不是库文件的最终形态(截断也让 WAL 增长从零起算)。
	if _, err = s.checkpointWAL(ctx); err != nil {
		return nil, err
	}
	s.setCompactProgress(CompactStagePreparing, 5)

	// 估算总量 = 压缩前数据页(非空闲页)× 页大小:VACUUM 只重写数据页,空闲页不参与。
	estBytes := (before/pageSize - free) * pageSize
	vacuumDone := make(chan struct{})
	go func() {
		defer close(vacuumDone)
		base := s.walBytes() // checkpoint 之后的基线(截断成功时为 0)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			grown := s.walBytes() - base
			if grown <= 0 {
				continue
			}
			// 估算不出(estBytes<=0)时报 -1,前端转不确定动画,不给假数字。
			if estBytes <= 0 {
				s.setCompactProgress(CompactStageVacuum, -1)
				continue
			}
			p := 5 + int(float64(grown)/float64(estBytes)*90)
			if p > 99 {
				p = 99 // VACUUM 返回前不给 100:收尾(截 WAL、读压缩后口径)还没做
			}
			s.setCompactProgress(CompactStageVacuum, p)
		}
	}()

	_, err = s.db.ExecContext(ctx, `VACUUM`)
	<-vacuumDone
	if err != nil {
		return nil, normalizeErr(err)
	}
	s.setCompactProgress(CompactStageFinishing, 99)

	// VACUUM 先把重写后的内容写进新的 WAL,再截断一次才算真正落盘、文件才变小。
	if _, err = s.checkpointWAL(ctx); err != nil {
		return nil, err
	}
	after, _, _, err := s.pageBytes(ctx)
	if err != nil {
		return nil, err
	}
	// 占用口径已被 VACUUM 改变:立刻作废缓存,保证设置页「压缩后刷新」读到的是
	// 压缩后的真实占用,而不是缓存里的压缩前数字。
	s.invalidateDBStats()
	out := &CompactResult{
		BeforeBytes: before,
		AfterBytes:  after,
		SavedBytes:  before - after,
		FreePages:   free,
		WalBytes:    walBefore - s.walBytes(),
		DurationMs:  time.Since(start).Milliseconds(),
	}
	return out, nil
}

// pageBytes 库文件占用(页数 × 页大小)、空闲页数与页大小;口径与 DBStats 一致。
func (s *Store) pageBytes(ctx context.Context) (total, free, pageSize int64, err error) {
	var pages int64
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, 0, 0, normalizeErr(err)
	}
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, 0, normalizeErr(err)
	}
	// 空闲页只用于解释"能压出多少",读不到就降级为 0,不让它挡掉压缩本身。
	_ = s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	return pages * pageSize, free, pageSize, nil
}

// checkpointWAL 把 WAL 内容落盘并截断文件,返回是否完整完成(有别的读事务时会 busy)。
// 本项目连接池固定 1 条连接(ADR-0005),故正常情况下不会再忙。
func (s *Store) checkpointWAL(ctx context.Context) (bool, error) {
	var busy, logFrames, checkpointed int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return false, normalizeErr(err)
	}
	return busy == 0, nil
}

// walBytes -wal 文件的当前字节数;内存库或文件不存在时为 0。
func (s *Store) walBytes() int64 {
	info, err := os.Stat(s.path + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}
