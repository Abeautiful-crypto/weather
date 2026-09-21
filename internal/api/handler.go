// Package api 提供 HTTP 路由：三个代理接口、健康检查与内嵌前端静态资源托管。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"weather/internal/cache"
	"weather/internal/qweather"
	"weather/internal/upstream"
)

// maxQueryRunes 限制城市关键词长度，避免超长参数打到上游。
const maxQueryRunes = 64

// Server 持有依赖，便于在测试中替换。
type Server struct {
	cache  *cache.Cache
	up     *upstream.Client
	qw     *qweather.Client // 为 nil 表示未启用和风，城市搜索回落到 Open-Meteo
	logger *slog.Logger
}

// New 创建 Server。qw 可为 nil（未配置和风凭据时城市搜索走 Open-Meteo 的候选降级路径）。
func New(c *cache.Cache, up *upstream.Client, qw *qweather.Client, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cache: c, up: up, qw: qw, logger: logger}
}

// Routes 注册全部路由。static 为内嵌的前端静态资源文件系统（根目录下应有 index.html）。
func (s *Server) Routes(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/geocode", s.handleGeocode)
	mux.HandleFunc("GET /api/weather", s.handleWeather)
	mux.HandleFunc("GET /api/air", s.handleAir)
	// 未实现的 /api/* 一律返回 JSON 404，避免落到静态资源的首页兜底上。
	mux.HandleFunc("/api/", s.handleAPINotFound)
	if static != nil {
		mux.Handle("/", staticHandler(static))
	}
	return s.withLogging(mux)
}

// staticHandler 托管内嵌前端，并关掉 http.FileServerFS 的目录列表能力。
//
// 目录列表今天无害（web/ 下只有 index.html），但前端会持续演进：一旦出现
// web/assets/ 这类子目录，`GET /assets/` 就会直接吐出文件清单。这里把以 "/" 结尾的
// 路径（根路径除外）一律判为 404，使目录不可列举。
//
// 注意：静态 404 保持 http.NotFound 的 text/plain 形态，与 /api/* 的 JSON 404 有意
// 不同——静态路径的访问者是浏览器，JSON 原文比 "404 page not found" 更难读，而静态
// 路径不存在 API 消费者。这是刻意的不一致，不是漏改。
func staticHandler(static fs.FS) http.Handler {
	fileServer := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

/* ------------------------------------------------------------------ handlers */

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("接口不存在：%s", r.URL.Path))
}

// handleGeocode 不复用 proxy：它需要先做候选降级与重排序（见 geocode.go），
// 因此这里单独走一遍流程，但复用同一套错误与响应辅助函数。
func (s *Server) handleGeocode(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "缺少参数 q")
		return
	}
	if len([]rune(q)) > maxQueryRunes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("参数 q 过长（最多 %d 个字符）", maxQueryRunes))
		return
	}

	ctx := context.WithoutCancel(r.Context())
	var key string
	if s.qw != nil {
		// 和风：中文行政区覆盖更完整（宿迁这类在 GeoNames 中文索引里缺失的地名也能搜到）
		w.Header().Set("X-Geocode-Source", "qweather")
		key = "geocode:qweather:" + strings.ToLower(q)
	} else {
		// 回落路径：候选词由纯函数推导，因此缓存命中时也能如实告知尝试了哪些词
		w.Header().Set("X-Geocode-Source", "open-meteo")
		w.Header().Set("X-Geocode-Candidates", strings.Join(geocodeCandidates(q), ","))
		key = "geocode:open-meteo:" + strings.ToLower(q)
	}

	data, err, hit := s.cache.GetOrLoad(key, func() ([]byte, bool, error) {
		var (
			body      []byte
			searchErr error
		)
		if s.qw != nil {
			body, searchErr = s.searchPlacesQWeather(ctx, q)
		} else {
			body, searchErr = s.searchPlaces(ctx, q)
		}
		if searchErr != nil {
			return nil, false, searchErr
		}
		return body, json.Valid(body), nil
	})
	if err != nil {
		s.writeProxyError(w, "geocode", err)
		return
	}
	s.writeCachedJSON(w, "geocode", key, data, hit)
}

