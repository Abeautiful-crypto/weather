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

	"weather/internal/amap"
	"weather/internal/cache"
	"weather/internal/upstream"
)

// testAMapKey 是**假 Key**（形态与真实 Key 一致即可）：真实 Key 只经环境变量
// AMAP_KEY 传入，不得写入任何文件。
const testAMapKey = "0123456789abcdef0123456789abcdef"

// 实测夹具：`朝阳` 在高德返回 3 条，且朝阳市的 district 是空数组。
// 这三条必须仅凭前端展示的 name + admin1 就能互相区分。
const amapChaoyang = `{"status":"1","info":"OK","infocode":"10000","count":"3","geocodes":[
	{"formatted_address":"辽宁省朝阳市","country":"中国","province":"辽宁省","city":"朝阳市",
	 "district":[],"adcode":"211300","location":"120.488801,41.601855","level":"市"},
	{"formatted_address":"吉林省长春市朝阳区","country":"中国","province":"吉林省","city":"长春市",
	 "district":"朝阳区","adcode":"220104","location":"125.288168,43.833845","level":"区县"},
	{"formatted_address":"北京市朝阳区","country":"中国","province":"北京市","city":"北京市",
	 "district":"朝阳区","adcode":"110105","location":"116.443136,39.921444","level":"区县"}]}`

// 实测夹具：用转换后的高德坐标反查天安门（city 为空数组，须用 province 兜底）。
const amapTiananmen = `{"status":"1","info":"OK","infocode":"10000","regeocode":{"addressComponent":{
	"city":[],"province":"北京市","adcode":"110101","district":"东城区","country":"中国",
	"township":"东华门街道","citycode":"010"},"formatted_address":"北京市东城区东华门街道天安门"}}`

const amapConverted = `{"status":"1","info":"ok","infocode":"10000","locations":"116.397340223525,39.909000379775"}`

// 实测夹具：高德无法把 Tokyo / 东京 解析成日本东京，却返回字面叫"东京"的村庄与
// 兴趣点（level 为「村庄」「兴趣点」）。这类弱结果不得挡住正确的海外答案。
const amapWeakVillages = `{"status":"1","info":"OK","infocode":"10000","count":"2","geocodes":[
	{"formatted_address":"广西壮族自治区贵港市平南县东京","country":"中国","province":"广西壮族自治区","city":"贵港市",
	 "district":"平南县","adcode":"450821","location":"110.451480,23.202676","level":"村庄"},
	{"formatted_address":"香港特别行政区油尖旺区东京","country":"中国","province":"香港特别行政区","city":[],
	 "district":"油尖旺区","adcode":"810005","location":"114.178279,22.313685","level":"兴趣点"}]}`

// 实测夹具：无法解析的输入返回 status=0 + infocode=30001，而不是空结果数组。
const amapUnresolvable = `{"status":"0","info":"ENGINE_RESPONSE_DATA_ERROR","infocode":"30001"}`

// 实测夹具（东京坐标）：境外坐标的逆地理编码返回 status=1 / infocode=10000，
// 但所有地址字段都是空数组——连 formatted_address 也是。
const amapRegeoEmptyOverseas = `{"status":"1","info":"OK","infocode":"10000","regeocode":{"formatted_address":[],"addressComponent":{"country":[],"province":[],"city":[],"citycode":[],"district":[],"adcode":[],"township":[],"towncode":[],"streetNumber":{"street":[],"number":[],"location":"139.69171,35.6895","direction":[],"distance":[]}},"pois":[],"roads":[],"roadinters":[],"aois":[]}}`

// dualEnv 是被测服务与两个假上游的组合：高德（可记录请求）与 Open-Meteo（可计数）。
type dualEnv struct {
	base      string
	amapReqs  func() []*http.Request
	openCalls func() int
}

