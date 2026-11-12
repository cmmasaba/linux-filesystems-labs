package main

import (
	"strings"
	"sync"
	"time"
)

// entry is the result of resolving a name: a file, a directory (explicit or
// implicit) or nothing. attrs is the file object or the directory placeholder,
// and nil for implicit directories and the root.
type entry struct {
	exists bool
	isDir  bool
	attrs  *ObjAttrs
}

type cachedEntry struct {
	e       entry
	expires time.Time
}

// statCache remembers lookups, including misses, for a short TTL. The kernel
// probes many names that don't exist, and each uncached probe costs two GCS
// requests. A TTL of 0 disables the cache.
//
// Results are stored with putIfUnchanged: a lookup that started before a
// local change and finished after it must not re-insert what it saw, so every
// invalidation bumps an epoch and stale results are dropped.
//
// get removes an expired entry only when its name is asked for again, so
// putIfUnchanged also sweeps every expired entry whenever the map reaches
// sweepAt. Otherwise a walk over a large bucket would keep every name it saw
// until unmount.
type statCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	m       map[string]cachedEntry
	epoch   uint64
	sweepAt int
}

// minSweep is the smallest map size that triggers a sweep.
const minSweep = 1024

func newStatCache(ttl time.Duration, now func() time.Time) *statCache {
	return &statCache{ttl: ttl, now: now, m: map[string]cachedEntry{}, sweepAt: minSweep}
}

func (c *statCache) get(name string) (entry, bool) {
	if c.ttl <= 0 {
		return entry{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ce, ok := c.m[name]
	if !ok {
		return entry{}, false
	}

	if c.now().After(ce.expires) {
		delete(c.m, name)
		return entry{}, false
	}

	return ce.e, true
}

// snapshot returns the current epoch; take it before fetching from GCS.
func (c *statCache) snapshot() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.epoch
}

// putIfUnchanged stores e unless something was invalidated since the
// snapshot was taken.
func (c *statCache) putIfUnchanged(name string, e entry, epoch uint64) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.epoch != epoch {
		return
	}

	now := c.now()
	c.m[name] = cachedEntry{e: e, expires: now.Add(c.ttl)}

	if len(c.m) >= c.sweepAt {
		c.sweepLocked(now)
	}
}

// sweepLocked drops every expired entry. The next sweep waits until the map
// has doubled from what is left, so the cost of a sweep is spread over the
// inserts in between, and the map never grows past twice what the previous
// sweep left (or minSweep). c.mu must be held.
func (c *statCache) sweepLocked(now time.Time) {
	for k, ce := range c.m {
		if now.After(ce.expires) {
			delete(c.m, k)
		}
	}

	c.sweepAt = max(minSweep, 2*len(c.m))
}

// invalidate drops the given names and their parent directories. A parent can
// change from implicit to gone (or appear) when its last/first child changes.
func (c *statCache) invalidate(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.epoch++

	for _, n := range names {
		delete(c.m, n)

		if n != "" {
			delete(c.m, parentOf(n))
		}
	}
}

// invalidateTree drops name and everything below it.
func (c *statCache) invalidateTree(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.epoch++
	prefix := dirPrefix(name)
	for k := range c.m {
		if k == name || strings.HasPrefix(k, prefix) {
			delete(c.m, k)
		}
	}

	if name != "" {
		delete(c.m, parentOf(name))
	}
}
