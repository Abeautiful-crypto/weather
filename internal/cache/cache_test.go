package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 提供可手动推进的假时钟，用于验证 TTL 过期。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestMissThenHit(t *testing.T) {
	clk := newFakeClock()
	c := NewWithClock(time.Minute, clk.Now)

	var calls int32
	load := func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("payload"), true, nil
	}

	data, err, hit := c.GetOrLoad("k", load)
	if err != nil || hit {
		t.Fatalf("首次调用应为未命中且无错误，got hit=%v err=%v", hit, err)
	}
	if string(data) != "payload" {
		t.Fatalf("数据不符：%q", data)
	}

	data, err, hit = c.GetOrLoad("k", load)
	if err != nil || !hit {
		t.Fatalf("二次调用应命中缓存，got hit=%v err=%v", hit, err)
	}
	if string(data) != "payload" {
		t.Fatalf("缓存数据不符：%q", data)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load 应只被调用 1 次，实际 %d 次", got)
	}
	if c.Len() != 1 {
		t.Fatalf("缓存条目数应为 1，实际 %d", c.Len())
	}
}

func TestTTLExpiry(t *testing.T) {
	clk := newFakeClock()
	c := NewWithClock(time.Minute, clk.Now)

	var calls int32
	load := func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("v"), true, nil
	}

	c.GetOrLoad("k", load)
	clk.Advance(59 * time.Second)
	if _, _, hit := c.GetOrLoad("k", load); !hit {
		t.Fatal("TTL 内应命中缓存")
	}
	clk.Advance(2 * time.Second) // 累计超过 1 分钟
	if _, _, hit := c.GetOrLoad("k", load); hit {
		t.Fatal("TTL 过期后不应命中缓存")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("load 应被调用 2 次，实际 %d 次", got)
	}
}

func TestZeroTTLNeverCaches(t *testing.T) {
	c := New(0)
	var calls int32
	load := func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("v"), true, nil
	}
	c.GetOrLoad("k", load)
	c.GetOrLoad("k", load)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("TTL 为 0 时不应缓存，load 应为 2 次，实际 %d 次", got)
	}
	if c.Len() != 0 {
		t.Fatalf("TTL 为 0 时缓存条目应为 0，实际 %d", c.Len())
	}
}

func TestErrorNotCached(t *testing.T) {
	c := New(time.Minute)
	wantErr := errors.New("boom")
	var calls int32

	data, err, hit := c.GetOrLoad("k", func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return nil, false, wantErr
	})
	if !errors.Is(err, wantErr) || hit || data != nil {
		t.Fatalf("错误结果不应被缓存，got data=%q hit=%v err=%v", data, hit, err)
	}

	if _, err, _ := c.GetOrLoad("k", func() ([]byte, bool, error) {
		return []byte("ok"), true, nil
	}); err != nil {
		t.Fatalf("重试应成功：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("失败分支应只调用 1 次，实际 %d 次", got)
	}
}

func TestNotCacheableResultNotStored(t *testing.T) {
	c := New(time.Minute)
	var calls int32
	load := func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("raw"), false, nil // cacheable=false
	}

	if _, _, hit := c.GetOrLoad("k", load); hit {
		t.Fatal("首次调用不应命中")
	}
	if _, _, hit := c.GetOrLoad("k", load); hit {
		t.Fatal("不可缓存的结果不应命中")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("不可缓存时 load 应为 2 次，实际 %d 次", got)
	}
	if c.Len() != 0 {
		t.Fatalf("不可缓存的结果不应入库，实际 %d 条", c.Len())
	}
}

func TestSingleflightMergesConcurrentLoads(t *testing.T) {
	c := New(time.Minute)
	var calls int32

	const goroutines = 32
	var wg sync.WaitGroup
	results := make([]string, goroutines)
	hits := make([]bool, goroutines)

	release := make(chan struct{})
	load := func() ([]byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		<-release // 卡住首个调用，确保其它 goroutine 进入等待
		return []byte("shared"), true, nil
	}

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			data, err, hit := c.GetOrLoad("k", load)
			if err != nil {
				t.Errorf("goroutine %d 出错: %v", idx, err)
				return
			}
			results[idx] = string(data)
			hits[idx] = hit
		}(i)
	}

	// 给所有 goroutine 时间进入等待状态
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("单飞应把并发请求合并为 1 次上游调用，实际 %d 次", got)
	}
	for i, r := range results {
		if r != "shared" {
			t.Fatalf("goroutine %d 结果不符：%q", i, r)
		}
		if hits[i] {
			t.Fatalf("goroutine %d 不应报告命中缓存（真实执行了加载）", i)
		}
	}
	// 加载完成后结果已入库，后续请求直接命中
	if _, _, hit := c.GetOrLoad("k", load); !hit {
		t.Fatal("加载完成后应命中缓存")
	}
}

func TestStats(t *testing.T) {
	c := New(time.Minute)
	load := func() ([]byte, bool, error) { return []byte("v"), true, nil }

	c.GetOrLoad("k", load)
	c.GetOrLoad("k", load)

	hits, misses := c.Stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("统计不符，hits=%d misses=%d", hits, misses)
	}
}
