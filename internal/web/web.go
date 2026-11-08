// Package web 提供内嵌的单页控制台。
//
// 为什么把前端嵌进二进制（go:embed）而不是单独部署一个前端：
//   - 这个项目的展示重点是后端链路，不是前端工程化。引入 Node 构建链
//     会让「clone 下来 go run 就能用」变成「先装 pnpm 再 build」。
//   - 同源部署省掉 CORS 与鉴权头，SSE 的 Cookie/Origin 问题一个都不存在。
//   - 代价是改前端要重新编译 Go 二进制 —— 对这个规模完全可以接受。
//
// 页面本身零依赖：没有框架、没有打包器、没有 CDN。
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"time"
)

//go:embed static
var assets embed.FS

// csp 是内容安全策略。
//
// 全部资源同源，且页面里没有内联 script/style —— 所以可以不留
// 'unsafe-inline'。这一条能挡掉「模型输出里被注入 <script>」这类问题：
// 即使前端哪里漏了转义，CSP 也不会执行注入的脚本。
const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; " +
	"script-src 'self'; connect-src 'self'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'none'"

// Handler 返回控制台的 HTTP 处理器。
//
// 路由只有两个：/ 给 index.html，/static/* 给静态资源。
// 其余路径返回 404 —— 不做 SPA 的 history fallback，因为这里只有一页。
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 不缓存 HTML：升级后用户刷新就能拿到新版本，
		// 而静态资源仍可长缓存。
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})

	files := http.FileServer(http.FS(sub))
	mux.Handle("/static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源带哈希吗？没有。所以 max-age 不敢给太长 ——
		// 改名/改内容后用户最多 1 小时内看到旧版本。
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})))

	return withSecurityHeaders(mux), nil
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
