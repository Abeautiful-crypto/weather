// Package cache 提供一个带 TTL 的内存缓存，并通过「单飞」把同一个 key 的
// 并发请求合并为一次上游调用，避免上游被重复请求或限流。
package cache

import (
	"sync"
	"time"
)

// LoadFunc 由调用方提供：返回待缓存的数据、该数据是否允许写入缓存，以及错误。
// 仅当 err == nil 且 cacheable == true 时结果才会进入缓存。
type LoadFunc func() (data []byte, cacheable bool, err error)

type entry struct {
	data []byte
	exp  time.Time
}

// call 代表一次正在执行中的加载，等待者共享同一个结果。
type call struct {
	wg   sync.WaitGroup
	data []byte
	err  error
}

// Cache 是并发安全的内存缓存。返回的 []byte 为共享只读切片，调用方不得修改。
type Cache struct {
	mu       sync.Mutex
	items    map[string]*entry
	inflight map[string]*call
	ttl      time.Duration
	now      func() time.Time
	hits     int64
	misses   int64
}

// New 创建一个 TTL 为 ttl 的缓存；ttl <= 0 表示永不写入缓存（但仍会合并并发请求）。
func New(ttl time.Duration) *Cache {
	return NewWithClock(ttl, time.Now)
}

// NewWithClock 与 New 相同，但允许注入时钟，便于测试 TTL 过期行为。
func NewWithClock(ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		items:    make(map[string]*entry),
		inflight: make(map[string]*call),
		ttl:      ttl,
		now:      now,
	}
}

// GetOrLoad 返回 key 对应的数据。
//
// hit 为 true 表示命中缓存（未调用 load）；为 false 表示这次真的执行了 load，
// 也可能只是等待了同 key 的其它请求。
func (c *Cache) GetOrLoad(key string, load LoadFunc) (data []byte, err error, hit bool) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		if c.now().Before(e.exp) {
			c.hits++
			d := e.data
			c.mu.Unlock()
			return d, nil, true
		}
		delete(c.items, key) // 过期惰性删除
	}
	if cl, ok := c.inflight[key]; ok {
		c.misses++
		c.mu.Unlock()
		cl.wg.Wait()
		return cl.data, cl.err, false
	}

	cl := &call{}
	cl.wg.Add(1)
	c.inflight[key] = cl
	c.misses++
	c.mu.Unlock()

	d, cacheable, loadErr := load()

	c.mu.Lock()
	if loadErr == nil && cacheable && c.ttl > 0 {
		c.items[key] = &entry{data: d, exp: c.now().Add(c.ttl)}
	}
	delete(c.inflight, key)
	cl.data, cl.err = d, loadErr
	c.mu.Unlock()
	cl.wg.Done()

	return d, loadErr, false
}

// Len 返回当前缓存条目数（含尚未过期的条目）。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Stats 返回累计的命中与未命中次数。
func (c *Cache) Stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
