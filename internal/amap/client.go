// Package amap 封装高德地图 Web 服务 API 中与「地点解析」相关的三个接口：
// 地理编码（地址 → 坐标）、逆地理编码（坐标 → 行政区）、坐标转换（GPS → 高德坐标）。
//
// 只服务于「城市搜索」与「定位取名」两层；天气数据仍走 Open-Meteo。
//
// 下列约束全部来自实测响应，不是文档推断，映射层必须遵守：
//   - 可空文本字段「有值时是字符串、无值时是空数组」（实测 "district":[]），
//     因此统一用 flexString 解析；直接声明为 string 会在遇到空值时反序列化失败；
//   - 直辖市的 city 恒为空（实测北京、上海），城市名需用 province 兜底；
//   - level 对直辖市返回「省」（实测上海 level="省"），不是「市」；
//   - location 是「经度,纬度」，与项目内 lat/lon 的顺序相反；
//   - 坐标转换接口的 info 是小写 "ok"（地理编码是 "OK"），因此判断成功只能看
//     status/infocode；它还会返回 12 位小数，而逆地理编码要求不超过 6 位，
//     所以再用该坐标发起请求时由 formatLonLat 统一截断到 6 位。
//
// 关于密钥泄漏：高德的鉴权只能通过 URL 查询参数 key（没有可用的请求头鉴权），
// 而 *url.Error 的字符串里会带上完整 URL。若原样写入日志，明文 Key 就会落进
// 日志文件。因此本包在构造错误时统一做脱敏（见 redactErr），调用方拿到的
// err 里不会出现 Key。
package amap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxBodyBytes 限制响应体大小，避免异常响应耗尽内存。
	maxBodyBytes = 1 << 20 // 1 MiB
	// statusOK 是高德在业务成功时返回的 status 字段值。
	statusOK = "1"
	// coordPrecision 逆地理编码文档要求经纬度小数点后不超过 6 位。
	coordPrecision = 6
	// redacted 是错误信息中替换 API Key 的占位符。
	redacted = "***"
	// infocodeNoResult 是高德表达"解析不出这个输入 / 没有对应地区数据"的 infocode。
	infocodeNoResult = "30001"
)

// Endpoints 保存三个接口地址，便于测试时替换为本地假服务。
type Endpoints struct {
	Geocode string
	Regeo   string
	Convert string
}

// DefaultEndpoints 返回高德 Web 服务 API 的公开接口地址。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		Geocode: "https://restapi.amap.com/v3/geocode/geo",
		Regeo:   "https://restapi.amap.com/v3/geocode/regeo",
		Convert: "https://restapi.amap.com/v3/assistant/coordinate/convert",
	}
}

// Config 是客户端配置。Key 必填。
type Config struct {
	// Key 为控制台创建的「Web 服务」类型 Key。
	Key string
	// Timeout 为单次请求超时，<=0 时使用 8 秒。
	Timeout time.Duration
	// HTTPClient 可选：注入自定义 HTTP 客户端，用于测试或走代理的部署。
	HTTPClient *http.Client
	// Endpoints 可选：留空的字段用默认地址补齐。
	Endpoints Endpoints
}

// Client 是高德 Web 服务 API 客户端。
type Client struct {
	key     string
	hc      *http.Client
	timeout time.Duration
	eps     Endpoints
}

// New 创建客户端。Key 为空时返回错误，由调用方决定是否降级到其它数据源。
func New(cfg Config) (*Client, error) {
	key := strings.TrimSpace(cfg.Key)
	if key == "" {
		return nil, errors.New("缺少 AMAP_KEY（高德 Web 服务 Key）")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// 主超时由 context 控制，这里的 Timeout 只作为兜底。
		hc = &http.Client{Timeout: timeout + 2*time.Second}
	}

	eps := cfg.Endpoints
	def := DefaultEndpoints()
	if eps.Geocode == "" {
		eps.Geocode = def.Geocode
	}
	if eps.Regeo == "" {
		eps.Regeo = def.Regeo
	}
	if eps.Convert == "" {
		eps.Convert = def.Convert
	}

	return &Client{key: key, hc: hc, timeout: timeout, eps: eps}, nil
}

