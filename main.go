package main

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// 控制台页面路由（前端用 pushState，服务端统一返回同一份 index.html）。
var consolePages = map[string]bool{
	"/":         true,
	"/accounts": true,
	"/models":   true,
	"/keys":     true,
	"/usage":    true,
	"/settings": true,
}

const consoleCSP = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// serveConsole 服务内嵌前端资源。
func (a *App) serveConsole(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch path {
	case "/app.js":
		a.serveAsset(w, r, "web/app.js", "text/javascript; charset=utf-8")
		return true
	case "/style.css":
		a.serveAsset(w, r, "web/style.css", "text/css; charset=utf-8")
		return true
	case "/favicon.svg":
		a.serveAsset(w, r, "web/favicon.svg", "image/svg+xml")
		return true
	}
	if !consolePages[path] {
		return false
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "控制台资源缺失", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", consoleCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return true
}

func (a *App) serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	data, err := webFS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func loadConfig() (Config, error) {
	cfg := Config{
		Addr:           env("RELAY_ADDR", ":8317"),
		DataDir:        env("RELAY_DATA_DIR", "/data"),
		AdminPassword:  strings.TrimSpace(os.Getenv("RELAY_ADMIN_PASSWORD")),
		MasterKey:      strings.TrimSpace(os.Getenv("RELAY_MASTER_KEY")),
		DefaultEdition: strings.ToLower(env("RELAY_DEFAULT_EDITION", "domestic")),
		CheckinEnabled: envBool("RELAY_CHECKIN_ENABLED", true),
		CheckinHour:    envInt("RELAY_CHECKIN_HOUR", 8),
		CheckinMinute:  envInt("RELAY_CHECKIN_MINUTE", 30),
		SecureCookie:   envBool("RELAY_SECURE_COOKIE", false),
		Version:        "1.0.0",
	}
	if cfg.MasterKey == "" {
		return cfg, errors.New("RELAY_MASTER_KEY 未设置：请用 `qoder-relay-web keygen` 生成后填入")
	}
	if cfg.DefaultEdition != "international" {
		cfg.DefaultEdition = "domestic"
	}
	if cfg.CheckinHour < 0 || cfg.CheckinHour > 23 {
		cfg.CheckinHour = 8
	}
	if cfg.CheckinMinute < 0 || cfg.CheckinMinute > 59 {
		cfg.CheckinMinute = 30
	}
	return cfg, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		raw := make([]byte, 32)
		if _, err := randRead(raw); err != nil {
			fmt.Fprintln(os.Stderr, "生成密钥失败:", err)
			os.Exit(1)
		}
		fmt.Println(base64.StdEncoding.EncodeToString(raw))
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	app.startCheckinScheduler(app.root)
	app.syncProxy()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           http.HandlerFunc(app.serve),
		ReadHeaderTimeout: 20 * time.Second,
	}

	go func() {
		logf("qoder-relay-web %s 启动于 %s（数据目录 %s，默认档位 %s）", cfg.Version, cfg.Addr, cfg.DataDir, cfg.DefaultEdition)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "监听失败:", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logf("收到退出信号，正在关闭…")
	app.cancel()
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = server.Shutdown(ctx)
	_ = app.store.Save()
}