// newDualEnv 启动假高德 + 假 Open-Meteo 的被测服务。和风固定为未配置，
// 这样"高德查无结果 → 回落 Open-Meteo"这条链路可被完整观测。
func newDualEnv(t *testing.T, amapHandler, openHandler http.HandlerFunc) dualEnv {
	t.Helper()

	var mu sync.Mutex
	var reqs []*http.Request
	amapTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		reqs = append(reqs, clone)
		mu.Unlock()
		amapHandler(w, r)
	}))
	t.Cleanup(amapTS.Close)

	var openCount int32
	openTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&openCount, 1)
		openHandler(w, r)
	}))
	t.Cleanup(openTS.Close)

	amClient, err := amap.New(amap.Config{
		Key:        testAMapKey,
		Timeout:    time.Second,
		HTTPClient: amapTS.Client(),
		Endpoints: amap.Endpoints{
			Geocode: amapTS.URL + "/v3/geocode/geo",
			Regeo:   amapTS.URL + "/v3/geocode/regeo",
			Convert: amapTS.URL + "/v3/assistant/coordinate/convert",
		},
	})
	if err != nil {
		t.Fatalf("创建高德客户端失败：%v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(
		cache.New(time.Minute),
		upstream.New(time.Second, upstream.Endpoints{
			Geocode:  openTS.URL + "/v1/search",
			Forecast: openTS.URL + "/v1/forecast",
			Air:      openTS.URL + "/v1/air-quality",
		}),
		amClient,
		nil,
		logger,
	)
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<title>weather</title>")}}
	ts := httptest.NewServer(srv.Routes(static))
	t.Cleanup(ts.Close)

	return dualEnv{
		base: ts.URL,
		amapReqs: func() []*http.Request {
			mu.Lock()
			defer mu.Unlock()
			return append([]*http.Request(nil), reqs...)
		},
		openCalls: func() int { return int(atomic.LoadInt32(&openCount)) },
	}
}

