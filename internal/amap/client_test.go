package amap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey 是**假 Key**：形态与真实高德 Key 一致（32 位十六进制）即可，
// 便于断言"绝不出现在错误信息里"。真实 Key 只允许经环境变量 AMAP_KEY 传入，
// 不得写入任何文件。
const testKey = "0123456789abcdef0123456789abcdef"

// 以下夹具直接取自实测响应（含最容易踩的字段形态），不是按文档手写的。
const (
	// 实测：`朝阳` 返回 3 条；朝阳市的 district 是**空数组**，北京朝阳区的 district 是字符串。
	chaoyangGeo = `{"status":"1","info":"OK","infocode":"10000","count":"3","geocodes":[
		{"formatted_address":"辽宁省朝阳市","country":"中国","province":"辽宁省","citycode":"0421","city":"朝阳市",
		 "district":[],"township":[],"street":[],"number":[],"adcode":"211300","location":"120.488801,41.601855","level":"市"},
		{"formatted_address":"北京市朝阳区","country":"中国","province":"北京市","citycode":"010","city":"北京市",
		 "district":"朝阳区","township":[],"street":[],"number":[],"adcode":"110105","location":"116.443136,39.921444","level":"区县"}]}`

	// 实测：用转换后的高德坐标反查天安门，city 是**空数组**（直辖市），须用 province 兜底。
	tiananmenRegeo = `{"status":"1","info":"OK","infocode":"10000","regeocode":{"addressComponent":{
		"city":[],"province":"北京市","adcode":"110101","district":"东城区","towncode":"110101001000",
		"country":"中国","township":"东华门街道","citycode":"010"},"formatted_address":"北京市东城区东华门街道天安门"}}`

	// 实测：坐标转换返回 12 位小数，且 info 是**小写 ok**（地理编码是 OK）。
	convertResult = `{"status":"1","info":"ok","infocode":"10000","locations":"116.397340223525,39.909000379775"}`

	// 实测（东京坐标 139.69171,35.6895）：境外坐标会返回 status=1 / infocode=10000，
	// 但**所有地址字段都是空数组**——连文档里声明为字符串的 formatted_address 也是
	// 空数组。这是"没有对应地区数据"，不是请求失败。
	regeoEmptyOverseas = `{"status":"1","info":"OK","infocode":"10000","regeocode":{"formatted_address":[],"addressComponent":{"country":[],"province":[],"city":[],"citycode":[],"district":[],"adcode":[],"township":[],"towncode":[],"streetNumber":{"street":[],"number":[],"location":"139.69171,35.6895","direction":[],"distance":[]}},"pois":[],"roads":[],"roadinters":[],"aois":[]}}`
)

// newTestClient 启动假高德服务并返回客户端与请求记录器。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, func() []*http.Request) {
	t.Helper()

	var mu sync.Mutex
	var reqs []*http.Request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		// 记录副本：请求对象在 handler 返回后不再可靠。
		reqs = append(reqs, r.Clone(r.Context()))
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(ts.Close)

	c, err := New(Config{
		Key:        testKey,
		Timeout:    time.Second,
		HTTPClient: ts.Client(),
		Endpoints: Endpoints{
			Geocode: ts.URL + "/v3/geocode/geo",
			Regeo:   ts.URL + "/v3/geocode/regeo",
			Convert: ts.URL + "/v3/assistant/coordinate/convert",
		},
	})
	if err != nil {
		t.Fatalf("创建高德客户端失败：%v", err)
	}
	return c, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]*http.Request(nil), reqs...)
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("缺少 Key 时应返回错误")
	}
	if _, err := New(Config{Key: "   "}); err == nil {
		t.Fatal("空白 Key 应视为缺失")
	}
}

