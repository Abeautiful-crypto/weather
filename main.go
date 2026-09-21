// Command weather 启动一个天气预报服务：内嵌前端页面，并把 Open-Meteo
// 三个公开接口代理为同源的 /api/*，附带 TTL 缓存与单飞合并。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"weather/internal/api"
	"weather/internal/cache"
	"weather/internal/upstream"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	cacheTTL := flag.Duration("cache-ttl", 10*time.Minute, "上游响应缓存时长，0 表示不缓存")
	upstreamTimeout := flag.Duration("upstream-timeout", 8*time.Second, "单个上游请求超时")
	logLevel := flag.String("log-level", "info", "日志级别：debug/info/warn/error")
	flag.Parse()

	logger := newLogger(*logLevel)

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		logger.Error("内嵌静态资源不可用", "err", err)
		os.Exit(1)
	}

	handler := api.New(
		cache.New(*cacheTTL),
		upstream.NewDefault(*upstreamTimeout),
		logger,
	).Routes(static)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("服务启动",
			"addr", *addr,
			"cache_ttl", cacheTTL.String(),
			"upstream_timeout", upstreamTimeout.String(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("监听失败", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	stop()

	logger.Info("收到退出信号，正在关闭服务……", "shutdown_timeout", shutdownTimeout(*upstreamTimeout).String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout(*upstreamTimeout))
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭超时", "err", err)
	}
	logger.Info("服务已退出")
}

// shutdownTimeout 由 -upstream-timeout 推导，保证关停等待足以覆盖一次完整的
// 上游调用（否则会出现"请求被强杀、上游 goroutine 仍在跑"的窗口），下限 5s。
// 刻意不新增 flag：这样用户调大上游超时不会让关停行为静默变差。
func shutdownTimeout(upstream time.Duration) time.Duration {
	limit := upstream + 2*time.Second
	if limit < 5*time.Second {
		limit = 5 * time.Second
	}
	return limit
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
