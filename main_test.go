package main

import (
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
