package store

import (
	"context"
	"errors"
	"os"
	"time"
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

// Compact 压缩数据库文件:VACUUM 按当前数据整体重写库文件,回收 DELETE 与保留期清理
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
func (s *Store) Compact(ctx context.Context) (*CompactResult, error) {
	if !s.compactMu.TryLock() {
		return nil, ErrCompactBusy
	}
	defer s.compactMu.Unlock()

	start := time.Now()
	before, free, err := s.pageBytes(ctx)
	if err != nil {
		return nil, err
	}
	walBefore := s.walBytes()

	// 先把 WAL 落盘并截断再 VACUUM:WAL 里可能还压着上一次清理的空闲页记录,
	// 不落盘的话 VACUUM 看到的不是库文件的最终形态。
	if _, err = s.checkpointWAL(ctx); err != nil {
		return nil, err
	}
	if _, err = s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return nil, normalizeErr(err)
	}
	// VACUUM 先把重写后的内容写进新的 WAL,再截断一次才算真正落盘、文件才变小。
	if _, err = s.checkpointWAL(ctx); err != nil {
		return nil, err
	}

	after, _, err := s.pageBytes(ctx)
	if err != nil {
		return nil, err
	}
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

// pageBytes 库文件占用(页数 × 页大小)与空闲页数;口径与 DBStats 一致。
func (s *Store) pageBytes(ctx context.Context) (total, free int64, err error) {
	var pages, pageSize int64
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, 0, normalizeErr(err)
	}
	if err = s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, normalizeErr(err)
	}
	// 空闲页只用于解释"能压出多少",读不到就降级为 0,不让它挡掉压缩本身。
	_ = s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	return pages * pageSize, free, nil
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