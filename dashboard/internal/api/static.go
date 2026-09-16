package api

import (
	"io/fs"

	"github.com/gogf/gf/v2/net/ghttp"

	"github.com/uptimemesh/dashboard/internal/webdist"
)

// registerStatic 挂载前端构建产物(go:embed,见 webdist 包)。
// /api 与 /ws 前缀交给接口路由;其余路径:命中文件则返回文件,
// 否则回退 index.html 以支持 SPA history 路由。
//
// 缓存策略:assets/ 下文件名带内容哈希,可长期强缓存;index.html 不缓存,
// 否则重新发布后浏览器仍引用旧哈希资源、页面停留在旧版本。
func (a *API) registerStatic(s *ghttp.Server) {
	s.BindHandler("/*", func(r *ghttp.Request) {
		p := r.URL.Path
		if p == "/api" || p == "/ws" ||
			len(p) >= 5 && p[:5] == "/api/" ||
			len(p) >= 4 && p[:4] == "/ws/" {
			r.Response.WriteStatus(404)
			return
		}
		if p != "/" {
			if data, err := fs.ReadFile(webdist.Assets, p[1:]); err == nil {
				r.Response.Header().Set("Content-Type", contentType(p))
				r.Response.Header().Set("Cache-Control", cacheControl(p))
				r.Response.Write(data)
				return
			}
		}
		index, err := fs.ReadFile(webdist.Assets, "index.html")
		if err != nil {
			r.Response.WriteStatus(404)
			return
		}
		r.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		r.Response.Header().Set("Cache-Control", "no-cache")
		r.Response.Write(index)
	})
}

// cacheControl assets/ 下的哈希资源可 immutable;其余(含 index.html)须回源校验。
func cacheControl(p string) string {
	if len(p) >= 8 && p[:8] == "/assets/" {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func contentType(p string) string {
	switch {
	case hasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case hasSuffix(p, ".js"):
		return "text/javascript; charset=utf-8"
	case hasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case hasSuffix(p, ".json"):
		return "application/json"
	case hasSuffix(p, ".svg"):
		return "image/svg+xml"
	case hasSuffix(p, ".png"):
		return "image/png"
	case hasSuffix(p, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