// 高德结果必须映射成前端已经在用的结构，否则前端要为新数据源写分支。
func TestGeocodeUsesAMapAndKeepsFrontendShape(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, amapChaoyang)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("高德有结果时不应请求 Open-Meteo")
	})

	res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("朝阳"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "amap" {
		t.Fatalf("X-Geocode-Source 应为 amap，实际 %q", got)
	}

	var parsed struct {
		Results []struct {
			Name      string  `json:"name"`
			Admin1    string  `json:"admin1"`
			Country   string  `json:"country"`
			Level     string  `json:"level"`
			Adcode    string  `json:"adcode"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(parsed.Results) != 3 {
		t.Fatalf("应返回 3 条，实际 %d 条", len(parsed.Results))
	}

	first := parsed.Results[0]
	if first.Name != "朝阳市" || first.Admin1 != "辽宁省" || first.Level != "地级" {
		t.Fatalf("市级别结果映射不符：%+v", first)
	}
	// 坐标顺序：location 是「经度,纬度」，映射后必须落到 latitude/longitude
	if first.Latitude != 41.601855 || first.Longitude != 120.488801 {
		t.Fatalf("经纬度映射错误：%+v", first)
	}
	if first.Adcode != "211300" || first.Country != "中国" {
		t.Fatalf("adcode/country 映射不符：%+v", first)
	}

	// 三条同名结果必须仅凭 name + admin1 就能区分（前端只展示这两个字段）
	labels := make(map[string]bool, len(parsed.Results))
	for _, r := range parsed.Results {
		label := r.Name + "|" + r.Admin1
		if labels[label] {
			t.Fatalf("候选标签重复，用户无法区分：%q", label)
		}
		labels[label] = true
	}
	if !labels["朝阳区|北京市"] || !labels["朝阳区|吉林省 · 长春市"] {
		t.Fatalf("区县级结果应带上「省 · 市」语境，实际：%v", labels)
	}

	reqs := env.amapReqs()
	if len(reqs) != 1 {
		t.Fatalf("高德应只被调用 1 次，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("address"); got != "朝阳" {
		t.Fatalf("address 参数不符：%q", got)
	}
	if got := reqs[0].URL.Query().Get("key"); got != testAMapKey {
		t.Fatalf("key 参数不符：%q", got)
	}
}

// 高德以中国大陆为主：若不回落，搜海外城市会从"能用"退化成"搜不到"。
func TestGeocodeFallsThroughToOpenMeteoWhenAMapHasNoResult(t *testing.T) {
	var openNames []string
	var mu sync.Mutex
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"1","infocode":"10000","count":"0","geocodes":[]}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		openNames = append(openNames, r.URL.Query().Get("name"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"id":1,"name":"Tokyo","latitude":35.68,"longitude":139.69,"feature_code":"PPLC","population":8000000}]}`)
	})

	res := get(t, env.base+"/api/geocode?q=Tokyo")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "open-meteo" {
		t.Fatalf("高德无结果时应标记 open-meteo，实际 %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "Tokyo") {
		t.Fatalf("应返回 Open-Meteo 的结果：%s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(openNames) == 0 {
		t.Fatal("应实际回落到 Open-Meteo")
	}
	// 回落路径必须保持全球覆盖：一旦加上 countryCode=CN，"搜不到海外城市"就会
	// 重新出现，正是这条回落要解决的问题。
	for _, n := range openNames {
		if n == "" {
			t.Fatal("候选词不应为空")
		}
	}
}

// 高德报错几乎都是配置问题（Key 无效 / 平台不匹配 / 白名单 / 额度），
// 必须直接暴露，不能靠静态回落到 Open-Meteo 把它藏起来。
func TestGeocodeAMapErrorIsSurfacedWithoutFallthrough(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"0","info":"INVALID_USER_KEY","infocode":"10001"}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("高德报错时不应静默回落到 Open-Meteo")
	})

	res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("上海"))
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("应映射为 502，实际 %d", res.StatusCode)
	}
	body := jsonBody(t, res)
	if !strings.Contains(body["error"], "AMAP_KEY") {
		t.Fatalf("提示应指出要检查 AMAP_KEY：%v", body)
	}
	if env.openCalls() != 0 {
		t.Fatalf("不应回落到 Open-Meteo，实际调用 %d 次", env.openCalls())
	}
}

// 高德用 infocode=30001 表达"这个输入解析不出来"。这是查无此地，不是故障：
// 必须走"未找到相关城市"并允许回落，而不是甩一个 502 给用户。
func TestGeocodeUnresolvableInputIsTreatedAsNoResult(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, amapUnresolvable)
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("不存在的城市名"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("查无此地应返回 200 与空结果，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "open-meteo" {
		t.Fatalf("应继续回落到 Open-Meteo，实际来源 %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.TrimSpace(string(body)) != `{"results":[]}` {
		t.Fatalf("应为空结果，前端据此显示「未找到相关城市」，实际 %s", body)
	}
}

// 高德是地址解析：解析不到"日本东京"时它退化成"字面叫东京的村庄"。
// 弱结果不能中断回落链，否则 GeoNames 给出的正确答案永远看不到。
func TestGeocodeWeakOnlyResultsFallThroughToNameSearch(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, amapWeakVillages)
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":1850147,"name":"东京","latitude":35.6895,"longitude":139.69171,"feature_code":"PPLC","population":8336599}]}`)
	})

	res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("东京"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "open-meteo" {
		t.Fatalf("弱结果应继续回落，实际来源 %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "东京") || strings.Contains(string(body), "平南县") {
		t.Fatalf("应返回真正的东京而不是同名村庄：%s", body)
	}
}

// 但弱结果也不能被丢掉：只搜得到村庄时（例如"三元村"，实测高德返回 10 条 level=村庄）
// 它就是这个输入唯一的答案，此时必须把它还给用户，而不是显示"未找到"。
func TestGeocodeWeakResultsAreKeptWhenNoSourceHasAdministrativeMatch(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, amapWeakVillages)
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("三元村"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Geocode-Source"); got != "amap" {
		t.Fatalf("没有更强的结果时应保留高德的弱结果，实际来源 %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "平南县") {
		t.Fatalf("应返回高德的村庄级结果：%s", body)
	}
	// 弱结果必须带级别标注，前端据此提示"这不是行政区"
	if !strings.Contains(string(body), "村镇级") && !strings.Contains(string(body), "非行政地名") {
		t.Fatalf("弱结果应带级别标注：%s", body)
	}
}

// "没有对应地区数据"有两种实测形态，都必须映射成 404 而不是 502/500，
// 前端据此回落到"当前位置 + 经纬度"。
func TestRegeoNoResultMapsToNotFound(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"高德返回 30001（解析不出这个坐标）", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, amapUnresolvable)
		}},
		{"境外坐标：请求成功但所有地址字段为空数组", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/coordinate/convert") {
				_, _ = io.WriteString(w, amapConverted)
				return
			}
			_, _ = io.WriteString(w, amapRegeoEmptyOverseas)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDualEnv(t, tc.handler, func(w http.ResponseWriter, r *http.Request) {
				t.Error("定位取名不应访问 Open-Meteo")
			})

			res := get(t, env.base+"/api/regeo?lat=35.68&lon=139.69")
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("无对应地区数据应返回 404，实际 %d", res.StatusCode)
			}
			if body := jsonBody(t, res); !strings.Contains(body["error"], "识别") {
				t.Fatalf("提示应说明未能识别该位置：%v", body)
			}
		})
	}
}

