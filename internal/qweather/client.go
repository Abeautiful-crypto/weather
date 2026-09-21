// Package qweather 封装和风天气（QWeather）的 GeoAPI 城市搜索。
//
// 只用于「城市搜索」这一层：中文行政区覆盖比 Open-Meteo（GeoNames）完整，
// 能解决「宿迁」这类在 GeoNames 中文索引里缺失的地名。天气数据仍走 Open-Meteo。
//
// 两点与官方文档一致的约束：
//   - 必须使用账号专属 API Host（控制台 → 设置），公共地址自 2026 年起逐步停服；
//   - 鉴权做了抽象，当前实现为 API Key（X-QW-Api-Key 请求头）；
//     官方已推荐 JWT（Ed25519）且 API Key 自 2027-01-01 起限制每日请求量，
//     届时只需新增一个 Authenticator 实现，不改调用方。
package qweather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBodyBytes 限制响应体大小，避免异常响应耗尽内存。
const maxBodyBytes = 1 << 20 // 1 MiB

// AuthHeaderAPIKey 是 API Key 鉴权使用的请求头名。
// 刻意用请求头而不是 key 查询参数：URL 更容易出现在日志与错误信息里。
const AuthHeaderAPIKey = "X-QW-Api-Key"

// Authenticator 负责给请求附加身份认证信息，便于日后从 API Key 切换到 JWT。
type Authenticator interface {
	Apply(req *http.Request)
}

// APIKeyAuth 使用 X-QW-Api-Key 请求头进行认证。
type APIKeyAuth struct {
	Key string
}

// Apply 实现 Authenticator。
func (a APIKeyAuth) Apply(req *http.Request) {
	req.Header.Set(AuthHeaderAPIKey, a.Key)
}

// Config 是客户端配置。Host 与 APIKey 均必填。
type Config struct {
	// Host 为专属 API Host，可带或不带 https:// 前缀，例如 abcxyz.qweatherapi.com。
	Host string
	// APIKey 为控制台创建的 API Key。
	APIKey string
	// Timeout 为单次请求超时，<=0 时使用 8 秒。
	Timeout time.Duration
	// HTTPClient 可选：注入自定义 HTTP 客户端，用于测试（自签名证书）或走代理的部署。
	// 为 nil 时按 Timeout 创建默认客户端。
	HTTPClient *http.Client
}

// Client 是 GeoAPI 客户端。
type Client struct {
	baseURL string
	auth    Authenticator
	hc      *http.Client
	timeout time.Duration
}

// New 创建客户端。Host 或 APIKey 缺失时返回错误，由调用方决定是否降级到其它数据源。
func New(cfg Config) (*Client, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("缺少 QWEATHER_HOST（和风专属 API Host）")
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("缺少 QWEATHER_KEY（和风 API Key）")
	}
	// 容错：允许用户把 https:// 一起粘过来
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// 主超时由 context 控制，这里的 Timeout 只作为兜底。
		hc = &http.Client{Timeout: timeout + 2*time.Second}
	}
	return &Client{
		baseURL: "https://" + host,
		auth:    APIKeyAuth{Key: key},
		hc:      hc,
		timeout: timeout,
	}, nil
}

// City 是 GeoAPI 返回的一个地点。字段类型与官方一致：经纬度是**字符串**。
type City struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Lat     string `json:"lat"`
	Lon     string `json:"lon"`
	Adm1    string `json:"adm1"`
	Adm2    string `json:"adm2"`
	Country string `json:"country"`
	TZ      string `json:"tz"`
}

type lookupResponse struct {
	Code     string `json:"code"`
	Location []City `json:"location"`
}

// Error 表示一次和风调用失败。Status 与 Msg 面向最终用户；err 仅内部保留。
// 注意：Msg 中不会出现 API Key 或完整请求 URL。
type Error struct {
	Status int
	Msg    string
	err    error
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("qweather error (status=%d, msg=%q): %v", e.Status, e.Msg, e.err)
	}
	return fmt.Sprintf("qweather error (status=%d, msg=%q)", e.Status, e.Msg)
}

// Unwrap 支持 errors.Is/As 向上追溯底层错误。
func (e *Error) Unwrap() error { return e.err }

// LookupCity 按名称搜索城市。关键词为空或查无结果时返回空切片与 nil 错误，
// 由调用方决定展示何种提示。
func (c *Client) LookupCity(ctx context.Context, location string, number int) ([]City, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, nil
	}
	if number <= 0 || number > 20 { // 官方取值范围 1–20
		number = 6
	}

	params := url.Values{}
	params.Set("location", location)
	params.Set("number", strconv.Itoa(number))
	params.Set("lang", "zh")

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	endpoint := c.baseURL + "/geo/v2/city/lookup?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Msg: "构造和风请求失败", err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "weather-go/1.0")
	c.auth.Apply(req)

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}

	var parsed lookupResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// 和风通常在 HTTP 200 的响应体里用 code 表达错误；无法解析时按上游异常处理，
		// 但绝不把原始响应体回显给客户端（可能包含账号相关信息）。
		return nil, &Error{
			Status: http.StatusBadGateway,
			Msg:    "和风天气返回了无法解析的数据，请稍后重试",
			err:    fmt.Errorf("unmarshal response: %w", err),
		}
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, statusError(res.StatusCode, parsed.Code)
	}
	switch parsed.Code {
	case "200":
		return parsed.Location, nil
	case "204", "404":
		// 无数据 / 查无此地：属于"搜不到"，不是故障。
		return nil, nil
	case "":
		return parsed.Location, nil
	default:
		status, _ := strconv.Atoi(parsed.Code)
		return nil, statusError(status, parsed.Code)
	}
}

// statusError 把 HTTP 状态或和风业务码映射为对用户可操作的中文提示。
func statusError(httpStatus int, bizCode string) error {
	code := bizCode
	if code == "" {
		code = strconv.Itoa(httpStatus)
	}
	switch code {
	case "401":
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    "和风天气鉴权失败，请检查 QWEATHER_KEY 与 QWEATHER_HOST 是否与控制台一致",
			err:    fmt.Errorf("qweather code 401 (http %d)", httpStatus),
		}
	case "402":
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    "和风天气本月免费额度已用尽，请到控制台查看用量或稍后再试",
			err:    fmt.Errorf("qweather code 402 (http %d)", httpStatus),
		}
	case "403":
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    "和风天气拒绝访问（无权限），请确认该凭据已授权 GeoAPI",
			err:    fmt.Errorf("qweather code 403 (http %d)", httpStatus),
		}
	default:
		return &Error{
			Status: http.StatusBadGateway,
			Msg:    fmt.Sprintf("和风天气返回异常（%s），请稍后重试", code),
			err:    fmt.Errorf("qweather unexpected code %s (http %d)", code, httpStatus),
		}
	}
}

// classifyTransport 处理连接层错误（超时、不可达）。
func classifyTransport(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return &Error{Status: http.StatusGatewayTimeout, Msg: "和风天气响应超时，请稍后重试", err: err}
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return &Error{Status: http.StatusRequestTimeout, Msg: "请求已被取消", err: err}
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Status: http.StatusGatewayTimeout, Msg: "和风天气响应超时，请稍后重试", err: err}
	}
	return &Error{Status: http.StatusBadGateway, Msg: "无法连接和风天气，请检查网络后重试", err: err}
}
