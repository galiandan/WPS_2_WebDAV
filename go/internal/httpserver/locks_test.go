package httpserver

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// newTimedLockStore returns a store driven by a controllable clock plus the
// function that advances it.
func newTimedLockStore(t *testing.T, maxLocks int) (*DavLockStore, func(time.Duration)) {
	t.Helper()
	store, err := NewDavLockStore(DefaultLockMaxTimeout, maxLocks)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Unix(0, 0)
	var mu sync.Mutex
	store.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(step time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(step)
	}
	return store, advance
}

func TestTokensFromHeadersExtraction(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    []string
	}{
		{name: "empty", headers: nil, want: []string{}},
		{name: "bracketed", headers: []string{"<opaquelocktoken:abc>"}, want: []string{"opaquelocktoken:abc"}},
		{name: "bare", headers: []string{"opaquelocktoken:abc"}, want: []string{"opaquelocktoken:abc"}},
		{name: "angle-stripped with spaces", headers: []string{"  <opaquelocktoken:abc>> "}, want: []string{"opaquelocktoken:abc"}},
		{name: "case-insensitive scheme", headers: []string{"<OPAQUELOCKTOKEN:abc>"}, want: []string{"OPAQUELOCKTOKEN:abc"}},
		{name: "if list", headers: []string{"<opaquelocktoken:a> (<opaquelocktoken:b>)"}, want: []string{"opaquelocktoken:a", "opaquelocktoken:b", "opaquelocktoken:a> (<opaquelocktoken:b>)"}},
		{name: "both headers", headers: []string{"<opaquelocktoken:a>", "<opaquelocktoken:b>"}, want: []string{"opaquelocktoken:a", "opaquelocktoken:b"}},
		{name: "other schemes ignored", headers: []string{"<urn:lock:a> <opaquelocktoken:keep>"}, want: []string{"opaquelocktoken:keep"}},
		{name: "non token survives untouched", headers: []string{"just text"}, want: []string{}},
	}
	for _, tc := range cases {
		tokens := TokensFromHeaders(tc.headers...)
		if len(tokens) != len(tc.want) {
			t.Fatalf("%s tokens = %v, want %v", tc.name, tokens, tc.want)
		}
		for _, want := range tc.want {
			if _, ok := tokens[want]; !ok {
				t.Fatalf("%s tokens = %v, want %q present", tc.name, tokens, want)
			}
		}
	}
}

func TestLockStoreAppliesExactAndInfinityRules(t *testing.T) {
	store, advance := newTimedLockStore(t, DefaultLockMaxLocks)
	if _, err := store.Acquire("/a/b", "0", "owner", 600, ""); err != nil {
		t.Fatal(err)
	}
	// A depth 0 lock covers only the exact path.
	if store.Allows("/a/b", nil) {
		t.Fatal("the exact path must be locked")
	}
	if !store.Allows("/a/b/c", nil) {
		t.Fatal("a depth 0 lock must not cover descendants")
	}
	if !store.Allows("/a", nil) {
		t.Fatal("a depth 0 lock must not cover ancestors")
	}

	// A depth infinity lock covers descendants but not siblings.
	store2, _ := newTimedLockStore(t, DefaultLockMaxLocks)
	deep, err := store2.Acquire("/a", "infinity", "owner", 600, "")
	if err != nil {
		t.Fatal(err)
	}
	if store2.Allows("/a/b", nil) || store2.Allows("/a", nil) {
		t.Fatal("the infinity lock must cover the tree")
	}
	if !store2.Allows("/ab", nil) {
		t.Fatal("a sibling prefix must not be covered")
	}

	// The holder's token releases every covered path.
	tokens := map[string]struct{}{deep.Token: {}}
	if !store2.Allows("/a/b", tokens) {
		t.Fatal("the holder token must release descendants")
	}
	if !store2.Allows("/a", tokens) {
		t.Fatal("the holder token must release the locked path")
	}
	if store2.Allows("/a/b", map[string]struct{}{"opaquelocktoken:other": {}}) {
		t.Fatal("a foreign token must not release the lock")
	}

	// Locks expire by the monotonic clock and purging happens on operations.
	if _, err := store.Acquire("/x", "0", "owner", 30, ""); err != nil {
		t.Fatal(err)
	}
	advance(31 * time.Second)
	if !store.Allows("/x", nil) {
		t.Fatal("the expired lock must vanish")
	}
	if _, err := store.Acquire("/x", "0", "owner", 30, ""); err != nil {
		t.Fatalf("re-acquire after expiry failed: %v", err)
	}
}