// 地理编码必须把「经度,纬度」解析成项目内的 lat/lon（顺序相反是最容易出错的一步），
// 并把空数组字段降级为空字符串而不是让解析失败。
func TestGeocodeParsesRealResponse(t *testing.T) {
	c, seen := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, chaoyangGeo)
	})

	places, err := c.Geocode(context.Background(), "朝阳")
	if err != nil {
		t.Fatalf("地理编码失败：%v", err)
	}
	if len(places) != 2 {
		t.Fatalf("应解析出 2 条结果，实际 %d 条", len(places))
	}

	first := places[0]
	if first.Name != "辽宁省朝阳市" || first.Province != "辽宁省" || first.City != "朝阳市" {
		t.Fatalf("行政区字段映射不符：%+v", first)
	}
	if first.District != "" {
		t.Fatalf("空数组 district 应降级为空字符串，实际 %q", first.District)
	}
	if first.Adcode != "211300" || first.Level != "市" {
		t.Fatalf("adcode/level 映射不符：%+v", first)
	}
	// location 是 "120.488801,41.601855"（经度在前）
	if first.Latitude != 41.601855 || first.Longitude != 120.488801 {
		t.Fatalf("经纬度顺序解析错误：lat=%v lon=%v", first.Latitude, first.Longitude)
	}

	second := places[1]
	if second.District != "朝阳区" || second.Latitude != 39.921444 || second.Longitude != 116.443136 {
		t.Fatalf("字符串形态 district 与坐标解析不符：%+v", second)
	}

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("应只调用一次上游，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("address"); got != "朝阳" {
		t.Fatalf("address 参数应为 朝阳，实际 %q", got)
	}
	// 高德的鉴权只能走查询参数（与和风的请求头方式不同），这里确认我们照做了。
	if got := reqs[0].URL.Query().Get("key"); got != testKey {
		t.Fatalf("key 参数应为测试 Key，实际 %q", got)
	}
}

// 坐标不可解析或越界的条目必须丢弃，不能让脏数据进前端。
func TestGeocodeDropsEntriesWithoutUsableLocation(t *testing.T) {
	const body = `{"status":"1","infocode":"10000","geocodes":[
		{"formatted_address":"缺坐标","location":[]},
		{"formatted_address":"非数字","location":"abc,def"},
		{"formatted_address":"越界","location":"200.0,95.0"},
		{"formatted_address":"正常","location":"116.4,39.9"}]}`

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	places, err := c.Geocode(context.Background(), "测试")
	if err != nil {
		t.Fatalf("地理编码失败：%v", err)
	}
	if len(places) != 1 || places[0].Name != "正常" {
		t.Fatalf("只应保留坐标可用的条目，实际 %+v", places)
	}
}

func TestGeocodeEmptyAddressDoesNotCallUpstream(t *testing.T) {
	c, seen := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("空关键词不应访问上游")
	})
	places, err := c.Geocode(context.Background(), "   ")
	if err != nil || places != nil {
		t.Fatalf("空关键词应返回空结果与 nil 错误，实际 places=%v err=%v", places, err)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("不应调用上游，实际 %d 次", n)
	}
}

// 直辖市的 city 恒为空数组，城市名必须回落到 province，否则定位后的城市名是空的。
func TestRegeoFallsBackToProvinceWhenCityEmpty(t *testing.T) {
	c, seen := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, tiananmenRegeo)
	})

	place, err := c.Regeo(context.Background(), 39.909, 116.39734)
	if err != nil {
		t.Fatalf("逆地理编码失败：%v", err)
	}
	if place.Name != "北京市" {
		t.Fatalf("city 为空时应回落 province，实际 %q", place.Name)
	}
	if place.District != "东城区" || place.Province != "北京市" || place.City != "" {
		t.Fatalf("字段映射不符：%+v", place)
	}
	if place.Latitude != 39.909 || place.Longitude != 116.39734 {
		t.Fatalf("应原样回传入参坐标，实际 %v / %v", place.Latitude, place.Longitude)
	}

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("应只调用一次上游，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("location"); got != "116.397340,39.909000" {
		t.Fatalf("location 应为「经度,纬度」且不超过 6 位小数，实际 %q", got)
	}
}

