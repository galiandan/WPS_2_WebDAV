// Package cache provides the bounded TTL metadata cache shared by the
// storage layer: folder listings keyed by group, root generation, and
// parent ID, with same-key request merging so a cold directory is fetched
// from WPS exactly once no matter how many requests race on it.
//
// The root generation partitions entries across workspace remappings: a
// full invalidation bumps the generation, so a listing that was in flight
// before the change can only land under the old generation's key and never
// repopulates the new one ("迟到请求不得污染新 workspace"). Single-folder
// invalidations after a mutation follow the same rule through a per-key
// load identity, keeping every untouched folder's cache entry valid.
package cache

import (
	"container/heap"
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
	key      Key
	index    int
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
	expiry     expiryHeap
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
// never stored. A load that races Invalidate or InvalidateFolder is
// discarded: its generation or load identity went stale, so it can neither
// repopulate the key nor absorb callers that arrived after the invalidation.
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
	c.mu.Unlock()

	entries, err := load()

	c.mu.Lock()
	// Invalidation detaches this load. An older load must never delete a
	// replacement or overwrite its result, even if the replacement finished.
	if c.inflights[key] == current {
		delete(c.inflights, key)
		if err == nil && key.Generation == c.generation {
			c.storeLocked(key, entries)
		}
	}
	c.mu.Unlock()
	current.entries = entries
	current.err = err
	close(current.done)
	return entries, err
}

// expiryHeap keeps eviction O(log n), ordered by expiry and then insertion
// sequence. Indexed removal prevents invalidated folders leaving tombstones.
type expiryHeap []*folderEntry

func (h expiryHeap) Len() int { return len(h) }
func (h expiryHeap) Less(i, j int) bool {
	return h[i].expireAt.Before(h[j].expireAt) ||
		(h[i].expireAt.Equal(h[j].expireAt) && h[i].seq < h[j].seq)
}
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *expiryHeap) Push(value any) {
	entry := value.(*folderEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *expiryHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	old[len(old)-1] = nil
	entry.index = -1
	*h = old[:len(old)-1]
	return entry
}

// storeLocked preserves earliest-expiry eviction, including insertion-order
// ties and refreshing an existing key without evicting an unrelated folder.
func (c *Cache) storeLocked(key Key, entries []model.RemoteEntry) {
	c.seq++
	if existing, ok := c.entries[key]; ok {
		existing.expireAt = c.now().Add(c.ttl)
		existing.seq = c.seq
		existing.entries = entries
		heap.Fix(&c.expiry, existing.index)
		return
	}
	if len(c.entries) >= c.maxFolders {
		oldest := heap.Pop(&c.expiry).(*folderEntry)
		delete(c.entries, oldest.key)
	}
	entry := &folderEntry{key: key, expireAt: c.now().Add(c.ttl), seq: c.seq, entries: entries}
	c.entries[key] = entry
	heap.Push(&c.expiry, entry)
}

// Invalidate drops every cached folder and bumps the root generation, so
// loads that were in flight before this call can no longer enter the cache.
// Called on workspace remapping, where every cached folder may change.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[Key]*folderEntry)
	c.expiry = nil
	c.inflights = make(map[Key]*inflightLoad)
	c.generation++
}

// InvalidateFolder drops one folder and detaches its active load. Existing
// callers still receive their result; later callers start a fresh load.
// No per-folder invalidation history is retained after the load completes.
func (c *Cache) InvalidateFolder(groupID string, parentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := Key{GroupID: groupID, Generation: c.generation, ParentID: parentID}
	if entry, ok := c.entries[key]; ok {
		heap.Remove(&c.expiry, entry.index)
		delete(c.entries, key)
	}
	delete(c.inflights, key)
}