func (s *Server) handleWeather(w http.ResponseWriter, r *http.Request) {
	lat, lon, errMsg := parseCoord(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	params := url.Values{}
	params.Set("latitude", formatCoord(lat))
	params.Set("longitude", formatCoord(lon))
	params.Set("current", "temperature_2m,relative_humidity_2m,apparent_temperature,is_day,precipitation,weather_code,wind_speed_10m")
	params.Set("hourly", "temperature_2m,precipitation_probability,weather_code")
	params.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,uv_index_max")
	tz := parseTZ(r)
	params.Set("timezone", tz)
	params.Set("forecast_days", "7")

	s.proxy(w, r, "weather", coordKey("weather", lat, lon, tz), s.up.Endpoints().Forecast, params)
}

func (s *Server) handleAir(w http.ResponseWriter, r *http.Request) {
	lat, lon, errMsg := parseCoord(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	params := url.Values{}
	params.Set("latitude", formatCoord(lat))
	params.Set("longitude", formatCoord(lon))
	tz := parseTZ(r)
	params.Set("current", "pm2_5,pm10,us_aqi")
	params.Set("timezone", tz)

	s.proxy(w, r, "air", coordKey("air", lat, lon, tz), s.up.Endpoints().Air, params)
}

/* ------------------------------------------------------------------ helpers */

// proxy 走缓存 + 单飞的统一代理流程。
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, name, key, endpoint string, params url.Values) {
	data, err, hit := s.cache.GetOrLoad(key, func() ([]byte, bool, error) {
		// 上游调用刻意脱离客户端 context：单飞会把同一 key 的并发请求合并到这一次
		// 调用上，若让它跟随 leader 的 r.Context()，任何客户端断线都会连坐全部等待者
		// （一起收到 408）。连接不泄漏由 upstream 侧的 defer res.Body.Close() 与
		// 上游超时保证，并不依赖客户端取消；断线客户端反而会替同城用户预热缓存。
		loadCtx := context.WithoutCancel(r.Context())
		body, fetchErr := s.up.FetchWithQuery(loadCtx, endpoint, params)
		if fetchErr != nil {
			return nil, false, fetchErr
		}
		// 上游契约就是 JSON。返回非 JSON 说明上游异常（被网关插入了错误页之类），
		// 按故障处理并映射为 502，而不是把无法解析的原始体透传给前端——那样前端
		// 只会在 res.json() 上抛出一个英文的解析错误，用户看到的是堆栈式文案。
		// 注意：这与"字段缺失"是两回事，字段缺失仍由前端的"暂无数据"逻辑兜底。
		if !json.Valid(body) {
			return nil, false, &upstream.Error{
				Status: http.StatusBadGateway,
				Msg:    "上游返回了无法解析的数据，请稍后重试",
			}
		}
		return body, true, nil
	})

	if err != nil {
		s.writeProxyError(w, name, err)
		return
	}
	s.writeCachedJSON(w, name, key, data, hit)
}

// writeProxyError 把加载阶段的错误转换成对用户的响应。
func (s *Server) writeProxyError(w http.ResponseWriter, name string, err error) {
	var upErr *upstream.Error
	if errors.As(err, &upErr) {
		s.logger.Warn("代理失败", "api", name, "status", upErr.Status, "err", upErr)
		writeError(w, upErr.Status, upErr.Msg)
		return
	}
	var qwErr *qweather.Error
	if errors.As(err, &qwErr) {
		s.logger.Warn("和风调用失败", "api", name, "status", qwErr.Status, "err", qwErr)
		writeError(w, qwErr.Status, qwErr.Msg)
		return
	}
	s.logger.Error("代理异常", "api", name, "err", err)
	writeError(w, http.StatusInternalServerError, "服务内部错误")
}

// writeCachedJSON 写回数据，并带上 X-Cache 便于验证命中行为。
func (s *Server) writeCachedJSON(w http.ResponseWriter, name, key string, data []byte, hit bool) {
	if hit {
		s.logger.Info("cache hit", "api", name, "key", key)
		w.Header().Set("X-Cache", "HIT")
	} else {
		s.logger.Info("cache miss", "api", name, "key", key)
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// parseCoord 解析并校验 lat/lon 查询参数，返回中文错误信息。
func parseCoord(r *http.Request) (lat, lon float64, errMsg string) {
	rawLat := strings.TrimSpace(r.URL.Query().Get("lat"))
	rawLon := strings.TrimSpace(r.URL.Query().Get("lon"))
	if rawLat == "" || rawLon == "" {
		return 0, 0, "缺少参数 lat 或 lon"
	}

	var err error
	if lat, err = strconv.ParseFloat(rawLat, 64); err != nil {
		return 0, 0, "参数 lat 必须是数字"
	}
	if lon, err = strconv.ParseFloat(rawLon, 64); err != nil {
		return 0, 0, "参数 lon 必须是数字"
	}
	if lat < -90 || lat > 90 {
		return 0, 0, "参数 lat 必须在 -90 到 90 之间"
	}
	if lon < -180 || lon > 180 {
		return 0, 0, "参数 lon 必须在 -180 到 180 之间"
	}
	return lat, lon, ""
}

// coordPrecision 是坐标量化精度（小数位数）。2 位小数约 1.1km，与上游天气模型的
// 网格尺度（公里级）相当。刻意不保留更高精度：GPS 坐标在同一城市内不同设备之间会
// 漂移数百米，保留 4 位小数（约 11m）会让缓存键几乎无法跨设备复用，命中率趋近于 0，
// 而天气结果并无差别。
const coordPrecision = 2

// formatCoord 把坐标量化到 coordPrecision 位小数，并同时用于上游入参与缓存键，
// 保证"同一个量化坐标"始终指向同一条缓存与同一个上游网格。
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', coordPrecision, 64)
}

func coordKey(name string, lat, lon float64, tz string) string {
	return fmt.Sprintf("%s:%s:%s:%s", name, formatCoord(lat), formatCoord(lon), tz)
}

// parseTZ 解析并校验 tz 参数；缺失或非法一律回落 "auto"（交由上游按坐标判断）。
//
// tz 由浏览器提供（Intl.DateTimeFormat().resolvedOptions().timeZone），让"时区"与
// "坐标"解耦——这正是坐标可以放心量化到 1km 的前提。必须用 time.LoadLocation 校验后
// 才写入缓存键：否则任意字符串都能构造出新键，仅凭一个坐标就能把缓存键空间无限撑大。
func parseTZ(r *http.Request) string {
	raw := strings.TrimSpace(r.URL.Query().Get("tz"))
	if raw == "" {
		return "auto"
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return "auto"
	}
	return loc.String()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// statusRecorder 记录响应状态码，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.logger.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	})
}
