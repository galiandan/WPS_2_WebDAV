package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestStaleKeyCannotRepopulateCache(t *testing.T) {
	c, _ := New(Options{})
	key := testKey("p")
	c.Invalidate()
	result, err := c.GetOrLoad(key, func() ([]model.RemoteEntry, error) {
		return []model.RemoteEntry{fakeEntry("old")}, nil
	})
	if err != nil || len(result) != 1 {
		t.Fatalf("caller result = %v, %v", result, err)
	}
	if _, ok := c.Get(key); ok {
		t.Fatal("key captured before invalidation repopulated cache")
	}
}

func TestDetachedLoadCannotOverwriteReplacement(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, oldFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("full=%v/oldFirst=%v", full, oldFirst), func(t *testing.T) {
				c, _ := New(Options{})
				key := testKey("p")
				start := func(key Key, id string) (chan struct{}, chan struct{}) {
					entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
					go func() {
						defer close(done)
						result, err := c.GetOrLoad(key, func() ([]model.RemoteEntry, error) {
							close(entered)
							<-release
							return []model.RemoteEntry{fakeEntry(id)}, nil
						})
						if err != nil || len(result) != 1 || result[0].ID != id {
							t.Errorf("load %s = %v, %v", id, result, err)
						}
					}()
					<-entered
					return release, done
				}
				oldRelease, oldDone := start(key, "old")
				if full {
					c.Invalidate()
				} else {
					c.InvalidateFolder(key.GroupID, key.ParentID)
				}
				key.Generation = c.Generation()
				newRelease, newDone := start(key, "new")
				if oldFirst {
					close(oldRelease)
					<-oldDone
					c.mu.Lock()
					pending := len(c.inflights)
					c.mu.Unlock()
					if pending != 1 {
						t.Errorf("old completion removed replacement: %d pending", pending)
					}
					close(newRelease)
					<-newDone
				} else {
					close(newRelease)
					<-newDone
					close(oldRelease)
					<-oldDone
				}
				if result, ok := c.Get(key); !ok || result[0].ID != "new" {
					t.Fatalf("cache = %v, %v", result, ok)
				}
			})
		}
	}
}

func TestEvictionAfterRefreshAndInvalidation(t *testing.T) {
	now := time.Unix(100, 0)
	c, _ := New(Options{TTL: time.Second, MaxFolders: 2, Now: func() time.Time { return now }})
	load := func() ([]model.RemoteEntry, error) { return nil, nil }
	c.GetOrLoad(testKey("a"), load)
	now = now.Add(time.Millisecond)
	c.GetOrLoad(testKey("b"), load)
	now = now.Add(2 * time.Second)
	c.GetOrLoad(testKey("a"), load) // Refresh the old heap root.
	c.GetOrLoad(testKey("c"), load)
	if _, ok := c.Get(testKey("a")); !ok {
		t.Fatal("refreshed entry was evicted")
	}
	if _, ok := c.entries[testKey("b")]; ok {
		t.Fatal("expired entry survived")
	}
	for i := 0; i < 1000; i++ {
		key := testKey(fmt.Sprint(i))
		c.GetOrLoad(key, load)
		c.InvalidateFolder(key.GroupID, key.ParentID)
	}
	if len(c.expiry) != len(c.entries) || len(c.expiry) > 2 || len(c.inflights) != 0 {
		t.Fatalf("metadata retained: heap=%d entries=%d loads=%d", len(c.expiry), len(c.entries), len(c.inflights))
	}
	c.Invalidate()
	if len(c.expiry) != 0 {
		t.Fatal("full invalidation retained heap entries")
	}
}

func TestConcurrentFolderAndFullInvalidation(t *testing.T) {
	c, _ := New(Options{})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := testKey(fmt.Sprint(i % 5))
				switch worker % 3 {
				case 0:
					c.Invalidate()
				case 1:
					c.InvalidateFolder(key.GroupID, key.ParentID)
				case 2:
					key.Generation = c.Generation()
					c.GetOrLoad(key, func() ([]model.RemoteEntry, error) { return nil, nil })
				}
			}
		}(worker)
	}
	wg.Wait()
}
