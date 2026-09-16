//go:build linux || darwin

package agentclient

import (
	"os"
	"syscall"
)

// selfUpgradeSupported 类 Unix 允许替换正在运行的可执行文件并原地换映像。
func selfUpgradeSupported() bool { return true }

// restartSelf 以同样的参数与环境变量把进程映像换成新二进制:PID 不变,
// systemd 的 Restart=always、cgroup 与资源限制都不受影响(无需依赖服务重启)。
func restartSelf(target string) error {
	return syscall.Exec(target, os.Args, os.Environ())
}
