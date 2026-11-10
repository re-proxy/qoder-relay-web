package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

/*
内置反代监听：为每个档位单独开一个只服务该档位的端口，
方便把不同客户端分别指向 http://host:7891/v1 与 http://host:7892/v1。
主监听端口本身按请求头/模型前缀分派档位。
*/

// proxyServer 是一个正在监听的档位反代。
type proxyServer struct {
	edition string
	addr    string
	server  *http.Server
}

// proxyRunning 报告是否有反代在监听。
func (a *App) proxyRunning() bool {
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	return len(a.proxyStop) > 0
}

// syncProxy 按当前配置对齐反代监听状态。
func (a *App) syncProxy() {
	cfg := a.store.ProxyConfig()
	want := map[string]string{}
	if cfg.Enabled {
		want["domestic"] = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.DomesticPort))
		want["international"] = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.InternationalPort))
	}

	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()

	for edition, stop := range a.proxyStop {
		if _, keep := want[edition]; !keep {
			stop()
			delete(a.proxyStop, edition)
		}
	}
	for edition, addr := range want {
		if _, ok := a.proxyStop[edition]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(a.root)
		handler := a.editionHandler(edition)
		server := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 20 * time.Second,
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			cancel()
			logf("反代 %s 监听 %s 失败: %v", edition, addr, err)
			continue
		}
		logf("反代 %s 已启动 → http://%s/v1", edition, addr)
		go func(edition, addr string, server *http.Server, listener net.Listener) {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("反代 %s 停止: %v", edition, err)
			}
		}(edition, addr, server, listener)
		a.proxyStop[edition] = func() {
			cancel()
			shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = server.Shutdown(shutdownCtx)
		}
		_ = ctx
	}
}

// editionHandler 返回强制某个档位的网关处理器。
func (a *App) editionHandler(edition string) http.Handler {
	forced := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Qoder-Edition", edition)
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/v1/models", forced(a.handleModels))
	mux.HandleFunc("/v1/chat/completions", forced(a.handleChatCompletions))
	mux.HandleFunc("/v1/responses", forced(a.handleResponses))
	mux.HandleFunc("/v1/messages", forced(a.handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", forced(a.handleCountTokens))
	return mux
}

/* ---------------- 公共路由 ---------------- */

// handleHealthz 是健康检查。
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	counts := a.store.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": a.cfg.Version,
		"counts":  counts,
	})
}

// cors 只对 /v1 前缀加宽松 CORS，便于浏览器直连。
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, x-api-key, anthropic-version, anthropic-beta, x-request-id, x-qoder-edition")
	w.Header().Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// serve 是主监听端口的总路由。
func (a *App) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	path := r.URL.Path

	if strings.HasPrefix(path, "/v1/") {
		w.Header().Set("Cache-Control", "no-store")
		if cors(w, r) {
			return
		}
		switch path {
		case "/v1/models":
			a.handleModels(w, r)
		case "/v1/chat/completions":
			a.handleChatCompletions(w, r)
		case "/v1/responses":
			a.handleResponses(w, r)
		case "/v1/messages":
			a.handleMessages(w, r)
		case "/v1/messages/count_tokens":
			a.handleCountTokens(w, r)
		default:
			openAIError(w, http.StatusNotFound, "not_found", "接口不存在: "+path)
		}
		return
	}

	switch path {
	case "/healthz":
		a.handleHealthz(w, r)
		return
	case "/health":
		a.handleHealthz(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/") {
		w.Header().Set("Cache-Control", "no-store")
		a.admin(w, r)
		return
	}

	// 控制台静态资源与页面路由。
	if a.serveConsole(w, r) {
		return
	}
	http.NotFound(w, r)
}

// logf 输出带时间戳的日志。
func logf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