// Place 是一条归一化后的地点：坐标已从「经度,纬度」字符串解析为数字。
type Place struct {
	// Name 是展示名（formatted_address），形如「辽宁省朝阳市」「上海市」。
	Name string
	// Country 是国内结果固定返回的「中国」。
	Country string
	// Province / City / District 为行政区三级，任意一级都可能为空
	// （实测：直辖市的 City 恒空，区县之外的匹配 District 为空）。
	Province string
	City     string
	District string
	// Adcode 是六位行政区编码，比名称更稳定，可作为身份标识。
	Adcode string
	// Level 保留高德的匹配级别原文（省/市/区县/乡镇/村庄/门牌号…），
	// 不在此处归一化：归一化词汇表由 api 层与 GeoNames 的级别放在一起维护。
	Level     string
	Latitude  float64
	Longitude float64
}

// Error 表示一次高德调用失败。Status 与 Msg 面向最终用户；
// err 仅内部保留，已在构造时完成脱敏，不会包含 API Key。
type Error struct {
	Status int
	Msg    string
	err    error
}

// ErrNoResult 表示高德明确"没有匹配到地名 / 没有对应地区数据"，而不是调用失败。
//
// 实测：解析不出来的输入（zzqqxx、不存在的城市名）与境外坐标不会返回空结果数组，
// 而是返回 status=0 且 infocode=30001（ENGINE_RESPONSE_DATA_ERROR）。把这种情形
// 当成故障会同时坏掉两件事：用户输入一个不存在的城市会看到"高德返回异常"，
// 而且回落链会被错误地中断（错误按约定不回落）。
//
// 调用方应当用 errors.Is(err, ErrNoResult) 区分：
//   - 查到：正常结果；
//   - ErrNoResult：换个关键词或继续尝试其它数据源；
//   - 其它 error：真正的故障，报给用户。
var ErrNoResult = errors.New("amap: 未匹配到地名")

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("amap error (status=%d, msg=%q): %v", e.Status, e.Msg, e.err)
	}
	return fmt.Sprintf("amap error (status=%d, msg=%q)", e.Status, e.Msg)
}

// Unwrap 支持 errors.Is/As 向上追溯底层错误。
func (e *Error) Unwrap() error { return e.err }

// flexString 兼容高德返回的两种字段形态：有值时是字符串，无值时是空数组。
//
// 实测样例：朝阳市的 "district":[]、逆地理编码北京/上海的 "city":[]。
// 遇到无法识别的形态一律降级为空字符串而不是报错——字段形态变化只应让某个
// 展示字段变空（再由 province 兜底），不该把整次搜索变成 502。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0, string(b) == "null":
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			*f = ""
			return nil
		}
		*f = flexString(s)
	case b[0] == '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(b, &arr); err != nil || len(arr) == 0 {
			*f = ""
			return nil
		}
		var s string
		if err := json.Unmarshal(arr[0], &s); err != nil {
			*f = ""
			return nil
		}
		*f = flexString(s)
	default:
		*f = ""
	}
	return nil
}

func (f flexString) String() string { return string(f) }

// envelope 是三个接口共有的状态字段。高德在业务失败时同样返回 HTTP 200，
// 因此成功与否必须看 status/infocode，不能只看 HTTP 状态码。
type envelope struct {
	Status   string `json:"status"`
	Info     string `json:"info"`
	Infocode string `json:"infocode"`
}

type geoItem struct {
	FormattedAddress flexString `json:"formatted_address"`
	Country          flexString `json:"country"`
	Province         flexString `json:"province"`
	City             flexString `json:"city"`
	District         flexString `json:"district"`
	Adcode           flexString `json:"adcode"`
	Level            flexString `json:"level"`
	Location         flexString `json:"location"`
}

type geoResponse struct {
	envelope
	Count    string    `json:"count"`
	Geocodes []geoItem `json:"geocodes"`
}

type regeoResponse struct {
	envelope
	Regeocode struct {
		// 文档把它列在字符串字段里，但实测境外坐标返回的是**空数组**：
		// 声明成 string 会让整次解析失败（"无法解析的数据"），把一个"无人区"
		// 报成上游故障。可空文本字段一律用 flexString。
		FormattedAddress flexString `json:"formatted_address"`
		AddressComponent struct {
			Country  flexString `json:"country"`
			Province flexString `json:"province"`
			City     flexString `json:"city"`
			District flexString `json:"district"`
			Adcode   flexString `json:"adcode"`
			Township flexString `json:"township"`
		} `json:"addressComponent"`
	} `json:"regeocode"`
}

type convertResponse struct {
	envelope
	Locations flexString `json:"locations"`
}

