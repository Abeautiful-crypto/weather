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
	"testing"
	"testing/fstest"
	"time"

	"weather/internal/cache"
	"weather/internal/qweather"
	"weather/internal/upstream"
)

// 官方文档给出的 GeoAPI 响应示例，作为测试夹具（字段名与类型与文档一致）。
const qweatherLookupOK = `{
  "code": "200",
  "location": [
    {
      "name": "宿迁",
      "id": "101191301",
      "lat": "33.94917",
      "lon": "118.29583",
      "adm2": "宿迁",
      "adm1": "江苏",
      "country": "中国",
      "tz": "Asia/Shanghai",
      "utcOffset": "+08:00",
      "isDst": "0",
      "type": "city",
      "rank": "35",
      "fxLink": "https://www.qweather.com/weather/suqian-101191301.html"
    }
  ]
}`

// newQWeatherEnv 启动一个假的和风服务（TLS，便于用 ts.Client() 作为 HTTP 客户端），
// 返回被测服务地址、和风侧收到的请求记录。
func newQWeatherEnv(t *testing.T, handler http.HandlerFunc) (baseURL string, seen func() []*http.Request) {
	t.Helper()

	var mu sync.Mutex
	var requests []*http.Request
	qwTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// 记录一份副本：请求对象在 handler 返回后不再可靠
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		requests = append(requests, clone)
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(qwTS.Close)

	qwClient, err := qweather.New(qweather.Config{
		Host:       strings.TrimPrefix(qwTS.URL, "https://"),
		APIKey:     "test-key-should-never-leak",
		Timeout:    time.Second,
		HTTPClient: qwTS.Client(),
	})
	if err != nil {
		t.Fatalf("创建和风客户端失败：%v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cache.New(time.Minute), upstream.New(time.Second, upstream.DefaultEndpoints()), qwClient, logger)
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<title>weather</title>")}}
	ts := httptest.NewServer(srv.Routes(static))
	t.Cleanup(ts.Close)

	return ts.URL, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]*http.Request(nil), requests...)
	}
}

// 启用和风后，中文地名应能搜到，且返回结构必须与前端已经在用的 Open-Meteo 结构一致
// （name / latitude / longitude / admin1 / country），这样前端不需要任何改动。
func TestGeocodeUsesQWeatherAndKeepsFrontendShape(t *testing.T) {
	base, seen := newQWeatherEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/geo/v2/city/lookup" {
			t.Errorf("请求路径不符：%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, qweatherLookupOK)
	})

	res := get(t, base+"/api/geocode?q="+url.QueryEscape("宿迁"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "qweather" {
		t.Fatalf("X-Geocode-Source 应为 qweather，实际 %q", got)
	}

	var parsed struct {
		Results []struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Admin1    string  `json:"admin1"`
			Country   string  `json:"country"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(parsed.Results) != 1 {
		t.Fatalf("应返回 1 条结果，实际 %d", len(parsed.Results))
	}
	got := parsed.Results[0]
	if got.Name != "宿迁" || got.Admin1 != "江苏" || got.Country != "中国" {
		t.Fatalf("地点字段映射不符：%+v", got)
	}
	// 文档里经纬度是字符串，必须转成数字供前端直接拼接
	if got.Latitude != 33.94917 || got.Longitude != 118.29583 {
		t.Fatalf("经纬度应被解析为数字，实际 %v / %v", got.Latitude, got.Longitude)
	}

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("和风应只被调用 1 次，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("location"); got != "宿迁" {
		t.Fatalf("location 参数应为 宿迁，实际 %q", got)
	}
	if got := reqs[0].URL.Query().Get("lang"); got != "zh" {
		t.Fatalf("lang 参数应为 zh，实际 %q", got)
	}
	if got := reqs[0].Header.Get(qweather.AuthHeaderAPIKey); got != "test-key-should-never-leak" {
		t.Fatalf("应通过 %s 请求头鉴权，实际 %q", qweather.AuthHeaderAPIKey, got)
	}
	// API Key 绝不能出现在 URL 里（URL 更容易进日志）
	if strings.Contains(reqs[0].URL.RawQuery, "test-key-should-never-leak") {
		t.Fatal("API Key 不应出现在查询参数中")
	}
}

func TestGeocodeQWeatherResultsAreCached(t *testing.T) {
	base, seen := newQWeatherEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, qweatherLookupOK)
	})
	target := base + "/api/geocode?q=" + url.QueryEscape("宿迁")

	if got := get(t, target).Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("首次应为 MISS，实际 %q", got)
	}
	if got := get(t, target).Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("二次应为 HIT，实际 %q", got)
	}
	if n := len(seen()); n != 1 {
		t.Fatalf("和风应只被调用 1 次，实际 %d 次", n)
	}
}

func TestGeocodeQWeatherEmptyResult(t *testing.T) {
	base, _ := newQWeatherEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"404"}`)
	})

	res := get(t, base+"/api/geocode?q="+url.QueryEscape("不存在的地方"))
	var parsed struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(parsed.Results) != 0 {
		t.Fatalf("查无结果时应返回空数组，实际 %d 条", len(parsed.Results))
	}
}

// 鉴权失败必须给出可操作的中文提示，且绝不能回显 API Key。
func TestGeocodeQWeatherAuthFailureIsActionableAndDoesNotLeakKey(t *testing.T) {
	base, _ := newQWeatherEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"401"}`)
	})

	res := get(t, base+"/api/geocode?q="+url.QueryEscape("宿迁"))
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("鉴权失败应映射为 502，实际 %d", res.StatusCode)
	}
	body := jsonBody(t, res)
	if !strings.Contains(body["error"], "鉴权失败") {
		t.Fatalf("错误信息应说明鉴权失败：%v", body)
	}
	if !strings.Contains(body["error"], "QWEATHER_KEY") {
		t.Fatalf("错误信息应指出要检查哪个环境变量：%v", body)
	}
	if strings.Contains(body["error"], "test-key-should-never-leak") {
		t.Fatalf("错误信息不得包含 API Key：%v", body)
	}
}

// 配额用尽要有明确提示（免费额度耗尽是最常见的失败原因）。
func TestGeocodeQWeatherQuotaExceeded(t *testing.T) {
	base, _ := newQWeatherEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"402"}`)
	})

	res := get(t, base+"/api/geocode?q="+url.QueryEscape("宿迁"))
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("配额超限应映射为 502，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "额度") {
		t.Fatalf("错误信息应提到额度：%v", body)
	}
}

// 未配置和风（qw == nil）时必须走 Open-Meteo 的候选降级路径，且行为不变。
func TestGeocodeFallsBackToOpenMeteoWithoutQWeather(t *testing.T) {
	var mu sync.Mutex
	var names []string
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		names = append(names, r.URL.Query().Get("name"))
		mu.Unlock()
		_, _ = io.WriteString(w, validGeocode)
	})

	res := get(t, base+"/api/geocode?q="+url.QueryEscape("江苏宿迁"))
	if got := res.Header.Get("X-Geocode-Source"); got != "open-meteo" {
		t.Fatalf("未启用和风时应标记 open-meteo，实际 %q", got)
	}
	if got := res.Header.Get("X-Geocode-Candidates"); !strings.Contains(got, "宿迁") {
		t.Fatalf("应保留候选降级头，实际 %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(names) == 0 {
		t.Fatal("应实际请求了 Open-Meteo")
	}
}
