// Package geoiptest 为地域解析测试提供一份内置的本地 MaxMind DB(.mmdb):
// 8.8.8.8→US、1.1.1.1→AU、9.9.9.9→JP(记录里只有 registered_country)。
// 生成脚本与记录说明见同目录 gen_mmdb.go。
package geoiptest

import (
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/uptimemesh/dashboard/internal/geoip"
)

//go:embed testdata/geoip-test.mmdb
var geoDB []byte

// Open 把内置测试库落到临时文件并打开,测试结束自动关闭。
// 落盘而非直接内存读取,是为了走与生产完全相同的 geoip.Open 路径。
func Open(t testing.TB) *geoip.Resolver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "geoip-test.mmdb")
	if err := os.WriteFile(path, geoDB, 0o600); err != nil {
		t.Fatalf("写入测试地域库失败: %v", err)
	}
	r, err := geoip.Open(path)
	if err != nil {
		t.Fatalf("打开测试地域库失败: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