func TestLockStoreAcquireConflictsAndClamps(t *testing.T) {
	store, advance := newTimedLockStore(t, DefaultLockMaxLocks)
	first, err := store.Acquire("/a/b", "0", "owner-1", 600, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.TimeoutSeconds != 600 {
		t.Fatalf("timeout = %d", first.TimeoutSeconds)
	}
	if !strings.HasPrefix(first.Token, "opaquelocktoken:") {
		t.Fatalf("token = %q", first.Token)
	}
	// An exact lock on the same path conflicts, whatever the depth.
	if _, err := store.Acquire("/a/b", "infinity", "owner-2", 600, ""); !errors.Is(err, errLockConflict) {
		t.Fatalf("same-path conflict error = %v", err)
	}
	// An infinity lock over a locked descendant conflicts both ways.
	if _, err := store.Acquire("/a", "infinity", "owner-2", 600, ""); !errors.Is(err, errLockConflict) {
		t.Fatalf("ancestor conflict error = %v", err)
	}
	// A sibling stays free.
	if _, err := store.Acquire("/a/c", "0", "owner-2", 600, ""); err != nil {
		t.Fatalf("sibling acquire failed: %v", err)
	}

	// Timeout clamping: above the max, below one, and Infinite.
	active, err := store.Acquire("/t1", "0", "owner", DefaultLockMaxTimeout*2, "")
	if err != nil || active.TimeoutSeconds != DefaultLockMaxTimeout {
		t.Fatalf("huge timeout = (%+v, %v)", active, err)
	}
	active, err = store.Acquire("/t2", "0", "owner", 0, "")
	if err != nil || active.TimeoutSeconds != 1 {
		t.Fatalf("zero timeout = (%+v, %v)", active, err)
	}

	// Refresh renews the same token on the same path and nothing else.
	advance(100 * time.Second)
	refreshed, err := store.Acquire("/a/b", "0", "ignored", 900, first.Token)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != first.Token || refreshed.TimeoutSeconds != 900 || refreshed.Owner != "owner-1" {
		t.Fatalf("refreshed lock = %+v", refreshed)
	}
	if _, err := store.Acquire("/other", "0", "owner", 600, first.Token); !errors.Is(err, errLockTokenInvalid) {
		t.Fatalf("wrong-path refresh error = %v", err)
	}
	if _, err := store.Acquire("/a/b", "0", "owner", 600, "opaquelocktoken:missing"); !errors.Is(err, errLockTokenInvalid) {
		t.Fatalf("unknown refresh token error = %v", err)
	}
	// A refresh must not renew a token that expired during the wait.
	store2, advance2 := newTimedLockStore(t, DefaultLockMaxLocks)
	doomed, _ := store2.Acquire("/p", "0", "owner", 10, "")
	advance2(11 * time.Second)
	if _, err := store2.Acquire("/p", "0", "owner", 10, doomed.Token); !errors.Is(err, errLockTokenInvalid) {
		t.Fatalf("expired refresh error = %v", err)
	}
}

func TestLockStoreUnlockRequiresExactPath(t *testing.T) {
	store, _ := newTimedLockStore(t, DefaultLockMaxLocks)
	active, err := store.Acquire("/a", "infinity", "owner", 600, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unlock("/a/b", active.Token); !errors.Is(err, errLockTokenInvalid) {
		t.Fatalf("descendant unlock error = %v", err)
	}
	if err := store.Unlock("/a", "opaquelocktoken:missing"); !errors.Is(err, errLockTokenInvalid) {
		t.Fatalf("unknown token unlock error = %v", err)
	}
	if err := store.Unlock("/a", active.Token); err != nil {
		t.Fatalf("unlock failed: %v", err)
	}
	if !store.Allows("/a", nil) {
		t.Fatal("the path must be free after the unlock")
	}
	if err := store.Unlock("/a", active.Token); !errors.Is(err, errLockTokenInvalid) {
		t.Fatal("a token must not unlock twice")
	}
}

func TestLockStoreCapsRegistrySize(t *testing.T) {
	store, _ := newTimedLockStore(t, 2)
	if _, err := store.Acquire("/a", "0", "o", 60, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire("/b", "0", "o", 60, ""); err != nil {
		t.Fatal(err)
	}
	_, err := store.Acquire("/c", "0", "o", 60, "")
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindServiceBusy || storageErr.Message != "too many active WebDAV locks" {
		t.Fatalf("cap error = %v", err)
	}
	// Expiry frees capacity on the next operation.
	store.now = func() time.Time { return time.Unix(0, 0).Add(61 * time.Second) }
	if _, err := store.Acquire("/c", "0", "o", 60, ""); err != nil {
		t.Fatalf("acquire after expiry failed: %v", err)
	}
}

func TestLockStoreIsRaceSafe(t *testing.T) {
	store, advance := newTimedLockStore(t, DefaultLockMaxLocks)
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 50; round++ {
				path := string(rune('a'+worker%8)) + string(rune('a'+round%8))
				active, err := store.Acquire("/"+path, "0", "owner", 1, "")
				if err == nil {
					store.Unlock("/"+path, active.Token)
				}
				store.Allows("/"+path, nil)
				TokensFromHeaders("<opaquelocktoken:probe>")
			}
		}(worker)
	}
	advance(0)
	wg.Wait()
}