// Geocode 把结构化地址/地名解析为坐标。关键词为空或查无结果时返回空切片与
// nil 错误，由调用方决定展示何种提示。
func (c *Client) Geocode(ctx context.Context, address string) ([]Place, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, nil
	}

	params := url.Values{}
	params.Set("address", address)

	var resp geoResponse
	if err := c.call(ctx, c.eps.Geocode, params, &resp); err != nil {
		return nil, err
	}

	places := make([]Place, 0, len(resp.Geocodes))
	for _, it := range resp.Geocodes {
		lat, lon, ok := parseLonLat(it.Location)
		if !ok {
			// 坐标缺失或越界的条目直接丢弃，不让脏数据进前端。
			continue
		}
		places = append(places, Place{
			Name:      it.FormattedAddress.String(),
			Country:   it.Country.String(),
			Province:  it.Province.String(),
			City:      it.City.String(),
			District:  it.District.String(),
			Adcode:    it.Adcode.String(),
			Level:     it.Level.String(),
			Latitude:  lat,
			Longitude: lon,
		})
	}
	return places, nil
}

// Regeo 反查坐标所在的行政区。传入坐标必须是高德坐标系（GCJ-02）；
// 若手上只有 GPS 的 WGS-84 坐标，先用 GPSToAMap 转换，否则会偏约 400 米，
// 实测足以让"天安门"从东城区变成西城区。
func (c *Client) Regeo(ctx context.Context, lat, lon float64) (Place, error) {
	params := url.Values{}
	params.Set("location", formatLonLat(lon, lat))

	var resp regeoResponse
	if err := c.call(ctx, c.eps.Regeo, params, &resp); err != nil {
		return Place{}, err
	}

	comp := resp.Regeocode.AddressComponent
	// 直辖市（city 为空数组）用 province 兜底，否则北京会显示成空城市名。
	name := comp.City.String()
	if name == "" {
		name = comp.Province.String()
	}
	if name == "" {
		name = comp.District.String()
	}
	if name == "" {
		name = resp.Regeocode.FormattedAddress.String()
	}
	// 实测：境外/海上坐标会返回 status=1 且所有地址字段都是空数组。请求本身是成功的，
	// 但高德就是在如实回答"这个点没有对应的地区数据"——必须与解析失败区分开，
	// 否则用户看到的是"上游故障"而不是"这个位置识别不了"。
	if name == "" {
		return Place{}, ErrNoResult
	}

	// 逆地理编码不返回查询点坐标，这里原样回传入参（调用方拿到的仍是自己给的
	// 高德坐标）。Level 留空：该接口没有"匹配级别"的概念，街道名也不属于级别。
	return Place{
		Name:      name,
		Country:   comp.Country.String(),
		Province:  comp.Province.String(),
		City:      comp.City.String(),
		District:  comp.District.String(),
		Adcode:    comp.Adcode.String(),
		Latitude:  lat,
		Longitude: lon,
	}, nil
}

// GPSToAMap 把 WGS-84（GPS/浏览器定位）坐标转换为高德坐标（GCJ-02）。
// 逆地理编码与高德的其它接口都要求高德坐标系，直接喂 WGS-84 会偏移数百米。
func (c *Client) GPSToAMap(ctx context.Context, lat, lon float64) (outLat, outLon float64, err error) {
	params := url.Values{}
	params.Set("locations", formatLonLat(lon, lat))
	params.Set("coordsys", "gps")

	var resp convertResponse
	if err := c.call(ctx, c.eps.Convert, params, &resp); err != nil {
		return 0, 0, err
	}

	outLat, outLon, ok := parseLonLat(resp.Locations)
	if !ok {
		return 0, 0, &Error{
			Status: http.StatusBadGateway,
			Msg:    "高德返回了无法解析的坐标，请稍后重试",
			err:    errors.New("unparsable converted location"),
		}
	}
	return outLat, outLon, nil
}

// call 执行一次 GET 并校验高德的业务状态字段。
func (c *Client) call(ctx context.Context, endpoint string, params url.Values, out any) error {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	// 高德的鉴权只能走查询参数；这一点决定了错误信息必须脱敏。
	q.Set("key", c.key)
	rawURL := endpoint + "?" + q.Encode()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &Error{Status: http.StatusInternalServerError, Msg: "构造高德请求失败", err: c.redactErr(err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "weather-go/1.0")

	res, err := c.hc.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return c.transportError(ctx, err)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    "高德返回了无法解析的数据，请稍后重试",
			err:    c.redactErr(fmt.Errorf("unmarshal envelope: %w", err)),
		}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 || env.Status != statusOK {
		return c.statusError(res.StatusCode, env)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    "高德返回了无法解析的数据，请稍后重试",
			err:    c.redactErr(fmt.Errorf("unmarshal body: %w", err)),
		}
	}
	return nil
}

