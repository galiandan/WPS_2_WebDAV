package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

type refreshTestOpener func(*http.Request) (*http.Response, error)

func (f refreshTestOpener) Do(r *http.Request) (*http.Response, error) { return f(r) }

type refreshFollowerContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *refreshFollowerContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestMountedClientSharesRefreshWithBase(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.CookieFile = filepath.Join(filepath.Dir(cfg.WorkspaceFile), "cookie")
	if err := os.WriteFile(cfg.CookieFile, []byte("sid=initial"), 0600); err != nil {
		t.Fatal(err)
	}
	grantStarted, release := make(chan struct{}), make(chan struct{})
	var grants atomic.Int32
	opener := refreshTestOpener(func(r *http.Request) (*http.Response, error) {
		status := 200
		header := make(http.Header)
		if strings.Contains(r.URL.Path, "grant_token") {
			if grants.Add(1) == 1 {
				close(grantStarted)
			}
			<-release
			header.Set("Set-Cookie", "sid=rotated; Path=/")
		} else if r.Header.Get("Cookie") == "sid=initial" {
			status = 401
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2}, nil
	})
	app, err := New(cfg, "test", WithTransports(opener, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	mount, err := app.spaceFactory()("other-group")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	request := wps.JSONRequest{Path: "/probe", RetryOn401: true}
	go func() { _, err := app.Client.RequestJSON(request); done <- err }()
	<-grantStarted
	ctx := &refreshFollowerContext{Context: context.Background(), entered: make(chan struct{})}
	go func() { _, err := mount.Lister.(*wps.Client).RequestJSONContext(ctx, request); done <- err }()
	<-ctx.entered
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if grants.Load() != 1 {
		t.Fatalf("base and mount issued %d grants", grants.Load())
	}
}
