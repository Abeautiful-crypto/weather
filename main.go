// Command weather 启动一个天气预报服务：内嵌前端页面，并把 Open-Meteo
// 三个公开接口代理为同源的 /api/*，附带 TTL 缓存与单飞合并。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// 内嵌 IANA 时区数据库。api 层用 time.LoadLocation 校验前端传来的 tz；若运行环境
	// 缺少 zoneinfo（例如目标机器上没有 Go 的 GOROOT），校验会全部失败并静默回落到
	// auto，使"时区与坐标解耦"这个前提失效。内嵌后行为与运行环境无关（仅标准库）。
	_ "time/tzdata"

	"weather/internal/amap"
	"weather/internal/api"
	"weather/internal/cache"
	"weather/internal/qweather"
	"weather/internal/upstream"
)

//go:embed web
var webFS embed.FS

// 凭据类的环境变量名。用环境变量而不是 flag：Key 属于机密，出现在命令行里会进入
// shell 历史与进程列表。三家的凭据都可缺省，缺省时城市搜索按优先级继续回落。
const (
	// envAMapKey 是高德 Web 服务 Key。高德的鉴权只能走 URL 查询参数（没有可用的
	// 请求头鉴权），因此 amap 包内部对错误信息做了脱敏，避免 Key 随日志落地。
	envAMapKey = "AMAP_KEY"
	// envQWeatherHost 与 envQWeatherKey 成对出现：和风必须使用账号专属 API Host。
	envQWeatherHost = "QWEATHER_HOST"
	envQWeatherKey  = "QWEATHER_KEY"
)

func main() {
	// 默认只监听回环：本服务无鉴权、无限流，绑全网卡会让同网段（含 WSL/容器所在的
	// 虚拟网卡）任何进程都能无偿使用这个上游代理。需要局域网访问时显式传 -addr。
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址（默认仅本机可访问，如要局域网访问填 :8080）")
	cacheTTL := flag.Duration("cache-ttl", 10*time.Minute, "上游响应缓存时长，0 表示不缓存")
	upstreamTimeout := flag.Duration("upstream-timeout", 8*time.Second, "单个上游请求超时")
	logLevel := flag.String("log-level", "info", "日志级别：debug/info/warn/error")
	webDir := flag.String("web-dir", "", "改从磁盘目录读取前端（开发用：改完刷新即可）；留空则用内嵌资源")
	flag.Parse()

	logger := newLogger(*logLevel)

	static, err := resolveStatic(*webDir)
	if err != nil {
		logger.Error("静态资源不可用", "err", err, "web_dir", *webDir)
		os.Exit(1)
	}

	// 城市搜索按「高德 → 和风 → Open-Meteo」回落；两家凭据都可缺省，
	// 保证"不配置也能一条命令跑起来"。定位取名（/api/regeo）依赖高德。
	amClient := newAMapClient(logger, *upstreamTimeout)
	qwClient := newQWeatherClient(logger, *upstreamTimeout)

	handler := api.New(
		cache.New(*cacheTTL),
		upstream.NewDefault(*upstreamTimeout),
		amClient,
		qwClient,
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
		webSource := "embed"
		if strings.TrimSpace(*webDir) != "" {
			webSource = *webDir
		}
		logger.Info("服务启动",
			"addr", *addr,
			"cache_ttl", cacheTTL.String(),
			"upstream_timeout", upstreamTimeout.String(),
			"web", webSource,
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

// resolveStatic 决定前端资源的来源：-web-dir 非空时从磁盘读取（开发用：改一行
// HTML 刷新即可看到效果），否则用内嵌资源（发布形态：单个可执行文件即可运行）。
//
// 两条路径最终都交给 api 层同一个 fs.FS 抽象（Routes 的参数类型），所以"禁用目录
// 列表""静态 404 形态"等行为对二者完全一致，不需要第二套静态处理器。
func resolveStatic(webDir string) (fs.FS, error) {
	dir := strings.TrimSpace(webDir)
	if dir == "" {
		return fs.Sub(webFS, "web")
	}
	// 提前校验入口文件存在：否则第一次访问才会暴露成 404，排查成本更高。
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return nil, fmt.Errorf("在 %s 下找不到 index.html：%w", dir, err)
	}
	return os.DirFS(dir), nil
}

// newAMapClient 按环境变量创建高德客户端；未配置 Key 时返回 nil，城市搜索退到
// 下一路，/api/regeo 返回 503。任何情况下都不打印 API Key。
func newAMapClient(logger *slog.Logger, timeout time.Duration) *amap.Client {
	key := strings.TrimSpace(os.Getenv(envAMapKey))
	if key == "" {
		logger.Info("城市搜索数据源：高德未配置（缺少 "+envAMapKey+"）",
			"提示", "配置 "+envAMapKey+" 可启用高德地理编码与定位取名")
		return nil
	}

	client, err := amap.New(amap.Config{Key: key, Timeout: timeout})
	if err != nil {
		logger.Warn("高德客户端初始化失败，城市搜索将回落", "err", err)
		return nil
	}
	logger.Info("城市搜索数据源：高德地理编码", "定位取名", "已启用")
	return client
}

// newQWeatherClient 按环境变量创建和风客户端；凭据缺失或不完整时返回 nil，
// 由 api 层回落到 Open-Meteo 的城市搜索。任何情况下都不打印 API Key。
func newQWeatherClient(logger *slog.Logger, timeout time.Duration) *qweather.Client {
	host := strings.TrimSpace(os.Getenv(envQWeatherHost))
	key := strings.TrimSpace(os.Getenv(envQWeatherKey))

	switch {
	case host == "" && key == "":
		logger.Info("城市搜索数据源：Open-Meteo",
			"提示", "配置 "+envQWeatherHost+" 与 "+envQWeatherKey+" 可启用和风天气中文地名搜索")
		return nil
	case host == "" || key == "":
		missing := envQWeatherHost
		if host != "" {
			missing = envQWeatherKey
		}
		logger.Warn("和风凭据不完整，城市搜索已回落 Open-Meteo", "缺少环境变量", missing)
		return nil
	}

	client, err := qweather.New(qweather.Config{Host: host, APIKey: key, Timeout: timeout})
	if err != nil {
		logger.Warn("和风客户端初始化失败，城市搜索已回落 Open-Meteo", "err", err)
		return nil
	}
	logger.Info("城市搜索数据源：和风天气 GeoAPI", "host", host)
	return client
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
