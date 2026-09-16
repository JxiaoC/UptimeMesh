// Package webdist 通过 go:embed 携带前端(web/)构建产物。
// 本地开发:web/ 用 vite dev server(代理到 dashboard 8000);
// 发布:先在仓库根执行 web 构建,把 web/dist 拷入本目录 dist/ 再 go build。
package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// Assets 根为 dist 的只读文件系统。
var Assets fs.FS = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
