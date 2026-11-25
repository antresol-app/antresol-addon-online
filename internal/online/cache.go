package online

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// ttlCache is a size-capped LRU whose entries expire after ttl. With a
// byte budget (limitBytes) it also drops the oldest entries while their
// values together are over it.
type ttlCache[K comparable, V any] struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	now      func() time.Time
	ll       *list.List
	m        map[K]*list.Element
	size     func(V) int // the budget's measure (nil: no budget)
	maxBytes int
	bytes    int
}

type cacheEntry[K comparable, V any] struct {
	k   K
	v   V
	n   int // size(v)
	exp time.Time
}

func newTTLCache[K comparable, V any](max int, ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{max: max, ttl: ttl, now: time.Now, ll: list.New(), m: map[K]*list.Element{}}
}

// limitBytes caps the values kept at maxBytes together, as size measures
// them (call before the first put).
func (c *ttlCache[K, V]) limitBytes(maxBytes int, size func(V) int) {
	c.size, c.maxBytes = size, maxBytes
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
		c.remove(e)
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
	n := 0
	if c.size != nil {
		n = c.size(v)
	}
	if e, ok := c.m[k]; ok {
		c.remove(e) // as new: its size may differ
	}
	c.m[k] = c.ll.PushFront(&cacheEntry[K, V]{k: k, v: v, n: n, exp: c.now().Add(ttl)})
	c.bytes += n
	for c.ll.Len() > c.max || (c.size != nil && c.bytes > c.maxBytes && c.ll.Len() > 1) {
		c.remove(c.ll.Back())
	}
}

// remove drops e (c.mu held).
func (c *ttlCache[K, V]) remove(e *list.Element) {
	ent := e.Value.(*cacheEntry[K, V])
	c.ll.Remove(e)
	delete(c.m, ent.k)
	c.bytes -= ent.n
}
