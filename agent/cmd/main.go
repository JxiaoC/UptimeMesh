package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/uptimemesh/shared/agentclient"
)

func main() {
	server := flag.String("server", "ws://127.0.0.1:8000/ws/agent", "Dashboard WebSocket 地址")
	key := flag.String("key", "", "全局接入密钥(首次接入必填)")
	name := flag.String("name", hostname(), "节点自报名称")
	dir := flag.String("cred-dir", ".uptimemesh-agent", "凭据持久化目录")
	flag.Parse()

	if !agentclient.WsURLValid(*server) {
		fmt.Fprintf(os.Stderr, "非法的 --server 地址: %s\n", *server)
		os.Exit(2)
	}

	logf := func(format string, args ...any) {
		fmt.Printf("[%s] "+format+"\n", append([]any{timeNow()}, args...)...)
	}

	c, err := agentclient.New(agentclient.Options{
		ServerURL: *server, EnrollmentKey: *key,
		Name: *name, CredentialDir: *dir,
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logf("UptimeMesh Agent v%s 启动,name=%s server=%s", agentclient.Version, *name, *server)
	if err := c.Run(ctx, logf); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "运行失败:", err)
		os.Exit(1)
	}
	logf("已退出")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "agent"
	}
	return h
}
