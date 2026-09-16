// Package retention 周期删除超保留期的数据。两条策略互相独立:
//   - 原始检测结果:store.DefaultResultRetentionDays 天,后台可配(1~365);
//   - 小时级聚合:store.HourlyStatsRetentionDays 天,固定不可配。
//
// 启动时立即执行一次,随后每小时对账。
package retention

import (
	"context"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"github.com/uptimemesh/dashboard/internal/store"
)

type Task struct {
	st    *store.Store
	every time.Duration
}

func New(st *store.Store) *Task {
	return &Task{st: st, every: time.Hour}
}

// Start 在 ctx 存续期间循环执行清理;ctx 取消即退出。
func (t *Task) Start(ctx context.Context) {
	t.once(ctx)
	go func() {
		ticker := time.NewTicker(t.every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.once(ctx)
			}
		}
	}()
}

// RunOnce 供测试直接调用。
func (t *Task) RunOnce(ctx context.Context) { t.once(ctx) }

// once 依次执行两条独立策略。分成两个函数而不是串在一个流程里:小时聚合的保留期
// 是固定值,不依赖 settings —— 读配置失败(库异常)时不该连带跳过它。
func (t *Task) once(ctx context.Context) {
	t.pruneResults(ctx)
	t.pruneHourlyStats(ctx)
}

// pruneResults 按设置里的保留期清理原始检测结果。
func (t *Task) pruneResults(ctx context.Context) {
	st, err := t.st.GetSettings(ctx)
	if err != nil {
		g.Log().Errorf(ctx, "读取保留期配置失败: %v", err)
		return
	}
	days := st.ResultRetentionDays
	if days <= 0 {
		days = store.DefaultResultRetentionDays
	}
	n, err := t.st.PruneOldResults(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		g.Log().Errorf(ctx, "清理过期结果失败: %v", err)
		return
	}
	if n > 0 {
		g.Log().Infof(ctx, "已清理 %d 条超过 %d 天的原始检测结果", n, days)
	}
}

// pruneHourlyStats 清理超过 store.HourlyStatsRetentionDays 天的小时聚合桶。
// 保留期固定(必须覆盖最长可用率窗口 30 天),故不读 settings。
func (t *Task) pruneHourlyStats(ctx context.Context) {
	days := store.HourlyStatsRetentionDays
	n, err := t.st.PruneOldHourlyStats(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		g.Log().Errorf(ctx, "清理过期小时聚合失败: %v", err)
		return
	}
	if n > 0 {
		g.Log().Infof(ctx, "已清理 %d 个小时级聚合桶(超过 %d 天)", n, days)
	}
}
