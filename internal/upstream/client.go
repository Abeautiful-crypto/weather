// Package upstream 封装对 Open-Meteo 三个公开接口的调用，
// 负责超时控制、响应体大小限制与错误分类（映射为对外的 HTTP 状态码）。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxBodyBytes 限制单个上游响应体大小，避免异常响应耗尽内存。
const maxBodyBytes = 4 << 20 // 4 MiB

// Endpoints 保存三个上游接口地址，便于测试时替换为本地假服务。
type Endpoints struct {
	Geocode  string
	Forecast string
	Air      string
}

// DefaultEndpoints 返回 Open-Meteo 的公开接口地址（免 API Key）。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		Geocode:  "https://geocoding-api.open-meteo.com/v1/search",
		Forecast: "https://api.open-meteo.com/v1/forecast",
		Air:      "https://air-quality-api.open-meteo.com/v1/air-quality",
	}
}

// Error 表示一次上游调用失败。
// Status 与 Msg 面向最终用户；err 仅内部保留，不会写入 HTTP 响应。
type Error struct {
	Status int
	Msg    string
	err    error
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("upstream error (status=%d, msg=%q): %v", e.Status, e.Msg, e.err)
	}
	return fmt.Sprintf("upstream error (status=%d, msg=%q)", e.Status, e.Msg)
}

// Unwrap 支持 errors.Is/As 向上追溯底层错误。
func (e *Error) Unwrap() error { return e.err }

// Client 是上游 HTTP 客户端。
type Client struct {
	hc      *http.Client
	eps     Endpoints
	timeout time.Duration
}

// New 创建客户端；timeout <= 0 时使用 8 秒默认值。
func New(timeout time.Duration, eps Endpoints) *Client {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &Client{
		// 主超时由 context 控制，这里的 Timeout 只作为最后兜底。
		hc:      &http.Client{Timeout: timeout + 2*time.Second},
		eps:     eps,
		timeout: timeout,
	}
}

// NewDefault 使用 Open-Meteo 默认地址创建客户端。
func NewDefault(timeout time.Duration) *Client {
	return New(timeout, DefaultEndpoints())
}

// Endpoints 返回当前使用的上游地址。
func (c *Client) Endpoints() Endpoints { return c.eps }

// FetchWithQuery 以 endpoint?query 的形式发起 GET 请求并返回原始响应体。
func (c *Client) FetchWithQuery(ctx context.Context, endpoint string, q url.Values) ([]byte, error) {
	rawURL := endpoint
	if len(q) > 0 {
		rawURL += "?" + q.Encode()
	}
	return c.fetch(ctx, rawURL)
}

// Fetch 直接请求一个完整 URL。
func (c *Client) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	return c.fetch(ctx, rawURL)
}

func (c *Client) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Msg: "构造上游请求失败", err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "weather-go/1.0")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &Error{
			Status: http.StatusBadGateway,
			Msg:    fmt.Sprintf("上游天气服务返回异常（HTTP %d），请稍后重试", res.StatusCode),
			err:    fmt.Errorf("unexpected upstream status %d", res.StatusCode),
		}
	}
	return body, nil
}

// classify 把底层网络错误映射为对用户友好的中文错误与语义化状态码。
func classify(ctx context.Context, err error) error {
	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) ||
		ctx.Err() == context.DeadlineExceeded
	if timedOut {
		return &Error{Status: http.StatusGatewayTimeout, Msg: "上游天气服务响应超时，请稍后重试", err: err}
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return &Error{Status: http.StatusRequestTimeout, Msg: "请求已被取消", err: err}
	}
	return &Error{Status: http.StatusBadGateway, Msg: "无法连接上游天气服务，请检查网络后重试", err: err}
}
