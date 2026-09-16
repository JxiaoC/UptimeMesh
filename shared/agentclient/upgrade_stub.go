//go:build !linux && !darwin

package agentclient

import "errors"

// selfUpgradeSupported Windows 上运行中的可执行文件被系统占用,无法就地替换,
// 因此不做自升级(节点页也不会对 Windows 节点提供升级按钮——分发目录只有 Linux 包)。
func selfUpgradeSupported() bool { return false }

// restartSelf 非类 Unix 平台没有 exec 自身的能力。
func restartSelf(string) error {
	return errors.New("当前平台不支持原地重启自身")
}
