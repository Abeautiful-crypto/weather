package qweather

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, timeout time.Duration) *Client {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)

	c, err := New(Config{
		Host:       strings.TrimPrefix(ts.URL, "https://"),
		APIKey:     "k",
		Timeout:    timeout,
		HTTPClient: ts.Client(),
	})
	if err != nil {
		t.Fatalf("创建客户端失败：%v", err)
	}
	return c
}

func TestNewRequiresHostAndKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"缺 Host", Config{APIKey: "k"}, "QWEATHER_HOST"},
		{"缺 APIKey", Config{Host: "abc.qweatherapi.com"}, "QWEATHER_KEY"},
		{"都缺", Config{}, "QWEATHER_HOST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应提到 %s，实际 %v", tc.want, err)
			}
		})
	}
}

// 用户常把控制台里的 Host 连 https:// 与结尾斜杠一起粘过来，必须容错。
func TestHostNormalization(t *testing.T) {
	var gotHost string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		_, _ = io.WriteString(w, `{"code":"200","location":[]}`)
	}))
	t.Cleanup(ts.Close)

	host := strings.TrimPrefix(ts.URL, "https://")
	messy, err := New(Config{Host: "https://" + host + "/", APIKey: "k", HTTPClient: ts.Client()})
	if err != nil {
		t.Fatalf("带协议的 Host 应被接受：%v", err)
	}
	if _, err := messy.LookupCity(context.Background(), "宿迁", 6); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if !strings.HasPrefix(gotHost, "127.0.0.1") {
		t.Fatalf("请求应打到去掉协议后的主机，实际 Host=%q", gotHost)
	}
}

func TestLookupCityMapsBusinessCodes(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantCities int
		wantErrSub string
		wantStatus int
	}{
		{"成功", http.StatusOK, `{"code":"200","location":[{"name":"宿迁","lat":"33.9","lon":"118.3"}]}`, 1, "", 0},
		{"无数据 204", http.StatusOK, `{"code":"204"}`, 0, "", 0},
		{"查无此地 404", http.StatusOK, `{"code":"404"}`, 0, "", 0},
		{"鉴权失败 401", http.StatusUnauthorized, `{"code":"401"}`, 0, "鉴权失败", http.StatusBadGateway},
		{"额度超限 402", http.StatusOK, `{"code":"402"}`, 0, "额度", http.StatusBadGateway},
		{"无权限 403", http.StatusOK, `{"code":"403"}`, 0, "无权限", http.StatusBadGateway},
		{"未知业务码", http.StatusOK, `{"code":"500"}`, 0, "返回异常", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}, time.Second)

			cities, err := c.LookupCity(context.Background(), "宿迁", 6)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("不应报错：%v", err)
				}
				if len(cities) != tc.wantCities {
					t.Fatalf("应返回 %d 条，实际 %d", tc.wantCities, len(cities))
				}
				return
			}
			if err == nil {
				t.Fatalf("应返回错误（含 %q）", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("错误信息应含 %q，实际 %v", tc.wantErrSub, err)
			}
			var qwErr *Error
			if !errors.As(err, &qwErr) {
				t.Fatalf("错误类型应为 *Error，实际 %T", err)
			}
			if qwErr.Status != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d", tc.wantStatus, qwErr.Status)
			}
		})
	}
}

// 无法解析的响应体：报 502，且不得把原始响应体带进错误信息（可能含账号相关信息）。
func TestLookupCityUnparsableBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>account 12345 blocked</html>")
	}, time.Second)

	_, err := c.LookupCity(context.Background(), "宿迁", 6)
	if err == nil {
		t.Fatal("应返回错误")
	}
	var qwErr *Error
	if !errors.As(err, &qwErr) {
		t.Fatalf("错误类型应为 *Error，实际 %T", err)
	}
	if qwErr.Status != http.StatusBadGateway {
		t.Fatalf("应为 502，实际 %d", qwErr.Status)
	}
	if strings.Contains(qwErr.Msg, "account 12345") {
		t.Fatalf("面向用户的提示不得回显上游原始响应体：%q", qwErr.Msg)
	}
}

// 超时必须映射为 504，便于前端给出"稍后重试"的语义。
func TestLookupCityTimeout(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"code":"200"}`)
	}, 50*time.Millisecond)

	_, err := c.LookupCity(context.Background(), "宿迁", 6)
	if err == nil {
		t.Fatal("应超时")
	}
	var qwErr *Error
	if !errors.As(err, &qwErr) {
		t.Fatalf("错误类型应为 *Error，实际 %T", err)
	}
	if qwErr.Status != http.StatusGatewayTimeout {
		t.Fatalf("超时应为 504，实际 %d", qwErr.Status)
	}
}

func TestLookupCityEmptyKeywordDoesNotCallUpstream(t *testing.T) {
	called := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	}, time.Second)

	cities, err := c.LookupCity(context.Background(), "   ", 6)
	if err != nil {
		t.Fatalf("空白关键词不应报错：%v", err)
	}
	if len(cities) != 0 {
		t.Fatalf("空白关键词应返回空结果，实际 %d 条", len(cities))
	}
	if called {
		t.Fatal("空白关键词不应发起上游请求")
	}
}

// number 超出官方范围（1–20）时应回落默认值，避免把非法参数打给上游。
func TestLookupCityClampsNumber(t *testing.T) {
	var gotNumber string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNumber = r.URL.Query().Get("number")
		_, _ = io.WriteString(w, `{"code":"200","location":[]}`)
	}, time.Second)

	for _, n := range []int{0, -1, 999} {
		if _, err := c.LookupCity(context.Background(), "宿迁", n); err != nil {
			t.Fatalf("number=%d 不应报错：%v", n, err)
		}
		if gotNumber != "6" {
			t.Fatalf("number=%d 应被夹到 6，实际 %q", n, gotNumber)
		}
	}
}
