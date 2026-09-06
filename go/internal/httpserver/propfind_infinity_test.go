package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// The B703 completion gate: the infinity walk stays bounded and ordered
// across 1/1000/10000 entries, deep trees, cycles, over-limit answers,
// and client disconnects.

func propfindManyFiles(count int) []model.RemoteEntry {
	children := make([]model.RemoteEntry, 0, count)
	for i := 0; i < count; i++ {
		children = append(children, propfindFile("f"+strconv.Itoa(i), fmt.Sprintf("c%04d.txt", i), "e"+strconv.Itoa(i)))
	}
	return children
}

// TestPropfindInfinitySingleEntry: one file answers exactly one response.
func TestPropfindInfinitySingleEntry(t *testing.T) {
	storage := &propfindStorage{
		root:       propfindFile("only", "only.txt", "e-only"),
		listByPath: map[string][]model.RemoteEntry{},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/only.txt", "infinity")
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	got := hrefs(recorder.Body.String())
	if len(got) != 1 || got[0] != "/dav/only.txt" {
		t.Fatalf("hrefs = %v", got)
	}
}

// TestPropfindInfinityThousandEntries keeps the depth-first pre-order:
// root first, then the children in listing order.
func TestPropfindInfinityThousandEntries(t *testing.T) {
	storage := &propfindStorage{
		root:       propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{"/": propfindManyFiles(1000)},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "infinity")
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d (body %d bytes)", recorder.Code, recorder.Body.Len())
	}
	got := hrefs(recorder.Body.String())
	if len(got) != 1001 {
		t.Fatalf("href count = %d, want 1001", len(got))
	}
	if got[0] != "/dav/" || got[1] != "/dav/c0000.txt" || got[1000] != "/dav/c0999.txt" {
		t.Errorf("order drifted: first=%q second=%q last=%q", got[0], got[1], got[1000])
	}
}

// TestPropfindEntryLimitBoundary: exactly 10000 entries answer 207, one
// more hits the configured limit with the fixed 507.
func TestPropfindEntryLimitBoundary(t *testing.T) {
	within := &propfindStorage{
		root:       propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{"/": propfindManyFiles(9999)},
	}
	recorder := servePropfind(t, newPropfindRouter(t, within, nil), "/dav/", "infinity")
	if recorder.Code != http.StatusMultiStatus || len(hrefs(recorder.Body.String())) != 10000 {
		t.Fatalf("within limit: status = %d hrefs = %d", recorder.Code, len(hrefs(recorder.Body.String())))
	}
	over := &propfindStorage{
		root:       propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{"/": propfindManyFiles(10000)},
	}
	recorder = servePropfind(t, newPropfindRouter(t, over, nil), "/dav/", "infinity")
	if recorder.Code != http.StatusInsufficientStorage || recorder.Body.String() != "PROPFIND exceeds the configured entry limit\n" {
		t.Fatalf("over limit: status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestPropfindDepthLimitDeepTree walks a 65-level folder chain: the level
// 64 folder still answers, recursing past the configured depth 507s.
func TestPropfindDepthLimitDeepTree(t *testing.T) {
	storage := &propfindStorage{
		root:       propfindRootEntry(),
		listByPath: map[string][]model.RemoteEntry{"/": {propfindFolder("f1", "f1", "e1")}},
	}
	childrenByEntry := map[string][]model.RemoteEntry{}
	for level := 1; level < 65; level++ {
		child := propfindFolder(fmt.Sprintf("f%d", level+1), fmt.Sprintf("f%d", level+1), fmt.Sprintf("e%d", level+1))
		if level == 64 {
			// The deepest folder is a file; its children never matter.
			child.Kind = model.KindFile
		}
		childrenByEntry[fmt.Sprintf("f%d", level)] = []model.RemoteEntry{child}
	}
	storage.childrenByEntry = childrenByEntry
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "infinity")
	if recorder.Code != http.StatusInsufficientStorage || recorder.Body.String() != "PROPFIND exceeds the configured depth limit\n" {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestPropfindCycleIsUpstreamError: folders listing each other repeat an
// entry ID, which is an upstream integrity failure with the fixed 502.
func TestPropfindCycleIsUpstreamError(t *testing.T) {
	folderA := propfindFolder("a", "a", "ea")
	folderB := propfindFolder("b", "b", "eb")
	storage := &propfindStorage{
		root:            propfindRootEntry(),
		listByPath:      map[string][]model.RemoteEntry{"/": {folderA}},
		childrenByEntry: map[string][]model.RemoteEntry{"a": {folderB}, "b": {folderA}},
	}
	recorder := servePropfind(t, newPropfindRouter(t, storage, nil), "/dav/", "infinity")
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "upstream WPS request failed\n" {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestPropfindDisconnectAbortsResponse: a canceled request context stops
// the walk and no response is written at all — Python's
// _ClientDisconnected behavior.
func TestPropfindDisconnectAbortsResponse(t *testing.T) {
	t.Run("canceled before the walk", func(t *testing.T) {
		storage := &propfindStorage{
			root:       propfindRootEntry(),
			listByPath: map[string][]model.RemoteEntry{"/": propfindManyFiles(10)},
		}
		router := newPropfindRouter(t, storage, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := newTestRequest("PROPFIND", "/dav/").WithContext(ctx)
		request.Header.Set("Depth", "infinity")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
			t.Fatalf("aborted walk wrote a response: %d %v", recorder.Body.Len(), recorder.Header())
		}
	})
	t.Run("canceled mid walk", func(t *testing.T) {
		folderA := propfindFolder("a", "a", "ea")
		storage := &propfindStorage{
			root:            propfindRootEntry(),
			listByPath:      map[string][]model.RemoteEntry{"/": {folderA}},
			childrenByEntry: map[string][]model.RemoteEntry{"a": {propfindFile("b", "b.txt", "eb")}},
		}
		ctx, cancel := context.WithCancel(context.Background())
		storage.onChildren = func(entryID string) {
			if entryID == "a" {
				cancel()
			}
		}
		router := newPropfindRouter(t, storage, nil)
		request := newTestRequest("PROPFIND", "/dav/").WithContext(ctx)
		request.Header.Set("Depth", "infinity")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
			t.Fatalf("aborted walk wrote a response: %d %v", recorder.Body.Len(), recorder.Header())
		}
	})
}
