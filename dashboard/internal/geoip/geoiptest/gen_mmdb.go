//go:build ignore

// gen_mmdb 生成 geoiptest/testdata/geoip-test.mmdb:一份只含几条测试记录的小型
// MaxMind DB(GeoIP2-City 结构),供 geoip 包与 api 包的地域解析测试使用。
//
// 重新生成(临时引入写入库,生成后 tidy 会自动摘掉该依赖):
//
//	cd dashboard
//	go get github.com/maxmind/mmdbwriter@v1.2.0
//	go run internal/geoip/geoiptest/gen_mmdb.go
//	go mod tidy
//
// 记录:8.8.8.8→us(小写,验证归一化)、1.1.1.1→au、9.9.9.9→jp
// (只有 registered_country,验证回落逻辑)。IPVersion 取 6,与官方测试库一致,
// 使 IPv4 查询走 v4-in-v6 映射路径。
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const outPath = "internal/geoip/geoiptest/testdata/geoip-test.mmdb"

func main() {
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "GeoIP2-City",
		RecordSize:   24,
		IPVersion:    6,
	})
	if err != nil {
		panic(err)
	}

	country := func(iso string) mmdbtype.DataType {
		return mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(iso)}}
	}
	insert := func(cidr string, value mmdbtype.DataType) {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		if err := w.Insert(network, value); err != nil {
			panic(err)
		}
	}
	insert("8.8.8.8/32", country("us"))
	insert("1.1.1.1/32", country("au"))
	insert("9.9.9.9/32", mmdbtype.Map{
		"registered_country": mmdbtype.Map{"iso_code": mmdbtype.String("jp")},
	})

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		panic(err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := w.WriteTo(f); err != nil {
		panic(err)
	}
	fmt.Printf("已生成 %s\n", outPath)
}