// 坐标转换接口实测返回 12 位小数，而逆地理编码要求不超过 6 位；
// 再把它用于 regeo 时必须由本包截断，否则就是把超出契约的值发出去。
func TestConvertedCoordinatesAreTruncatedBeforeReuse(t *testing.T) {
	c, seen := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/coordinate/convert") {
			_, _ = io.WriteString(w, convertResult)
			return
		}
		_, _ = io.WriteString(w, tiananmenRegeo)
	})

	lat, lon, err := c.GPSToAMap(context.Background(), 39.9076, 116.3911)
	if err != nil {
		t.Fatalf("坐标转换失败：%v", err)
	}
	if lat != 39.909000379775 || lon != 116.397340223525 {
		t.Fatalf("转换结果解析不符：lat=%v lon=%v", lat, lon)
	}

	if _, err := c.Regeo(context.Background(), lat, lon); err != nil {
		t.Fatalf("逆地理编码失败：%v", err)
	}

	reqs := seen()
	if len(reqs) != 2 {
		t.Fatalf("应依次调用转换与逆地理编码，实际 %d 次", len(reqs))
	}
	if got := reqs[0].URL.Query().Get("coordsys"); got != "gps" {
		t.Fatalf("coordsys 应为 gps，实际 %q", got)
	}
	if got := reqs[0].URL.Query().Get("locations"); got != "116.391100,39.907600" {
		t.Fatalf("转换入参应为「经度,纬度」，实际 %q", got)
	}
	if got := reqs[1].URL.Query().Get("location"); got != "116.397340,39.909000" {
		t.Fatalf("再传给逆地理编码前必须截断到 6 位小数，实际 %q", got)
	}
}

// 高德业务失败时同样返回 HTTP 200，必须按 infocode 映射成可操作的中文提示。
func TestStatusErrorsAreActionable(t *testing.T) {
	cases := []struct {
		infocode string
		want     string
	}{
		{"10001", "AMAP_KEY"},
		{"10002", "Web 服务"},
		{"10003", "超限"},
		{"10005", "白名单"},
		{"10009", "平台不匹配"},
		{"10013", "重新创建"},
	}
	for _, tc := range cases {
		t.Run(tc.infocode, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				// 业务失败也可能带 HTTP 200，故意两种都不写状态码。
				_, _ = io.WriteString(w, `{"status":"0","info":"INVALID_USER_KEY","infocode":"`+tc.infocode+`"}`)
			})

			_, err := c.Geocode(context.Background(), "上海")
			if err == nil {
				t.Fatal("业务失败应返回错误")
			}
			var amErr *Error
			if !errors.As(err, &amErr) {
				t.Fatalf("错误类型应为 *Error，实际 %T", err)
			}
			if amErr.Status != http.StatusBadGateway {
				t.Fatalf("应映射为 502，实际 %d", amErr.Status)
			}
			if !strings.Contains(amErr.Msg, tc.want) {
				t.Fatalf("提示应包含 %q，实际 %q", tc.want, amErr.Msg)
			}
			if strings.Contains(amErr.Msg, testKey) {
				t.Fatalf("提示不得包含 Key：%q", amErr.Msg)
			}
			if strings.Contains(amErr.Msg, "restapi.amap.com") {
				t.Fatalf("提示不得泄漏上游地址：%q", amErr.Msg)
			}
		})
	}
}

// 高德的 Key 只能放在 URL 查询参数里，而 net/http 的连接层错误会把完整 URL
// 带进 err.Error()。若不脱敏，明文 Key 会随日志落地。
func TestTransportErrorDoesNotLeakKey(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 立刻关闭，制造连接失败

	c, err := New(Config{Key: testKey, Timeout: time.Second, Endpoints: Endpoints{
		Geocode: deadURL + "/v3/geocode/geo",
		Regeo:   deadURL + "/v3/geocode/regeo",
		Convert: deadURL + "/v3/assistant/coordinate/convert",
	}})
	if err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	_, callErr := c.Geocode(context.Background(), "上海")
	if callErr == nil {
		t.Fatal("上游不可达应返回错误")
	}
	var amErr *Error
	if !errors.As(callErr, &amErr) {
		t.Fatalf("错误类型应为 *Error，实际 %T", callErr)
	}
	if amErr.Status != http.StatusBadGateway {
		t.Fatalf("上游不可达应映射为 502，实际 %d", amErr.Status)
	}

	// Error() 是日志里实际会被打印的字符串，这是泄漏的真正出口。
	if strings.Contains(amErr.Error(), testKey) {
		t.Fatalf("错误字符串不得包含 Key：%s", amErr.Error())
	}
	if strings.Contains(amErr.Msg, testKey) {
		t.Fatalf("用户可见提示不得包含 Key：%s", amErr.Msg)
	}
}