// 未配置 AMAP_KEY 时定位取名能力不可用，用 503 与"客户端错误/上游故障"区分开。
func TestRegeoWithoutKeyReturns503(t *testing.T) {
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		t.Error("未配置高德时不应访问任何上游")
	})

	res := get(t, base+"/api/regeo?lat=39.9&lon=116.4")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("应返回 503，实际 %d", res.StatusCode)
	}
	if body := jsonBody(t, res); !strings.Contains(body["error"], "AMAP_KEY") {
		t.Fatalf("提示应说明缺少配置：%v", body)
	}
}

// 参数校验与其它接口保持一致：非法坐标不得打到上游。
func TestRegeoParamValidation(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应访问上游")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应访问上游")
	})

	for _, q := range []string{"", "?lat=39.9", "?lat=abc&lon=116.4", "?lat=95&lon=116.4"} {
		res := get(t, env.base+"/api/regeo"+q)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("query=%q 应返回 400，实际 %d", q, res.StatusCode)
		}
	}
	if n := len(env.amapReqs()); n != 0 {
		t.Fatalf("不应调用高德，实际 %d 次", n)
	}
}

// 定位取名必须先做 GPS→高德坐标转换再逆地理编码：直接喂 WGS-84 会偏约 400 米，
// 实测足以让"天安门"从东城区变成西城区。
func TestRegeoConvertsBeforeLookupAndOmitsCoordinates(t *testing.T) {
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/coordinate/convert"):
			_, _ = io.WriteString(w, amapConverted)
		case strings.HasSuffix(r.URL.Path, "/geocode/regeo"):
			_, _ = io.WriteString(w, amapTiananmen)
		default:
			t.Errorf("非预期的高德路径：%s", r.URL.Path)
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("定位取名不应访问 Open-Meteo")
	})

	res := get(t, env.base+"/api/regeo?lat=39.9076&lon=116.3911")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", res.StatusCode)
	}

	raw, _ := io.ReadAll(res.Body)
	var parsed struct {
		Name     string  `json:"name"`
		Province string  `json:"province"`
		District string  `json:"district"`
		Country  string  `json:"country"`
		Source   string  `json:"source"`
		Latitude float64 `json:"latitude"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if parsed.Name != "北京市" || parsed.District != "东城区" || parsed.Source != "amap" {
		t.Fatalf("取名结果不符：%+v", parsed)
	}
	// 刻意不返回坐标：调用方必须继续使用原始 GPS 坐标（天气接口要求 WGS-84），
	// 暴露转换后的高德坐标只会诱导误用。
	if strings.Contains(string(raw), "latitude") || strings.Contains(string(raw), "longitude") {
		t.Fatalf("响应不应包含坐标：%s", raw)
	}

	reqs := env.amapReqs()
	if len(reqs) != 2 {
		t.Fatalf("应依次调用坐标转换与逆地理编码，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("coordsys"); got != "gps" {
		t.Fatalf("第一步应是 GPS 坐标系转换，实际 coordsys=%q", got)
	}
	if got := reqs[0].URL.Query().Get("locations"); got != "116.391100,39.907600" {
		t.Fatalf("转换入参应为「经度,纬度」，实际 %q", got)
	}
	// 坐标转换实测返回 12 位小数，而逆地理编码要求不超过 6 位
	if got := reqs[1].URL.Query().Get("location"); got != "116.397340,39.909000" {
		t.Fatalf("逆地理编码入参应截断到 6 位小数，实际 %q", got)
	}
}

// 定位取名按量化后的坐标缓存：同一地点重复定位不应反复打高德。
func TestRegeoIsCached(t *testing.T) {
	var calls int32
	env := newDualEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if strings.HasSuffix(r.URL.Path, "/coordinate/convert") {
			_, _ = io.WriteString(w, amapConverted)
			return
		}
		_, _ = io.WriteString(w, amapTiananmen)
	}, func(w http.ResponseWriter, r *http.Request) {})

	target := env.base + "/api/regeo?lat=39.9076&lon=116.3911"
	if got := get(t, target).Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("首次应为 MISS，实际 %q", got)
	}
	if got := get(t, target).Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("二次应为 HIT，实际 %q", got)
	}
	// 一个客户端位置漂移几十米后仍应命中同一条缓存（坐标已量化到 2 位小数）
	if got := get(t, env.base+"/api/regeo?lat=39.9079&lon=116.3915").Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("邻近坐标应命中同一条缓存，实际 %q", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("高德应只被调用 2 次（转换 + 逆地理），实际 %d 次", got)
	}
}

// 错误信息不得泄漏上游地址、协议与堆栈——这是"可执行的"落地方式，
// 而不是一句口号。刻意只断言"地址/协议/堆栈"，不断言厂商名：
// 「请检查 QWEATHER_KEY」这类可操作提示本来就应该出现在错误里。
func TestErrorMessagesDoNotLeakUpstreamURLs(t *testing.T) {
	forbidden := []string{"restapi.amap.com", "api.open-meteo.com", "geocoding-api.open-meteo.com",
		"qweatherapi.com", "http://", "https://", "goroutine"}

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"高德鉴权失败", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"status":"0","info":"INVALID_USER_KEY","infocode":"10001"}`)
		}},
		{"高德返回非 JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDualEnv(t, tc.handler, func(w http.ResponseWriter, r *http.Request) {})
			res := get(t, env.base+"/api/geocode?q="+url.QueryEscape("上海"))
			if res.StatusCode == http.StatusOK {
				t.Fatal("应返回错误响应")
			}
			msg := jsonBody(t, res)["error"]
			for _, bad := range forbidden {
				if strings.Contains(msg, bad) {
					t.Fatalf("错误信息不得包含 %q：%q", bad, msg)
				}
			}
			if strings.Contains(msg, testAMapKey) {
				t.Fatalf("错误信息不得包含 Key：%q", msg)
			}
		})
	}

	// Open-Meteo 路径同样要守这条线
	base, _ := newEnv(t, time.Minute, time.Second, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>not json</html>")
	})
	msg := jsonBody(t, get(t, base+"/api/weather?lat=1&lon=2"))["error"]
	for _, bad := range forbidden {
		if strings.Contains(msg, bad) {
			t.Fatalf("错误信息不得包含 %q：%q", bad, msg)
		}
	}
}
