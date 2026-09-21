package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"weather/internal/cache"
	"weather/internal/upstream"
)

const (
	validForecast = `{"latitude":39.9,"current":{"temperature_2m":31.5},"daily":{"time":["2026-09-21"]}}`
	validGeocode  = `{"results":[{"name":"上海","latitude":31.22,"longitude":121.45}]}`
)

// newEnv 启动一个假的 Open-Meteo 与一个被测服务，返回被测服务地址与上游调用计数器。
func newEnv(t *testing.T, ttl, timeout time.Duration, up http.HandlerFunc) (baseURL string, calls *int32) {
	t.Helper()
	var n int32
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		up(w, r)
	}))
	t.Cleanup(upTS.Close)

	baseURL = newEnvWithEndpoints(t, ttl, timeout, upstream.Endpoints{
		Geocode:  upTS.URL + "/v1/search",
		Forecast: upTS.URL + "/v1/forecast",
		Air:      upTS.URL + "/v1/air-quality",
	})
	return baseURL, &n
}

// newEnvWithEndpoints 允许直接指定上游地址（用于模拟上游不可达）。
func newEnvWithEndpoints(t *testing.T, ttl, timeout time.Duration, eps upstream.Endpoints) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cache.New(ttl), upstream.New(timeout, eps), logger)

	static := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>weather</title>")},
	}
	ts := httptest.NewServer(srv.Routes(static))
	t.Cleanup(ts.Close)
	return ts.URL
}

func jsonBody(t *testing.T, res *http.Response) map[string]string {
	t.Helper()
	defer res.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("响应不是合法 JSON 对象: %v", err)
	}
	return out
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求失败 %s: %v", url, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

/* ------------------------------------------------------------------ 基础路由 */

func TestHealthOK(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("健康检查不应访问上游")
	})
	res := get(t, base+"/api/health")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); body["status"] != "ok" {
		t.Fatalf("响应体不符：%v", body)
	}
}

func TestServesEmbeddedIndex(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {})
	res := get(t, base+"/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首页状态码应为 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "<title>weather</title>") {
		t.Fatalf("首页内容不符：%s", body)
	}
}

func TestStaticDirectoryListingIsDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cache.New(time.Minute), upstream.New(time.Second, upstream.DefaultEndpoints()), logger)
	static := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<title>weather</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	ts := httptest.NewServer(srv.Routes(static))
	t.Cleanup(ts.Close)

	res := get(t, ts.URL+"/assets/")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("目录访问应返回 404，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "app.js") {
		t.Fatalf("不应列出目录内容：%s", body)
	}

	// 目录禁用不能误伤静态文件本身
	if res := get(t, ts.URL+"/assets/app.js"); res.StatusCode != http.StatusOK {
		t.Fatalf("静态文件应仍可访问，实际 %d", res.StatusCode)
	}
	if res := get(t, ts.URL+"/"); res.StatusCode != http.StatusOK {
		t.Fatalf("根路径应仍可访问，实际 %d", res.StatusCode)
	}
}

func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {})
	res := get(t, base+"/api/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码应为 404，实际 %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实际 %q", ct)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "接口不存在") {
		t.Fatalf("错误信息不符：%v", body)
	}
}

/* ------------------------------------------------------------------ 参数校验 */

func TestWeatherParamValidation(t *testing.T) {
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应访问上游")
	})

	cases := []struct {
		name   string
		query  string
		errSub string
	}{
		{"缺少两个参数", "", "缺少参数 lat 或 lon"},
		{"缺少 lon", "?lat=39.9", "缺少参数 lat 或 lon"},
		{"lat 非数字", "?lat=abc&lon=116.4", "参数 lat 必须是数字"},
		{"lon 非数字", "?lat=39.9&lon=xyz", "参数 lon 必须是数字"},
		{"lat 超范围", "?lat=95&lon=116.4", "参数 lat 必须在 -90 到 90 之间"},
		{"lon 超范围", "?lat=39.9&lon=-200", "参数 lon 必须在 -180 到 180 之间"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := get(t, base+"/api/weather"+tc.query)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码应为 400，实际 %d", res.StatusCode)
			}
			if body := jsonBody(t, res); !strings.Contains(body["error"], tc.errSub) {
				t.Fatalf("错误信息应包含 %q，实际 %v", tc.errSub, body)
			}
		})
	}

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("非法参数不应触发上游请求，实际 %d 次", got)
	}
}

