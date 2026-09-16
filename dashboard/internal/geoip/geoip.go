// Package geoip 用本地 MaxMind DB(.mmdb,GeoLite2/GeoIP2 的 City 或 Country
// 版格式)把 IP 解析为 ISO 3166-1 alpha-2 国家/地区码(大写,如 "CN"/"HK")。
// 查询经开源 oschwald/maxminddb-golang 在进程内完成(内存映射/加载),
// 零网络请求,离线可用。
//
// 数据库来源:MaxMind GeoLite2 免费地理库(注册免费账号获取下载许可密钥);
// 通过 GEOIP_DB_PATH 指向 .mmdb 文件启用。未配置或打开失败时调用方持 nil
// Resolver——节点地域仅回显手动值/已缓存值,自动解析停用。
package geoip

import (
	"net"
	"strings"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang"
)

// Resolver 是一个只读的本地地域库句柄;并发安全(Reader 支持并行读)。
// 零值不可用,请用 Open。
type Resolver struct {
	db *maxminddb.Reader
}

// record 同时兼容 City/Country 两版结构:优先 country,缺失时退到
// registered_country(部分 ASN/CDN 记录只有后者)。
type record struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Open 打开 dbPath 指向的 .mmdb;空路径表示不启用,返回 (nil, nil)。
func Open(dbPath string) (*Resolver, error) {
	if dbPath == "" {
		return nil, nil
	}
	db, err := maxminddb.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Resolver{db: db}, nil
}

// Close 释放底层映射;nil 接收者安全。
func (r *Resolver) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Lookup 返回 ip 的国家/地区码(大写);nil 接收者、私有/回环/链路本地/非法
// 地址、库中查无此 IP 均返回 ""。
//
// 内网地址在查库前就被挡掉:地域库对它们没有记录,而展示层要用 ScopeOf 区分
// 「本机 / 局域网 / 链路本地」,不该把「查不到国家」和「是内网」混为一谈。
func (r *Resolver) Lookup(ip string) string {
	if r == nil {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || scopeOf(parsed) != ScopePublic || !parsed.IsGlobalUnicast() {
		return ""
	}
	var rec record
	if err := r.db.Lookup(parsed, &rec); err != nil {
		return ""
	}
	code := rec.Country.ISOCode
	if code == "" {
		code = rec.RegisteredCountry.ISOCode
	}
	return strings.ToUpper(strings.TrimSpace(code))
}

// BuildDate 返回地域库的构建日期(yyyy-mm-dd,UTC);nil 接收者或读不到元数据
// 时返回 ""。
//
// GeoLite2 是按周更新的,库本身不会报错、只会慢慢过时;启动日志里带上这个日期,
// 运维一眼就能看出「解析不出地域」到底是没配库,还是库太旧。
func (r *Resolver) BuildDate() string {
	if r == nil || r.db == nil {
		return ""
	}
	epoch := r.db.Metadata.BuildEpoch
	if epoch == 0 {
		return ""
	}
	return time.Unix(int64(epoch), 0).UTC().Format("2006-01-02")
}