// 实测：无法解析的输入返回 status=0 + infocode=30001（ENGINE_RESPONSE_DATA_ERROR），
// 而不是空结果数组。这属于"查无此地"，不是上游故障——若按故障处理，用户输入一个
// 不存在的城市会看到「高德返回异常」，而且回落链会被错误地中断。
func TestUnresolvableAddressIsNoResultNotFailure(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"0","info":"ENGINE_RESPONSE_DATA_ERROR","infocode":"30001"}`)
	})

	_, err := c.Geocode(context.Background(), "不存在的城市名")
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("应返回 ErrNoResult，实际 %v", err)
	}
	var amErr *Error
	if errors.As(err, &amErr) {
		t.Fatalf("查无此地不应被当成调用失败（会被映射成 502）：%v", amErr)
	}
}

// 逆地理编码同样可能拿到 30001（例如坐标在境外或海上），语义同样是"没有对应地区"。
func TestRegeoNoResultIsNotFailure(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"0","info":"ENGINE_RESPONSE_DATA_ERROR","infocode":"30001"}`)
	})

	if _, err := c.Regeo(context.Background(), 35.68, 139.69); !errors.Is(err, ErrNoResult) {
		t.Fatalf("应返回 ErrNoResult，实际 %v", err)
	}
}

// 境外坐标是另一种"没有数据"的形态：请求成功（status=1 / infocode=10000），
// 但所有地址字段都是空数组。两个坑叠在一起——既要把空数组解析成空字符串，
// 又要把"全空"判定为 ErrNoResult；否则用户看到的是"高德返回了无法解析的数据"。
func TestRegeoOverseasCoordinateIsNoResult(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, regeoEmptyOverseas)
	})

	_, err := c.Regeo(context.Background(), 35.6895, 139.69171)
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("境外坐标应返回 ErrNoResult，实际 %v", err)
	}
	var amErr *Error
	if errors.As(err, &amErr) {
		t.Fatalf("「没有对应地区数据」不应被当成解析失败：%v", amErr)
	}
}

// 境内坐标必须仍能正常解析——上一条修复不能把正常路径一起吞掉。
func TestRegeoDomesticCoordinateStillResolves(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, tiananmenRegeo)
	})

	place, err := c.Regeo(context.Background(), 39.909, 116.39734)
	if err != nil {
		t.Fatalf("境内坐标不应报错：%v", err)
	}
	if place.Name != "北京市" || place.District != "东城区" {
		t.Fatalf("境内结果不符：%+v", place)
	}
}

func TestTimeoutMapsTo504(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, chaoyangGeo)
	})
	// 用一个更短的超时客户端替换默认值。
	short, err := New(Config{Key: testKey, Timeout: 50 * time.Millisecond, HTTPClient: c.hc, Endpoints: c.eps})
	if err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}

	_, callErr := short.Geocode(context.Background(), "上海")
	var amErr *Error
	if !errors.As(callErr, &amErr) {
		t.Fatalf("应返回 *Error，实际 %T", callErr)
	}
	if amErr.Status != http.StatusGatewayTimeout {
		t.Fatalf("超时应映射为 504，实际 %d", amErr.Status)
	}
	if !strings.Contains(amErr.Msg, "超时") {
		t.Fatalf("提示应说明超时：%q", amErr.Msg)
	}
}

func TestNonJSONMapsTo502(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	})

	_, err := c.Geocode(context.Background(), "上海")
	var amErr *Error
	if !errors.As(err, &amErr) {
		t.Fatalf("应返回 *Error，实际 %T", err)
	}
	if amErr.Status != http.StatusBadGateway {
		t.Fatalf("非 JSON 应映射为 502，实际 %d", amErr.Status)
	}
	if strings.Contains(amErr.Msg, "Bad Gateway") {
		t.Fatalf("不应回显上游原始响应体：%q", amErr.Msg)
	}
}

// 空白字段的宽容降级不能掩盖真正的结构变化：无法识别的形态按空串处理，
// 但整体仍应成功返回（避免一次字段变化把搜索变成 502）。
func TestUnexpectedFieldShapeDegradesInsteadOfFailing(t *testing.T) {
	const body = `{"status":"1","infocode":"10000","geocodes":[
		{"formatted_address":"形状异常","province":{"nested":1},"city":[],"district":[],"location":"116.4,39.9","level":123}]}`

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	places, err := c.Geocode(context.Background(), "测试")
	if err != nil {
		t.Fatalf("字段形态异常不应导致整体失败：%v", err)
	}
	if len(places) != 1 {
		t.Fatalf("应保留坐标可用的条目，实际 %d 条", len(places))
	}
	if places[0].Province != "" || places[0].City != "" || places[0].Level != "" {
		t.Fatalf("异常形态应降级为空串：%+v", places[0])
	}
}
