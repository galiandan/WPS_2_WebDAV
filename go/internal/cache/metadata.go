// Package cache provides the bounded TTL metadata cache shared by the
// storage layer: folder listings keyed by group, root generation, and
// parent ID, with same-key request merging so a cold directory is fetched
// from WPS exactly once no matter how many requests race on it.
//
// The root generation partitions entries across workspace remappings and
// successful mutations: invalidation bumps the generation, so a listing
// that was in flight before the change can only land under the old
// generation's key and never repopulates the new one ("迟到请求不得污染
// 新 workspace").
package cache

import (
	"errors"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Defaults mirror storage.py's WpsStorage(cache_ttl=2.0,
// max_cached_folders=1024).
const (
	DefaultTTL        = 2 * time.Second
	DefaultMaxFolders = 1024
)

// Key identifies one cached folder. Every component matters: the group ID
// and generation keep mounted spaces and remapped roots from ever serving
// each other's entries.
type Key struct {
	GroupID    string
	Generation uint64
	ParentID   string
}

// Options configures a Cache. Zero TTL and MaxFolders fall back to the
// Python defaults.
type Options struct {
	TTL        time.Duration
	MaxFolders int
	// Now is a test seam; nil means time.Now.
	Now func() time.Time
}

// New validates the options and returns an empty cache.
func New(options Options) (*Cache, error) {
	if options.TTL == 0 {
		options.TTL = DefaultTTL
	}
	if options.MaxFolders == 0 {
		options.MaxFolders = DefaultMaxFolders
	}
	if options.TTL < 0 {
		return nil, errors.New("cache_ttl must not be negative")
	}
	if options.MaxFolders <= 0 {
		return nil, errors.New("max_cached_folders must be positive")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Cache{
		entries:    make(map[Key]*folderEntry),
		inflights:  make(map[Key]*inflightLoad),
		ttl:        options.TTL,
		maxFolders: options.MaxFolders,
		now:        now,
	}, nil
}

type folderEntry struct {
	expireAt time.Time
	seq      uint64
	entries  []model.RemoteEntry
}

type inflightLoad struct {
	done    chan struct{}
	entries []model.RemoteEntry
	err     error
}

// Cache is safe for concurrent use.
type Cache struct {
	mu         sync.Mutex
	entries    map[Key]*folderEntry
	inflights  map[Key]*inflightLoad
	generation uint64
	ttl        time.Duration
	maxFolders int
	seq        uint64
	now        func() time.Time
}

// Generation returns the current root generation for key building. A
// generation read here stays valid for building keys even if Invalidate
// runs afterwards: such a load simply lands under a stale key and is
// discarded.
func (c *Cache) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// Get returns the cached children for key if present and unexpired. It is a
// pure read and never triggers a load.
func (c *Cache) Get(key Key) ([]model.RemoteEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.entries[key]
	if !ok || c.now().After(cached.expireAt) {
		return nil, false
	}
	return cached.entries, true
}

// GetOrLoad returns the cached children for key, loading them exactly once
// when absent: concurrent cold callers with the same key join one load,
// while different keys load in parallel. Only complete successful results
// are cached — an error is returned to every caller of that attempt and
// never stored.
func (c *Cache) GetOrLoad(key Key, load func() ([]model.RemoteEntry, error)) ([]model.RemoteEntry, error) {
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok && !c.now().After(cached.expireAt) {
		entries := cached.entries
		c.mu.Unlock()
		return entries, nil
	}
	if existing, ok := c.inflights[key]; ok {
		c.mu.Unlock()
		<-existing.done
		return existing.entries, existing.err
	}

	current := &inflightLoad{done: make(chan struct{})}
	c.inflights[key] = current
	generation := c.generation
	c.mu.Unlock()

	entries, err := load()

	c.mu.Lock()
	delete(c.inflights, key)
	if err == nil && generation == c.generation {
		// A complete, successful result for the current generation is the
		// only thing that may enter the cache. Partial pages and failures
		// are never cached, and a result that raced with Invalidate lands
		// under a dead generation and is dropped.
		c.storeLocked(key, entries)
	}
	c.mu.Unlock()
	current.entries = entries
	current.err = err
	close(current.done)
	return entries, err
}

// storeLocked inserts the folder, evicting the deterministically oldest
// entry when a new key needs room: earliest expiry, ties broken by earliest
// insertion sequence, which reproduces storage.py's min() over its
// insertion-ordered dict. Re-storing a known key only refreshes it, exactly
// like the Python dict assignment.
func (c *Cache) storeLocked(key Key, entries []model.RemoteEntry) {
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxFolders {
		var oldestKey Key
		var oldest folderEntry
		first := true
		for candidate, entry := range c.entries {
			older := entry.expireAt.Before(oldest.expireAt) ||
				(entry.expireAt.Equal(oldest.expireAt) && entry.seq < oldest.seq)
			if first || older {
				oldestKey, oldest, first = candidate, *entry, false
			}
		}
		delete(c.entries, oldestKey)
	}
	c.seq++
	c.entries[key] = &folderEntry{expireAt: c.now().Add(c.ttl), seq: c.seq, entries: entries}
}

// Invalidate drops every cached folder and bumps the root generation, so
// loads that were in flight before this call can no longer enter the cache.
// Called after successful mutations and on workspace remapping.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[Key]*folderEntry)
	c.generation++
}
