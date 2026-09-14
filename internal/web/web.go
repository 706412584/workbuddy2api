// Package web 提供管理面板的静态资源，并在编译期把它们嵌进 exe。
//
// 资源由 vite 构建产出到本包的 dist/（见 frontend/vite.config.ts 的 build.outDir），
// 于是 wb2api.exe 一个文件就能同时提供 API 与面板，不再需要另起一个 dev server。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回面板的静态文件服务。
//
// 不做单页应用的深链接回落：前端是纯标签页状态（App.tsx 里的 useState），没有路由，
// 也就没有「直接粘某个子路径」这回事。反过来，回落会把打错的 API 路径喂成
// index.html —— 浏览器把 HTML 当脚本执行，报出的错与真实原因毫不相干。
// 将来若引入路由，这里再补 fallback。
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// 只有 embed 结构被改坏才会走到（构建期问题），fail fast 好过静默 404
		panic("web: embedded dist/ missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, sub)
			return
		}
		// 目录一律 404：FileServer 对没有 index.html 的目录会给出文件列表，
		// 而这里没有任何理由把构建产物的目录结构透出去。
		fi, err := fs.Stat(sub, p)
		if err != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		setCache(w, p)
		files.ServeHTTP(w, r)
	})
}

// serveIndex 直接吐首页，不经 FileServer —— 这里要显式控制缓存头。
func serveIndex(w http.ResponseWriter, sub fs.FS) {
	raw, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "index.html missing from embedded assets", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

// setCache 给带内容哈希的构建产物长缓存，其余每次校验。
// index.html 必须每次校验：它引用的是带哈希的资源名，缓存住就永远发现不了新版本。
func setCache(w http.ResponseWriter, p string) {
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
