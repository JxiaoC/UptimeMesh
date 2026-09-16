package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/uptimemesh/shared/checkconfig"
	"github.com/uptimemesh/shared/protocol"
)

// 下载速度监控(见 .scratch/download-speed-monitor/spec.md):把 URL 指向的文件完整下载
// 一遍(只计数、不落盘、不保留正文),用「总字节数 ÷ 总耗时」算出平均速度回传。
//
// 请求侧与 HTTP 探测共用同一套语义:方法/请求头/请求体、Host 头选源(IP 直连 + 虚拟主机)、
// 跟随重定向(≤ maxRedirects 跳)、忽略证书校验。判定侧不同:节点只负责测速,
// 是否达标由 Dashboard 按本轮平均速度与监控的下载速度阈值比较。
func init() { Register(checkconfig.TypeDownload, executeDownload) }

// downloadBufSize 是下载时的读缓冲(64KB)。速度只由总字节数与总耗时决定,缓冲大小
// 只影响系统调用次数,故取一个能打满吞吐又不占内存的固定值。
const downloadBufSize = 64 << 10

func executeDownload(ctx context.Context, raw json.RawMessage) (*protocol.ProbeResultPayload, error) {
	run, err := runDownload(ctx, raw, false)
	if err != nil {
		return nil, err
	}
	return run.Result, nil
}

// downloadRun 一次下载测速的产物:Result 是给轮次用的判定结果;Detail 只在「测试」路径采集。
type downloadRun struct {
	Result *protocol.ProbeResultPayload
	Detail *protocol.ProbeTestDetail
}

func runDownload(ctx context.Context, raw json.RawMessage, capture bool) (*downloadRun, error) {
	var cfg checkconfig.Download
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析下载探测配置失败: %w", err)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// 超时覆盖"发请求 + 读完整响应体":下载速度监控的超时就是本次下载的时间预算。
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := strings.ToUpper(cfg.Method)
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, cfg.URL, body)
	if err != nil {
		return nil, err
	}
	// Host 头不能走 Header(见 http.go 的同款说明):它决定虚拟主机选源,
	// 且 HTTPS 的 SNI 与证书校验必须按这个主机名走。
	hostHeader := ""
	for k, v := range cfg.Headers {
		if strings.EqualFold(k, "Host") {
			hostHeader = v
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	// Host 头选源与 IP 协议族进连接池的缓存键(httpclient.go):与 HTTP 探测同一套
	// 复用口径。下载监控独占的串行下发(scheduler.dispatchSerial)保证同一时刻一个
	// 目标只有一个节点在下载,复用不会把多轮测速挤进同一条连接互相压带宽。
	vhostName, connectAddr := "", ""
	if vhost, dialAddr := vhostTarget(req.URL, hostHeader); vhost != "" {
		req.URL.Host = vhost
		vhostName, connectAddr = vhost, dialAddr
	}

	transport, cached := transportFor(transportParams{
		insecure: cfg.AllowInsecureTLS,
		network:  checkconfig.Network(cfg.IPVersion, "tcp"),
		vhost:    vhostName,
		dialAddr: connectAddr,
	})
	// 缓存已满时拿到的是一次性实例:用完必须收掉空闲连接,否则退化回连接堆积。
	if !cached {
		defer transport.CloseIdleConnections()
	}

	var redirects []string
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("重定向超过 %d 次", maxRedirects)
			}
			redirects = append(redirects, r.URL.String())
			return nil
		},
	}

	var detail *protocol.ProbeTestDetail
	if capture {
		detail = &protocol.ProbeTestDetail{
			Method: method, URL: req.URL.String(), Headers: cfg.Headers, Body: cfg.Body,
		}
		if connectAddr != "" {
			detail.Target = connectAddr
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if detail != nil {
		detail.Status = resp.StatusCode
		detail.StatusText = resp.Status
		detail.RespHeaders = headerMap(resp.Header)
		detail.FinalURL = resp.Request.URL.String()
		detail.Redirects = redirects
	}
	// 非 2xx 不读正文:下载目标给的是错误页/鉴权页,读了也只是浪费带宽。
	if !httpStatusOK(resp.StatusCode, nil) {
		if detail != nil {
			detail.BodyNote = fmt.Sprintf("状态码 %d,未读取正文", resp.StatusCode)
		}
		return &downloadRun{
			Result: &protocol.ProbeResultPayload{
				OK: false, LatencyMs: msSince(start),
				HTTPStatus:    resp.StatusCode,
				Error:         fmt.Sprintf("状态码 %d 不符合期望(文件下载须为 2xx)", resp.StatusCode),
				StartedAtUnix: start.Unix(), FinishedAtUnix: time.Now().Unix(),
			},
			Detail: detail,
		}, nil
	}

	total, readErr := drainBody(resp.Body)
	elapsed := time.Since(start)
	speedKbps := speedOf(total, elapsed)
	ok := readErr == nil
	errMsg := ""
	if readErr != nil {
		// 下载中断(连接被掐断/超时):已读到的字节数照实回报,但速度记 0 ——
		// 半截速度既不能代表这条链路的带宽,也不能进轮次平均。
		speedKbps = 0
		errMsg = downloadFailReason(readErr, timeout)
	}
	if detail != nil {
		detail.BodyBytes = total
		if readErr != nil {
			detail.BodyNote = fmt.Sprintf("下载未完成(%d 字节后中断)", total)
		} else {
			detail.BodyNote = fmt.Sprintf("文件已完整下载(%d 字节),不回传正文", total)
		}
	}
	return &downloadRun{
		Result: &protocol.ProbeResultPayload{
			OK: ok, LatencyMs: milliseconds(elapsed), HTTPStatus: resp.StatusCode,
			Error: errMsg, SpeedKbps: speedKbps, Bytes: total,
			StartedAtUnix: start.Unix(), FinishedAtUnix: time.Now().Unix(),
		},
		Detail: detail,
	}, nil
}

// drainBody 读完响应体并返回字节数(丢弃内容,只做统计)。
func drainBody(r io.Reader) (int64, error) {
	buf := make([]byte, downloadBufSize)
	var total int64
	for {
		n, err := r.Read(buf)
		total += int64(n)
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// speedOf 平均速度(KB/s):总字节数 ÷ 总秒数。耗时过短(亚毫秒)时按 1ms 兜底,
// 避免除出天文数字。
func speedOf(bytes int64, elapsed time.Duration) float64 {
	if bytes <= 0 {
		return 0
	}
	sec := elapsed.Seconds()
	if sec <= 0 {
		sec = 0.001
	}
	return float64(bytes) / 1024 / sec
}

// downloadFailReason 把下载中断的原因翻译成可读文案(超时最常见)。
func downloadFailReason(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("下载超时(超过 %s)", timeout)
	}
	return "下载中断: " + err.Error()
}

func milliseconds(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

// msSince 与 runHTTP 的计时口径一致(纳秒精度转毫秒)。
func msSince(start time.Time) float64 { return milliseconds(time.Since(start)) }
