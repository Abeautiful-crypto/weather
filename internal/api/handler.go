// Package api 提供 HTTP 路由：三个代理接口、健康检查与内嵌前端静态资源托管。
package api

import (
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
	"weather/internal/upstream"
)

// maxQueryRunes 限制城市关键词长度，避免超长参数打到上游。
const maxQueryRunes = 64

// Server 持有依赖，便于在测试中替换。
type Server struct {
	cache  *cache.Cache
	up     *upstream.Client
	logger *slog.Logger
}

// New 创建 Server。
func New(c *cache.Cache, up *upstream.Client, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cache: c, up: up, logger: logger}
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
		mux.Handle("/", http.FileServerFS(static))
	}
	return s.withLogging(mux)
}

/* ------------------------------------------------------------------ handlers */

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("接口不存在：%s", r.URL.Path))
}

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

	params := url.Values{}
	params.Set("name", q)
	params.Set("count", "6")
	params.Set("language", "zh")
	params.Set("format", "json")

	s.proxy(w, r, "geocode", "geocode:"+strings.ToLower(q), s.up.Endpoints().Geocode, params)
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
	params.Set("timezone", "auto")
	params.Set("forecast_days", "7")

	s.proxy(w, r, "weather", coordKey("weather", lat, lon), s.up.Endpoints().Forecast, params)
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
	params.Set("current", "pm2_5,pm10,us_aqi")
	params.Set("timezone", "auto")

	s.proxy(w, r, "air", coordKey("air", lat, lon), s.up.Endpoints().Air, params)
}

/* ------------------------------------------------------------------ helpers */

// proxy 走缓存 + 单飞的统一代理流程。
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, name, key, endpoint string, params url.Values) {
	data, err, hit := s.cache.GetOrLoad(key, func() ([]byte, bool, error) {
		// 请求 context 透传到上游：客户端断开时上游连接会一并释放。
		body, fetchErr := s.up.FetchWithQuery(r.Context(), endpoint, params)
		if fetchErr != nil {
			return nil, false, fetchErr
		}
		// 只有能被解析的 JSON 才写入缓存；否则原样透传交给前端兜底。
		return body, json.Valid(body), nil
	})

	if err != nil {
		var upErr *upstream.Error
		if errors.As(err, &upErr) {
			s.logger.Warn("代理失败", "api", name, "status", upErr.Status, "err", upErr)
			writeError(w, upErr.Status, upErr.Msg)
			return
		}
		s.logger.Error("代理异常", "api", name, "err", err)
		writeError(w, http.StatusInternalServerError, "服务内部错误")
		return
	}

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

// formatCoord 统一经纬度格式，保证缓存键稳定。
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func coordKey(name string, lat, lon float64) string {
	return fmt.Sprintf("%s:%s:%s", name, formatCoord(lat), formatCoord(lon))
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
