package cache

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func fakeEntry(id string) model.RemoteEntry {
	return model.RemoteEntry{ID: id, Name: "name-" + id, Kind: model.KindFile}
}

func testKey(parent string) Key {
	return Key{GroupID: "group-1", Generation: 0, ParentID: parent}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{TTL: -1, MaxFolders: 10}); err == nil || err.Error() != "cache_ttl must not be negative" {
		t.Fatalf("New error = %v", err)
	}
	if _, err := New(Options{TTL: time.Second, MaxFolders: -1}); err == nil || err.Error() != "max_cached_folders must be positive" {
		t.Fatalf("New error = %v", err)
	}
	cache, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Zero options fall back to the Python defaults.
	if cache.ttl != DefaultTTL || cache.maxFolders != DefaultMaxFolders {
		t.Fatalf("defaults drifted: %v %d", cache.ttl, cache.maxFolders)
	}
}

func TestGetOrLoadCachesSuccessfulResult(t *testing.T) {
	cache, err := New(Options{TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	loader := func() ([]model.RemoteEntry, error) {
		loads++
		return []model.RemoteEntry{fakeEntry("1"), fakeEntry("2")}, nil
	}
	entries, err := cache.GetOrLoad(testKey("p"), loader)
	if err != nil || len(entries) != 2 {
		t.Fatalf("first GetOrLoad = %v, %v", entries, err)
	}
	entries, err = cache.GetOrLoad(testKey("p"), loader)
	if err != nil || len(entries) != 2 || loads != 1 {
		t.Fatalf("second GetOrLoad loaded again (%d): %v, %v", loads, entries, err)
	}
	if _, ok := cache.Get(testKey("p")); !ok {
		t.Fatal("Get missed a freshly cached folder")
	}
	// The root folder result (empty listing) is a complete result too.
	empty := 0
	if _, err := cache.GetOrLoad(testKey("root"), func() ([]model.RemoteEntry, error) {
		empty++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetOrLoad(testKey("root"), func() ([]model.RemoteEntry, error) {
		empty++
		return nil, nil
	}); err != nil || empty != 1 {
		t.Fatalf("empty folder reloaded (%d)", empty)
	}
}

func TestErrorsAreNeverCached(t *testing.T) {
	cache, err := New(Options{TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	loader := func() ([]model.RemoteEntry, error) {
		loads++
		return nil, errors.New("upstream failed")
	}
	for i := 0; i < 3; i++ {
		if _, err := cache.GetOrLoad(testKey("p"), loader); err == nil {
			t.Fatal("loader error swallowed")
		}
	}
	if loads != 3 {
		t.Fatalf("error results were cached: %d loads", loads)
	}
	if _, ok := cache.Get(testKey("p")); ok {
		t.Fatal("error result cached")
	}
}

// TestConcurrentColdMissesMerge is the completion condition: many requests
// hitting a cold directory produce exactly one full upstream pagination.
func TestConcurrentColdMissesMerge(t *testing.T) {
	cache, err := New(Options{TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var loads int
	var mu sync.Mutex
	release := make(chan struct{})
	loader := func() ([]model.RemoteEntry, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		<-release
		return []model.RemoteEntry{fakeEntry("1")}, nil
	}

	const callers = 8
	results := make([][]model.RemoteEntry, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	started := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			results[i], errs[i] = cache.GetOrLoad(testKey("p"), loader)
		}(i)
	}
	for i := 0; i < callers; i++ {
		<-started
	}
	// Let the merged callers reach the join point, then finish the load.
	deadline := time.Now().Add(2 * time.Second)
	for {
		cache.mu.Lock()
		pending := len(cache.inflights)
		cache.mu.Unlock()
		if pending == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	leaderStarted := loads == 1
	mu.Unlock()
	if !leaderStarted {
		t.Fatal("leader did not start first")
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if loads != 1 {
		t.Fatalf("loads = %d, want 1 (merge failed)", loads)
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil || len(results[i]) != 1 || results[i][0].ID != "1" {
			t.Fatalf("caller %d got %v, %v", i, results[i], errs[i])
		}
	}
}

func TestDifferentKeysLoadInParallel(t *testing.T) {
	cache, err := New(Options{TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var entered sync.WaitGroup
	entered.Add(2)
	loader := func(name string) func() ([]model.RemoteEntry, error) {
		return func() ([]model.RemoteEntry, error) {
			entered.Done() // both loaders must be inside their load at once
			entered.Wait()
			return []model.RemoteEntry{fakeEntry(name)}, nil
		}
	}
	results := make([][]model.RemoteEntry, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = cache.GetOrLoad(testKey("a"), loader("a")) }()
	go func() { defer wg.Done(); results[1], errs[1] = cache.GetOrLoad(testKey("b"), loader("b")) }()
	wg.Wait()
	for i, want := range []string{"a", "b"} {
		if errs[i] != nil || len(results[i]) != 1 || results[i][0].ID != want {
			t.Fatalf("loader %d got %v, %v", i, results[i], errs[i])
		}
	}
}

func TestKeyComponentsIsolateEntries(t *testing.T) {
	cache, err := New(Options{TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	loader := func(tag string) func() ([]model.RemoteEntry, error) {
		return func() ([]model.RemoteEntry, error) {
			return []model.RemoteEntry{fakeEntry(tag)}, nil
		}
	}
	if _, err := cache.GetOrLoad(Key{GroupID: "g1", Generation: 0, ParentID: "p"}, loader("g1g0")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{
		{GroupID: "g2", Generation: 0, ParentID: "p"},
		{GroupID: "g1", Generation: 1, ParentID: "p"},
		{GroupID: "g1", Generation: 0, ParentID: "q"},
	} {
		entries, err := cache.GetOrLoad(key, loader(key.GroupID+strconv.FormatUint(key.Generation, 10)+key.ParentID))
		if err != nil {
			t.Fatal(err)
		}
		if entries[0].ID == "g1g0" {
			t.Fatalf("key %v served another key's entries", key)
		}
	}
}

func TestTTLOverridesWithFreshStore(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: 2 * time.Second, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	loader := func() ([]model.RemoteEntry, error) {
		loads++
		return []model.RemoteEntry{fakeEntry("1")}, nil
	}
	if _, err := cache.GetOrLoad(testKey("p"), loader); err != nil {
		t.Fatal(err)
	}
	current = current.Add(1999 * time.Millisecond)
	if _, ok := cache.Get(testKey("p")); !ok {
		t.Fatal("entry expired one millisecond early")
	}
	current = current.Add(2 * time.Millisecond)
	if _, ok := cache.Get(testKey("p")); ok {
		t.Fatal("entry survived its TTL")
	}
	if _, err := cache.GetOrLoad(testKey("p"), loader); err != nil {
		t.Fatal(err)
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2 after expiry", loads)
	}
}

// TestEvictionOrderDeterministic pins the bounded size and the eviction
// order: earliest expiry first, ties by insertion order (Python's min()
// over an insertion-ordered dict).
func TestEvictionOrderDeterministic(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: time.Second, MaxFolders: 3, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	loader := func() ([]model.RemoteEntry, error) {
		return []model.RemoteEntry{fakeEntry("x")}, nil
	}
	for _, parent := range []string{"a", "b", "c"} {
		if _, err := cache.GetOrLoad(testKey(parent), loader); err != nil {
			t.Fatal(err)
		}
	}
	// All three share the same expiry instant; adding d must evict a, the
	// earliest insertion among the ties.
	current = current.Add(500 * time.Millisecond)
	if _, err := cache.GetOrLoad(testKey("d"), loader); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, aAlive := cache.entries[testKey("a")]
	_, bAlive := cache.entries[testKey("b")]
	cache.mu.Unlock()
	if aAlive || !bAlive {
		t.Fatalf("eviction picked wrong tie: a=%v b=%v", aAlive, bAlive)
	}

	// Re-inserting a at the new instant is itself a new key needing a
	// slot, so it evicts b (the oldest remaining tie); inserting e then
	// evicts c.
	current = current.Add(500 * time.Millisecond)
	if _, err := cache.GetOrLoad(testKey("a"), loader); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, bAlive = cache.entries[testKey("b")]
	_, dAlive := cache.entries[testKey("d")]
	cache.mu.Unlock()
	if bAlive || !dAlive {
		t.Fatalf("re-insert eviction picked wrong entry: b=%v d=%v", bAlive, dAlive)
	}
	if _, err := cache.GetOrLoad(testKey("e"), loader); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, cAlive := cache.entries[testKey("c")]
	_, aAlive = cache.entries[testKey("a")]
	cache.mu.Unlock()
	if cAlive || !aAlive {
		t.Fatalf("second eviction picked wrong entry: c=%v a=%v", cAlive, aAlive)
	}
}

// TestEvictionPrefersExpiredEntries covers the Python behaviour where an
// expired entry still occupies a slot until eviction: it expires earliest
// and is the first to go.
func TestEvictionPrefersExpiredEntries(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: time.Second, MaxFolders: 2, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	loader := func() ([]model.RemoteEntry, error) {
		return []model.RemoteEntry{fakeEntry("x")}, nil
	}
	if _, err := cache.GetOrLoad(testKey("old"), loader); err != nil {
		t.Fatal(err)
	}
	current = current.Add(5 * time.Second) // old is long expired, not refreshed
	if _, err := cache.GetOrLoad(testKey("new"), loader); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, oldAlive := cache.entries[testKey("old")]
	_, newAlive := cache.entries[testKey("new")]
	cache.mu.Unlock()
	if !oldAlive || !newAlive {
		t.Fatalf("unexpected state: old=%v new=%v", oldAlive, newAlive)
	}
	// The expired entry must be the eviction victim, not the fresh one.
	if _, err := cache.GetOrLoad(testKey("third"), loader); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, oldAlive = cache.entries[testKey("old")]
	_, newAlive = cache.entries[testKey("new")]
	cache.mu.Unlock()
	if oldAlive || !newAlive {
		t.Fatalf("expired entry survived eviction: old=%v new=%v", oldAlive, newAlive)
	}
}

// TestInvalidateBlocksLateLoads pins the generation contract: a load that
// was in flight before invalidation cannot enter the cache afterwards.
func TestInvalidateBlocksLateLoads(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: time.Second, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	lateLoaderRan := make(chan struct{})
	loader := func() ([]model.RemoteEntry, error) {
		close(lateLoaderRan)
		<-release
		return []model.RemoteEntry{fakeEntry("stale")}, nil
	}
	results := make(chan []model.RemoteEntry, 1)
	go func() {
		entries, err := cache.GetOrLoad(testKey("p"), loader)
		if err != nil {
			t.Error(err)
			return
		}
		results <- entries
	}()
	<-lateLoaderRan
	cache.Invalidate()
	close(release)
	if entries := <-results; len(entries) != 1 || entries[0].ID != "stale" {
		t.Fatalf("leader lost its own result: %v", entries)
	}
	if _, ok := cache.Get(testKey("p")); ok {
		t.Fatal("late load polluted the new generation")
	}
	if cache.Generation() != 1 {
		t.Fatalf("generation = %d, want 1", cache.Generation())
	}

	// A load rebuilt with the current generation (the caller pattern after
	// a remap) works and caches.
	fresh := 0
	currentKey := Key{GroupID: "group-1", Generation: cache.Generation(), ParentID: "p"}
	if _, err := cache.GetOrLoad(currentKey, func() ([]model.RemoteEntry, error) {
		fresh++
		return []model.RemoteEntry{fakeEntry("fresh")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if fresh != 1 {
		t.Fatal("fresh generation loader did not run")
	}
	if entries, ok := cache.Get(currentKey); !ok || entries[0].ID != "fresh" {
		t.Fatalf("new generation cache = %v, %v", entries, ok)
	}
}

// TestInvalidateRemovesEverything covers the successful-mutation path:
// after Invalidate every cached folder is gone and keys must be rebuilt
// with the new generation.
func TestInvalidateRemovesEverything(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: time.Second, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	loader := func() ([]model.RemoteEntry, error) {
		return []model.RemoteEntry{fakeEntry("1")}, nil
	}
	for _, parent := range []string{"a", "b"} {
		if _, err := cache.GetOrLoad(testKey(parent), loader); err != nil {
			t.Fatal(err)
		}
	}
	cache.Invalidate()
	for _, parent := range []string{"a", "b"} {
		if _, ok := cache.Get(testKey(parent)); ok {
			t.Fatalf("folder %s survived invalidation", parent)
		}
	}
	if len(cache.entries) != 0 {
		t.Fatalf("entries map not empty: %d", len(cache.entries))
	}
}

// TestConcurrentLoadAndInvalidateRace exercises the interleaving of loads,
// joins, and invalidations under the race detector.
func TestConcurrentLoadAndInvalidateRace(t *testing.T) {
	current := time.Unix(1000, 0)
	cache, err := New(Options{TTL: time.Second, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				key := testKey(strconv.Itoa(worker%3) + "-" + strconv.Itoa(i%5))
				_, _ = cache.GetOrLoad(key, func() ([]model.RemoteEntry, error) {
					return []model.RemoteEntry{fakeEntry(strconv.Itoa(worker))}, nil
				})
				if i%10 == 0 {
					cache.Invalidate()
				}
			}
		}(worker)
	}
	wg.Wait()
	if cache.Generation() == 0 {
		t.Fatal("no invalidation observed")
	}
}