// statusError 把高德的 infocode 映射为可操作的中文提示。
// 刻意不回显 info 原文与请求 URL：前者无助于用户，后者含 API Key。
func (c *Client) statusError(httpStatus int, env envelope) error {
	// 30001 ENGINE_RESPONSE_DATA_ERROR 在高德的实际语义是"这个输入解析不出来"
	// （实测：zzqqxx / 不存在的城市名 / 境外坐标），属于查无此地而非故障。
	// 刻意不包成 *Error：调用方要靠 errors.Is(err, ErrNoResult) 判断。
	if env.Infocode == infocodeNoResult {
		return ErrNoResult
	}

	internalErr := fmt.Errorf("amap infocode=%q (http %d)", env.Infocode, httpStatus)

	var msg string
	switch env.Infocode {
	case "10001":
		msg = "高德 Key 无效或已过期，请检查 AMAP_KEY（必要时到控制台重置后更新）"
	case "10002", "10012":
		msg = "高德 Key 无权调用该服务，请确认 Key 的服务平台选的是「Web 服务」且已开通地理编码"
	case "10003":
		msg = "高德日调用量已超限，将于次日 0:00 自动恢复"
	case "10004":
		msg = "高德提示访问过于频繁，请稍后重试"
	case "10005":
		msg = "高德提示请求 IP 不在白名单内，请把本机出口 IP 加入控制台白名单或关闭白名单"
	case "10007":
		msg = "高德数字签名校验失败，请勿在控制台开启数字签名（本项目未实现 sig）"
	case "10009":
		msg = "高德 Key 与调用平台不匹配，请改用「Web 服务」类型的 Key"
	case "10010":
		msg = "高德提示单 IP 请求次数超限（此状态不会自动恢复），需到控制台提工单解除"
	case "10013":
		msg = "高德 Key 已被删除，请在控制台重新创建并更新 AMAP_KEY"
	case "10019", "10020", "10021":
		msg = "高德并发（QPS）超限，请稍后重试"
	case "10026":
		msg = "高德账号处于封禁状态，如有疑问请到控制台提工单"
	case "40000":
		msg = "高德账号余额已耗尽，无法继续调用"
	case "40002":
		msg = "高德购买的服务已到期"
	default:
		if env.Infocode == "" {
			msg = fmt.Sprintf("高德返回异常（HTTP %d），请稍后重试", httpStatus)
		} else {
			msg = fmt.Sprintf("高德返回异常（%s），请稍后重试", env.Infocode)
		}
	}

	return &Error{Status: http.StatusBadGateway, Msg: msg, err: internalErr}
}

// transportError 处理连接层错误（超时、不可达）。
func (c *Client) transportError(ctx context.Context, err error) error {
	redactedErr := c.redactErr(err)

	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) ||
		ctx.Err() == context.DeadlineExceeded
	if timedOut {
		return &Error{Status: http.StatusGatewayTimeout, Msg: "高德响应超时，请稍后重试", err: redactedErr}
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return &Error{Status: http.StatusRequestTimeout, Msg: "请求已被取消", err: redactedErr}
	}
	return &Error{Status: http.StatusBadGateway, Msg: "无法连接高德服务，请检查网络后重试", err: redactedErr}
}

// redactErr 抹掉错误信息中的 API Key。
//
// net/http 在连接失败与重定向失败时返回 *url.Error，其字符串包含完整请求 URL，
// 而高德的 Key 只能放在 URL 里。不做这一步，日志里就会出现明文 Key。
func (c *Client) redactErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.key, redacted))
}

// formatLonLat 输出高德要求的「经度,纬度」，并按文档截断到 6 位小数
// （坐标转换接口实测会返回 12 位，直接回传逆地理编码会超出其要求）。
func formatLonLat(lon, lat float64) string {
	return strconv.FormatFloat(lon, 'f', coordPrecision, 64) + "," +
		strconv.FormatFloat(lat, 'f', coordPrecision, 64)
}

// parseLonLat 解析高德的「经度,纬度」字符串。顺序与项目内 lat/lon 相反，
// 这里一次性做对并顺带校验范围，避免把互换后的坐标带进下游。
func parseLonLat(s flexString) (lat, lon float64, ok bool) {
	parts := strings.Split(strings.TrimSpace(string(s)), ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	lat, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}