func TestAirParamValidation(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应访问上游")
	})
	res := get(t, base+"/api/air?lat=39.9")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，实际 %d", res.StatusCode)
	}
}

func TestGeocodeParamValidation(t *testing.T) {
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应访问上游")
	})

	for _, q := range []string{"", "   "} {
		res := get(t, base+"/api/geocode?q="+url.QueryEscape(q))
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("空白关键词应返回 400，实际 %d", res.StatusCode)
		}
		if body := jsonBody(t, res); !strings.Contains(body["error"], "缺少参数 q") {
			t.Fatalf("错误信息不符：%v", body)
		}
	}

	long := strings.Repeat("京", 65)
	res := get(t, base+"/api/geocode?q="+url.QueryEscape(long))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("超长关键词应返回 400，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "过长") {
		t.Fatalf("错误信息不符：%v", body)
	}

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("非法参数不应触发上游请求，实际 %d 次", got)
	}
}

/* ------------------------------------------------------------------ 代理与缓存 */

func TestWeatherProxiesThenServesFromCache(t *testing.T) {
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/forecast" {
			t.Errorf("上游路径不符：%s", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("latitude"); got != "39.91" {
			t.Errorf("latitude 应被量化到 2 位小数即 39.91，实际 %q", got)
		}
		if got := q.Get("longitude"); got != "116.40" {
			t.Errorf("longitude 应被量化到 2 位小数即 116.40，实际 %q", got)
		}
		if got := q.Get("forecast_days"); got != "7" {
			t.Errorf("forecast_days 应为 7，实际 %q", got)
		}
		if got := q.Get("timezone"); got != "auto" {
			t.Errorf("timezone 应为 auto，实际 %q", got)
		}
		if !strings.Contains(q.Get("current"), "weather_code") {
			t.Errorf("current 参数缺少 weather_code：%q", q.Get("current"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validForecast)
	})

	url := base + "/api/weather?lat=39.9075&lon=116.39723"

	first := get(t, url)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("首次请求状态码应为 200，实际 %d", first.StatusCode)
	}
	if got := first.Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("首次应为 MISS，实际 %q", got)
	}
	body, _ := io.ReadAll(first.Body)
	if string(body) != validForecast {
		t.Fatalf("透传的响应体应保持不变：%s", body)
	}

	second := get(t, url)
	if got := second.Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("二次应为 HIT，实际 %q", got)
	}
	body2, _ := io.ReadAll(second.Body)
	if string(body2) != validForecast {
		t.Fatalf("缓存返回的响应体不符：%s", body2)
	}

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("上游应只被调用 1 次，实际 %d 次", got)
	}
}

func TestGeocodeForwardsQuery(t *testing.T) {
	var gotName, gotLang, gotCount string
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotName, gotLang, gotCount = q.Get("name"), q.Get("language"), q.Get("count")
		_, _ = io.WriteString(w, validGeocode)
	})

	res := get(t, base+"/api/geocode?q=%E4%B8%8A%E6%B5%B7") // 上海
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if gotName != "上海" {
		t.Fatalf("上游收到的 name 应为 上海，实际 %q", gotName)
	}
	if gotLang != "zh" || gotCount != "6" {
		t.Fatalf("上游参数不符：language=%q count=%q", gotLang, gotCount)
	}
}

