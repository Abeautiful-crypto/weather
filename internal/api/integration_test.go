package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"weather/internal/amap"
	"weather/internal/cache"
	"weather/internal/upstream"
)

// realUpstreamKeyEnv 与 main.go 的 envAMapKey 同名。
const realUpstreamKeyEnv = "AMAP_KEY"

// newRealEnv 用真实高德上游启动被测服务；未设置 Key 时**明确跳过并说明原因**，
// 不会静默通过。
func newRealEnv(t *testing.T) string {
	t.Helper()

	key := strings.TrimSpace(os.Getenv(realUpstreamKeyEnv))
	if key == "" {
		t.Skipf("跳过真实上游集成测试：未设置 %s（httptest 单测已覆盖映射与错误处理）", realUpstreamKeyEnv)
	}

	client, err := amap.New(amap.Config{Key: key, Timeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("创建高德客户端失败：%v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cache.New(time.Minute), upstream.NewDefault(8*time.Second), client, nil, logger)
	ts := httptest.NewServer(srv.Routes(nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

type realResult struct {
	Name      string  `json:"name"`
	Admin1    string  `json:"admin1"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Level     string  `json:"level"`
	Adcode    string  `json:"adcode"`
}

func searchReal(t *testing.T, base, keyword string) []realResult {
	t.Helper()

	res := get(t, base+"/api/geocode?q="+url.QueryEscape(keyword))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("搜索 %q 应返回 200，实际 %d", keyword, res.StatusCode)
	}
	var parsed struct {
		Results []realResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	return parsed.Results
}

// TestIntegrationAMapRealUpstream 存在的理由：高德那些字段形态（空数组、直辖市
// city 恒空、level 对直辖市返回"省"、location 是"经度,纬度"）全部是从真实响应里
// 挖出来的。httptest 的假响应只能验证"给定这种响应我们处理正确"，无法验证
// "真实响应仍然长这样"——这个测试就是那条防线。
func TestIntegrationAMapRealUpstream(t *testing.T) {
	base := newRealEnv(t)

	t.Run("GeoNames 查不到的地名高德能搜到且坐标顺序正确", func(t *testing.T) {
		results := searchReal(t, base, "宿迁")
		if len(results) == 0 {
			t.Fatal("高德应能搜到宿迁（这正是换搜索源要解决的问题）")
		}

		found := false
		for _, r := range results {
			if !strings.Contains(r.Name, "宿迁") {
				continue
			}
			found = true
			// 中国境内的范围校验：若把「经度,纬度」读反了，纬度会变成 118 度。
			// 客户端的范围校验会把这种条目丢掉（结果为空），所以这里能同时守住两侧。
			if r.Latitude < 3 || r.Latitude > 54 || r.Longitude < 73 || r.Longitude > 136 {
				t.Fatalf("坐标不像中国境内，疑似经纬度顺序读反：%+v", r)
			}
			if r.Level == "" {
				t.Fatalf("归一化级别不应为空：%+v", r)
			}
		}
		if !found {
			t.Fatalf("结果中应包含宿迁：%+v", results)
		}
	})

	t.Run("同名地名返回多条且仅凭展示字段可区分", func(t *testing.T) {
		results := searchReal(t, base, "朝阳")
		if len(results) < 2 {
			t.Fatalf("高德对「朝阳」应返回多条候选（实测 3 条），实际 %d 条：%+v", len(results), results)
		}
		labels := make(map[string]bool, len(results))
		for _, r := range results {
			label := r.Name + "|" + r.Admin1
			if labels[label] {
				t.Fatalf("候选标签重复，用户无法区分：%q（%+v）", label, results)
			}
			labels[label] = true
		}
	})

	t.Run("直辖市城市名不落空", func(t *testing.T) {
		results := searchReal(t, base, "上海")
		if len(results) == 0 {
			t.Fatal("应能搜到上海")
		}
		// 直辖市的 city 字段恒为空数组，城市名必须由 province 兜底；
		// 若兜底失效，这里会看到空名字。
		if results[0].Name == "" {
			t.Fatalf("城市名不应为空（直辖市需用 province 兜底）：%+v", results[0])
		}
	})

	t.Run("定位取名经坐标转换后给出正确区县", func(t *testing.T) {
		// 天安门正好在东城/西城分界上：不做 GPS→高德坐标转换会得到「西城区」，
		// 转换后才是正确的「东城区」。这条断言把"转换不能省"钉死在真实数据上。
		res := get(t, base+"/api/regeo?lat=39.9076&lon=116.3911")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("定位取名应返回 200，实际 %d", res.StatusCode)
		}
		var parsed struct {
			Name     string `json:"name"`
			District string `json:"district"`
		}
		if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
			t.Fatalf("响应不是合法 JSON：%v", err)
		}
		if parsed.Name != "北京市" {
			t.Fatalf("城市名应为北京市，实际 %q", parsed.Name)
		}
		if parsed.District != "东城区" {
			t.Fatalf("区县应为东城区（若是西城区说明漏掉了坐标转换），实际 %q", parsed.District)
		}
	})
}
