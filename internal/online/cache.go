package online

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// ttlCache is a size-capped LRU whose entries expire after ttl.
type ttlCache[K comparable, V any] struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	now func() time.Time
	ll  *list.List
	m   map[K]*list.Element
}

type cacheEntry[K comparable, V any] struct {
	k   K
	v   V
	exp time.Time
}

func newTTLCache[K comparable, V any](max int, ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{max: max, ttl: ttl, now: time.Now, ll: list.New(), m: map[K]*list.Element{}}
}

type bypassKey struct{}

// Bypass marks ctx so that the caches are not read (a re-play after a
// stale link resolves everything fresh); answers are still stored.
func Bypass(ctx context.Context) context.Context { return context.WithValue(ctx, bypassKey{}, true) }

func bypassed(ctx context.Context) bool { b, _ := ctx.Value(bypassKey{}).(bool); return b }

func (c *ttlCache[K, V]) get(ctx context.Context, k K) (V, bool) {
	var zero V
	if bypassed(ctx) {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return zero, false
	}
	ent := e.Value.(*cacheEntry[K, V])
	if !c.now().Before(ent.exp) {
		c.ll.Remove(e)
		delete(c.m, k)
		return zero, false
	}
	c.ll.MoveToFront(e)
	return ent.v, true
}

func (c *ttlCache[K, V]) put(k K, v V) { c.putFor(k, v, c.ttl) }

// putFor stores v for ttl instead of the cache's own.
func (c *ttlCache[K, V]) putFor(k K, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(ttl)
	if e, ok := c.m[k]; ok {
		ent := e.Value.(*cacheEntry[K, V])
		ent.v, ent.exp = v, exp
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&cacheEntry[K, V]{k: k, v: v, exp: exp})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*cacheEntry[K, V]).k)
	}
}