func TestAirProxiesSuccessfully(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/air-quality" {
			t.Errorf("上游路径不符：%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"current":{"us_aqi":175}}`)
	})
	res := get(t, base+"/api/air?lat=39.9&lon=116.4")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"current":{"us_aqi":175}}` {
		t.Fatalf("上游响应应原样透传，实际：%s", body)
	}
}

// 上游返回 200 但不是 JSON 时，按上游故障处理：映射为 502、给出中文提示、
// 不把无法解析的原始体回显给客户端，且失败结果不入缓存。
func TestInvalidJSONMapsTo502AndIsNotCached(t *testing.T) {
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not a json at all")
	})

	url := base + "/api/weather?lat=1&lon=2"
	first := get(t, url)
	if first.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游非 JSON 应映射为 502，实际 %d", first.StatusCode)
	}
	body := jsonBody(t, first)
	if !strings.Contains(body["error"], "无法解析") {
		t.Fatalf("错误信息不符：%v", body)
	}
	if strings.Contains(body["error"], "not a json at all") {
		t.Fatalf("不应把上游原始响应体回显给客户端：%v", body)
	}

	get(t, url) // 失败不缓存，重试仍应打到上游
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("不可解析的响应不应被缓存，上游应被调用 2 次，实际 %d 次", got)
	}
}

// tz 必须转发给上游并纳入缓存键：同一坐标不同时区互不串味；非法 tz 回落 auto，
// 且不会因为字符串不同而产生新缓存键（否则任意字符串都能撑大缓存键空间）。
func TestTZIsForwardedAndPartitionsCache(t *testing.T) {
	var mu sync.Mutex
	var lastTZ string
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastTZ = r.URL.Query().Get("timezone")
		mu.Unlock()
		_, _ = io.WriteString(w, validForecast)
	})
	tzSeen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastTZ
	}

	shanghai := base + "/api/weather?lat=39.9&lon=116.4&tz=Asia%2FShanghai"

	if res := get(t, shanghai); res.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("首次请求应为 MISS，实际 %q", res.Header.Get("X-Cache"))
	}
	if got := tzSeen(); got != "Asia/Shanghai" {
		t.Fatalf("tz 应转发给上游，实际 timezone=%q", got)
	}

	// 同坐标不同时区 → 缓存必须隔离
	if res := get(t, base+"/api/weather?lat=39.9&lon=116.4&tz=UTC"); res.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("不同 tz 不应命中同一缓存条目，实际 %q", res.Header.Get("X-Cache"))
	}
	if got := tzSeen(); got != "UTC" {
		t.Fatalf("timezone 应为 UTC，实际 %q", got)
	}

	// 同时区重复请求 → 命中
	if res := get(t, shanghai); res.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("同 tz 重复请求应命中，实际 %q", res.Header.Get("X-Cache"))
	}

	// 缺省 tz → auto
	if res := get(t, base+"/api/weather?lat=39.9&lon=116.4"); res.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("首次缺省 tz 应为 MISS，实际 %q", res.Header.Get("X-Cache"))
	}
	if got := tzSeen(); got != "auto" {
		t.Fatalf("缺省 tz 应回落 auto，实际 %q", got)
	}

	// 非法 tz → 回落 auto，并复用同一条 auto 缓存（不产生新键）
	if res := get(t, base+"/api/weather?lat=39.9&lon=116.4&tz=Not%2FAZone"); res.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("非法 tz 应回落 auto 并命中既有条目，实际 %q", res.Header.Get("X-Cache"))
	}

	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("上游应只被调用 3 次（auto / Asia/Shanghai / UTC），实际 %d 次", got)
	}
}

// 同一城市不同设备的 GPS 坐标会漂移数百米；量化到 2 位小数后应共享同一条缓存，
// 这才是"提升命中率"真正生效的路径。同时确认量化没有把明显不同的坐标准确化到一起。
func TestCoordQuantizationSharesCacheEntry(t *testing.T) {
	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, validForecast)
	})

	// 两点相距约 50m
	deviceA := base + "/api/weather?lat=39.90751&lon=116.39723"
	deviceB := base + "/api/weather?lat=39.90799&lon=116.39751"

	if res := get(t, deviceA); res.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("设备 A 首次应为 MISS，实际 %q", res.Header.Get("X-Cache"))
	}
	if res := get(t, deviceB); res.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("设备 B 的坐标量化后应命中同一条缓存，实际 %q", res.Header.Get("X-Cache"))
	}

	// 明显不同的坐标不得被合并
	if res := get(t, base+"/api/weather?lat=39.95&lon=116.45"); res.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("明显不同的坐标不应命中，实际 %q", res.Header.Get("X-Cache"))
	}

	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("上游应被调用 2 次，实际 %d 次", got)
	}
}

// 客户端断开（此处用客户端超时模拟）不得连坐上游调用：上游仍应以未取消的
// context 正常完成，并把结果写入缓存供后续请求命中。
func TestClientCancelDoesNotCancelUpstream(t *testing.T) {
	upstreamContext := make(chan error, 1)
	upstreamDone := make(chan struct{})

	base, calls := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 故意慢于客户端超时
		upstreamContext <- r.Context().Err()
		_, _ = io.WriteString(w, validForecast)
		close(upstreamDone)
	})

	client := &http.Client{Timeout: 80 * time.Millisecond}
	res, err := client.Get(base + "/api/weather?lat=39.9&lon=116.4")
	if err == nil {
		res.Body.Close()
		t.Fatal("客户端应当因超时而放弃本次请求")
	}

	select {
	case ctxErr := <-upstreamContext:
		if ctxErr != nil {
			t.Fatalf("客户端断开后上游 context 不应被取消，实际 err=%v", ctxErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上游请求未完成")
	}
	<-upstreamDone

	// 断线的客户端反而替后续请求预热了缓存
	deadline := time.Now().Add(2 * time.Second)
	for {
		r := get(t, base+"/api/weather?lat=39.9&lon=116.4")
		if r.Header.Get("X-Cache") == "HIT" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("上游结果未写入缓存，上游调用次数=%d", atomic.LoadInt32(calls))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("上游应只被调用 1 次，实际 %d 次", got)
	}
}

/* ------------------------------------------------------------------ 错误映射 */

func TestUpstreamNon2xxMapsTo502(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	res := get(t, base+"/api/weather?lat=39.9&lon=116.4")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游 5xx 应映射为 502，实际 %d", res.StatusCode)
	}
	body := jsonBody(t, res)
	if !strings.Contains(body["error"], "上游天气服务返回异常") {
		t.Fatalf("错误信息不符：%v", body)
	}
	if strings.Contains(body["error"], "open-meteo") {
		t.Fatalf("错误信息不应泄漏上游地址：%v", body)
	}
}

func TestUpstreamTimeoutMapsTo504(t *testing.T) {
	base, _ := newEnv(t, time.Minute, 80*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, validForecast)
	})
	res := get(t, base+"/api/weather?lat=39.9&lon=116.4")
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("上游超时应映射为 504，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "超时") {
		t.Fatalf("错误信息不符：%v", body)
	}
}

func TestUpstreamUnreachableMapsTo502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 立刻关闭，制造连接失败

	base := newEnvWithEndpoints(t, time.Minute, time.Second, upstream.Endpoints{
		Geocode:  deadURL + "/v1/search",
		Forecast: deadURL + "/v1/forecast",
		Air:      deadURL + "/v1/air-quality",
	})

	res := get(t, base+"/api/weather?lat=39.9&lon=116.4")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游不可达应映射为 502，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "无法连接上游天气服务") {
		t.Fatalf("错误信息不符：%v", body)
	}
}

func TestUpstreamErrorsAreNotCached(t *testing.T) {
	var calls int32
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, validForecast)
	})

	url := base + "/api/weather?lat=39.9&lon=116.4"
	if res := get(t, url); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("首次应为 502，实际 %d", res.StatusCode)
	}
	// 失败后重试应立即重新请求上游并成功（说明失败结果未入缓存）
	res := get(t, url)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("重试应为 200，实际 %d", res.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("上游应被调用 2 次，实际 %d 次", got)
	}
}
