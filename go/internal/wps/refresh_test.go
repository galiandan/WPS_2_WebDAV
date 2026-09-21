package wps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
)

type refreshOpenerFunc func(*http.Request) (*http.Response, error)

func (f refreshOpenerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// Done is consulted after a follower has joined the active attempt.
type refreshWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *refreshWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestRefreshSharedAcrossSpaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cookiePath := filepath.Join(dir, "cookie")
	if err := os.WriteFile(cookiePath, []byte("sid=initial"), 0600); err != nil {
		t.Fatal(err)
	}
	source := credentials.NewFileCredentialSource(cookiePath, "", nil, 0)
	rejected, err := source.Get()
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	opener := refreshOpenerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"sid=rotated; Path=/"}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2}, nil
	})
	coordinator := &RefreshCoordinator{}
	cfg := DefaultConfig("a")
	cfg.CredentialSource = source
	a, _ := NewClient(cfg, WithOpener(opener), WithRefreshCoordinator(coordinator))
	cfg.GroupID = "b"
	b, _ := NewClient(cfg, WithOpener(opener), WithRefreshCoordinator(coordinator))
	done := make(chan error, 2)
	run := func(c *Client, ctx context.Context) {
		ok, err := c.refreshCredentials(ctx, rejected)
		if err == nil && !ok {
			err = errors.New("session was not refreshed")
		}
		done <- err
	}
	go run(a, context.Background())
	<-started
	ctx := &refreshWaitContext{Context: context.Background(), entered: make(chan struct{})}
	go run(b, ctx)
	<-ctx.entered
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// An old 401 arriving after completion must use the newer snapshot too.
	run(b, context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("grants=%d, want one shared grant", calls.Load())
	}
}

func TestRefreshWaiterCancellationAndFailureRetry(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	cfg := DefaultConfig("a")
	cfg.CredentialSource = staticSource()
	c, _ := NewClient(cfg, WithOpener(refreshOpenerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})))
	rejected, _ := c.currentCredentials()
	done := make(chan struct{})
	go func() { defer close(done); c.refreshCredentials(context.Background(), rejected) }()
	<-entered
	parent, cancel := context.WithCancel(context.Background())
	ctx := &refreshWaitContext{Context: parent, entered: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := c.refreshCredentials(ctx, rejected); waiter <- err }()
	<-ctx.entered
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error=%v", err)
	}
	close(release)
	<-done
	ok, err := c.refreshCredentials(context.Background(), rejected)
	if ok || err != nil || calls.Load() != 2 {
		t.Fatalf("failed refresh not retryable: ok=%v err=%v calls=%d", ok, err, calls.Load())
	}
}
