package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// ExecuteTest 执行一次"测试"探测(节点侧 probe_test 帧的入口),并采集请求/响应明细。
//
// 与 Execute 的三点区别:
//   - 不建轮次、不落库:结果不回 Dashboard 的结果表,只回给发起测试的那次请求;
//   - HTTP 额外采集明细(实际请求、响应码、响应头、响应体片段、重定向链),
//     判定口径与轮次完全一致(同一个 runHTTP);
//   - 任何错误都折进结果里而不是返回 error:连不上/TLS 失败同样是"一次测试的结论",
//     调用方不需要区分"执行失败"与"探测失败"(HTTP 路径的判定失败本来就在 Error 里)。
func ExecuteTest(ctx context.Context, monitorType string, check json.RawMessage) *protocol.ProbeTestResultPayload {
	if monitorType == checkconfig.TypeHTTP {
		run, err := runHTTP(ctx, check, true)
		if err != nil {
			return &protocol.ProbeTestResultPayload{OK: false, Error: err.Error(), Detail: &protocol.ProbeTestDetail{
				Target: targetOf(monitorType, check),
			}}
		}
		return &protocol.ProbeTestResultPayload{
			OK:         run.Result.OK,
			LatencyMs:  run.Result.LatencyMs,
			HTTPStatus: run.Result.HTTPStatus,
			Error:      run.Result.Error,
			Detail:     run.Detail,
		}
	}
	// 下载速度监控:测试同样真下载一遍(与轮次同一个 runDownload),并把测得的
	// 平均速度与字节数带回,弹窗里当场就能看到"这条链路下这个文件有多快"。
	if monitorType == checkconfig.TypeDownload {
		run, err := runDownload(ctx, check, true)
		if err != nil {
			return &protocol.ProbeTestResultPayload{OK: false, Error: err.Error(), Detail: &protocol.ProbeTestDetail{
				Target: targetOf(monitorType, check),
			}}
		}
		return &protocol.ProbeTestResultPayload{
			OK:         run.Result.OK,
			LatencyMs:  run.Result.LatencyMs,
			HTTPStatus: run.Result.HTTPStatus,
			Error:      run.Result.Error,
			SpeedKbps:  run.Result.SpeedKbps,
			Bytes:      run.Result.Bytes,
			Detail:     run.Detail,
		}
	}
	// ping / tcp 没有"响应内容"可言:能拿到的就是耗时与失败原因(连通性结论)。
	res := Execute(ctx, &protocol.ProbeTaskPayload{MonitorType: monitorType, Check: check})
	if res == nil {
		return &protocol.ProbeTestResultPayload{OK: false, Error: "探测执行器返回空结果"}
	}
	return &protocol.ProbeTestResultPayload{
		OK:         res.OK,
		LatencyMs:  res.LatencyMs,
		HTTPStatus: res.HTTPStatus,
		Error:      res.Error,
		Detail:     &protocol.ProbeTestDetail{Target: targetOf(monitorType, check)},
	}
}

// targetOf 从探测配置里取出"测的是什么"给页面展示:ping 是主机,tcp 是主机:端口。
// HTTP 的实际请求由 runHTTP 采集,不在这里拼。
func targetOf(monitorType string, check json.RawMessage) string {
	switch monitorType {
	case checkconfig.TypePing:
		var cfg checkconfig.Ping
		if json.Unmarshal(check, &cfg) == nil {
			return cfg.Host
		}
	case checkconfig.TypeTCP:
		var cfg checkconfig.TCP
		if json.Unmarshal(check, &cfg) == nil {
			return net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
		}
	}
	return ""
}

// ValidateTestType 校验测试请求里的监控类型:push 没有可探测的目标,直接拒绝,
// 免得下发一个节点侧根本没有注册的类型、回一句难懂的错误。
func ValidateTestType(monitorType string) error {
	switch monitorType {
	case checkconfig.TypeHTTP, checkconfig.TypePing, checkconfig.TypeTCP, checkconfig.TypeDownload:
		return nil
	case checkconfig.TypePush:
		return errors.New("外部上报监控没有可测试的目标(它由外部系统上报结果)")
	default:
		return fmt.Errorf("不支持的监控类型: %s", monitorType)
	}
}
