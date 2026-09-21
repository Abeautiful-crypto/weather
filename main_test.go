package main

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestShutdownTimeoutIsDerivedFromUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name     string
		upstream time.Duration
		want     time.Duration
	}{
		{"默认 8s 上游超时 → 10s 关停等待", 8 * time.Second, 10 * time.Second},
		{"上游超时很小 → 关停下限 5s", time.Second, 5 * time.Second},
		{"上游超时 3s → 推导值恰好达到下限", 3 * time.Second, 5 * time.Second},
		{"上游超时很大 → 关停等待跟随放大", 30 * time.Second, 32 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shutdownTimeout(tc.upstream); got != tc.want {
				t.Fatalf("shutdownTimeout(%v) = %v，期望 %v", tc.upstream, got, tc.want)
			}
		})
	}

	// 核心不变量：关停等待必须覆盖一次完整的上游调用
	for _, upstream := range []time.Duration{
		500 * time.Millisecond, time.Second, 8 * time.Second, 30 * time.Second,
	} {
		if got := shutdownTimeout(upstream); got <= upstream {
			t.Fatalf("关停等待 %v 未覆盖上游超时 %v", got, upstream)
		}
	}
}

// 前端必须只走同源 /api/*。这条断言把人工验收里的"Network 面板不再出现 open-meteo
// 域名请求"机械化：后续任何人往页面里加 CDN、或把 fetch 目标改回绝对地址，go test
// 都会直接失败，而不是依赖"记得打开 Network 面板看一眼"。
//
// 它同时守住 embed 路径（webFS/web 写错只有运行时才炸）。
func TestEmbeddedFrontendStaysSameOrigin(t *testing.T) {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("内嵌静态资源不可用：%v", err)
	}

	raw, err := fs.ReadFile(static, "index.html")
	if err != nil {
		t.Fatalf("读不到内嵌的 index.html：%v", err)
	}
	html := string(raw)
	if len(html) == 0 {
		t.Fatal("内嵌的 index.html 为空")
	}
	if !strings.Contains(html, "<title>") {
		t.Fatal("内嵌的 index.html 不含 <title>，embed 路径可能写错")
	}

	// 只断言"接口主机名"，不能断言 open-meteo 字样：页脚有一个指向 open-meteo.com
	// 的文档链接和"数据来源 Open-Meteo"文案，那是允许存在的。
	for _, host := range []string{
		"api.open-meteo.com",
		"geocoding-api.open-meteo.com",
		"air-quality-api.open-meteo.com",
	} {
		if strings.Contains(html, host) {
			t.Fatalf("前端不应直连上游，但页面里出现了 %s", host)
		}
	}

	// 不允许加载任何外部资源（CDN 脚本/样式/图标/字体）。
	// 只放行 <a href="..."> 这类文档链接（页脚有一个指向 open-meteo.com 的说明链接），
	// 因为它不产生额外的网络请求，也不会破坏"页面自包含"的约束。
	attrRe := regexp.MustCompile(`(?i)<([a-z0-9]+)[^>]*?(?:src|href)\s*=\s*"(https?://[^"]*)"`)
	for _, m := range attrRe.FindAllStringSubmatch(html, -1) {
		if tag := strings.ToLower(m[1]); tag != "a" {
			t.Fatalf("页面不应加载外部资源，但发现 <%s ... %s>", tag, m[2])
		}
	}

	// 所有 getJSON 调用必须是同源相对路径
	re := regexp.MustCompile(`getJSON\('([^']*)'`)
	matches := re.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		t.Fatal("没有匹配到任何 getJSON 调用，断言已失效，请同步更新本测试")
	}
	for _, m := range matches {
		if !strings.HasPrefix(m[1], "/") {
			t.Fatalf("getJSON 目标必须是同源相对路径，实际 %q", m[1])
		}
	}

	// 三个接口都应被前端调用到，防止接口改名后前端漏改
	for _, api := range []string{"/api/geocode", "/api/weather", "/api/air"} {
		if !strings.Contains(html, api) {
			t.Fatalf("前端未调用 %s", api)
		}
	}
}
