// Package probe 是探测执行器的共享实现:Agent 运行时调用,
// Dashboard 集成测试的“测试替身节点”也调用同一份代码(spec 测试决策)。
// 探测器按 monitor_type 注册(PING 由 Agent 侧在票 08 注册 ICMP 实现)。
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// Executor 执行一次探测并产出结果载荷(字段 RoundID/MonitorID 由 Execute 回填)。
type Executor func(ctx context.Context, check json.RawMessage) (*protocol.ProbeResultPayload, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Executor{}
)

// Register 注册某探测类型的执行器;重复注册覆盖(agent 启动时调用)。
func Register(monitorType string, ex Executor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[monitorType] = ex
}

func lookup(monitorType string) (Executor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	ex, ok := registry[monitorType]
	return ex, ok
}

// Execute 执行任务对应探测;未注册类型返回失败结果而非错误(调用方照常回传)。
func Execute(ctx context.Context, task *protocol.ProbeTaskPayload) *protocol.ProbeResultPayload {
	now := time.Now().Unix()
	failed := func(msg string) *protocol.ProbeResultPayload {
		return &protocol.ProbeResultPayload{
			RoundID: task.RoundID, MonitorID: task.MonitorID,
			OK: false, Error: msg, StartedAtUnix: now, FinishedAtUnix: now,
		}
	}
	ex, ok := lookup(task.MonitorType)
	if !ok {
		return failed(fmt.Sprintf("本节点不支持探测类型: %s", task.MonitorType))
	}
	res, err := ex(ctx, task.Check)
	if err != nil || res == nil {
		if err == nil {
			err = fmt.Errorf("探测执行器返回空结果")
		}
		return failed(err.Error())
	}
	res.RoundID = task.RoundID
	res.MonitorID = task.MonitorID
	return res
}

func init() {
	Register(checkconfig.TypeHTTP, executeHTTP)
}
