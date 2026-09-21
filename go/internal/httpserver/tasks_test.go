package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/tasks"
)

type taskStorageFake struct {
	*batchStorageFake
	mu         sync.Mutex
	boundCalls [][6]string
	start      chan struct{}
	release    chan struct{}
	err        error
}

func (s *taskStorageFake) ApplyBoundBatch(ctx context.Context, op, source, destination, sourceID, parentID string) error {
	s.mu.Lock()
	s.boundCalls = append(s.boundCalls, [6]string{op, source, destination, sourceID, parentID, ""})
	first := len(s.boundCalls) == 1
	s.mu.Unlock()
	if first && s.start != nil {
		close(s.start)
		<-s.release
	}
	return s.err
}
func taskHTTPDispatcher(t *testing.T, s *taskStorageFake, identity func() ([32]byte, error)) *RESTDispatcher {
	t.Helper()
	d := batchDispatcher(t, s.batchStorageFake)
	d.mutations = s
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if identity == nil {
		identity = func() ([32]byte, error) { return [32]byte{1}, nil }
	}
	if err := d.EnableTasks(filepath.Join(dir, "tasks.json"), identity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.CloseTasks)
	return d
}
func taskHTTP(t *testing.T, d *RESTDispatcher, method, target, body string) (*httptest.ResponseRecorder, tasks.Task) {
	t.Helper()
	r := writeRequest(method, target, nil, body)
	w := httptest.NewRecorder()
	readRouter(t, d).ServeHTTP(w, r)
	var payload struct {
		Task tasks.Task `json:"task"`
	}
	if w.Code < 300 && json.Unmarshal(w.Body.Bytes(), &payload) != nil {
		t.Fatalf("invalid task JSON: %s", w.Body.String())
	}
	return w, payload.Task
}
func awaitHTTPTask(t *testing.T, d *RESTDispatcher, id string) tasks.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := d.tasks.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != "queued" && task.State != "running" {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("task remained active")
	return tasks.Task{}
}

func TestTaskRoutesPersistSourceBindingReportProgressAndCancel(t *testing.T) {
	s := &taskStorageFake{batchStorageFake: &batchStorageFake{entries: map[string]model.RemoteEntry{"/a": fileEntry("source-a", "a"), "/b": fileEntry("source-b", "b"), "/target": folderEntry("destination-id")}}, start: make(chan struct{}), release: make(chan struct{})}
	d := taskHTTPDispatcher(t, s, nil)
	w, created := taskHTTP(t, d, "POST", "/api/v1/tasks", `{"operation":"move","paths":["/a","/b"],"destination":"/target"}`)
	if w.Code != 202 || created.Total != 2 || created.Destination != "/target" {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	<-s.start
	w, cancelled := taskHTTP(t, d, "POST", "/api/v1/tasks/"+created.ID+"/cancel", "")
	if w.Code != 200 || !cancelled.CancelRequested || cancelled.Items[1].State != "cancelled" {
		t.Fatal(w.Body.String())
	}
	close(s.release)
	final := awaitHTTPTask(t, d, created.ID)
	if final.Succeeded != 1 || final.RetryableCount != 1 {
		t.Fatal(final)
	}
	w, retry := taskHTTP(t, d, "POST", "/api/v1/tasks/"+created.ID+"/retry", "")
	if w.Code != 202 || len(retry.Items) != 1 || retry.Items[0].Path != "/b" {
		t.Fatal(w.Body.String())
	}
	awaitHTTPTask(t, d, retry.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.boundCalls) != 2 || s.boundCalls[0][3] != "source-a" || s.boundCalls[1][3] != "source-b" || s.boundCalls[0][4] != "destination-id" {
		t.Fatal(s.boundCalls)
	}
	w, _ = taskHTTP(t, d, "GET", "/api/v1/tasks", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "source-a") || strings.Contains(w.Body.String(), "destination-id") || strings.Contains(w.Body.String(), "identity") {
		t.Fatalf("private task binding leaked: %s", w.Body.String())
	}
}

func TestQueuedTaskRefusesChangedAccountBeforeStartingNextItem(t *testing.T) {
	s := &taskStorageFake{batchStorageFake: &batchStorageFake{entries: map[string]model.RemoteEntry{"/a": fileEntry("a", "a"), "/b": fileEntry("b", "b")}}, start: make(chan struct{}), release: make(chan struct{})}
	var identityMu sync.Mutex
	identity := [32]byte{1}
	d := taskHTTPDispatcher(t, s, func() ([32]byte, error) { identityMu.Lock(); defer identityMu.Unlock(); return identity, nil })
	w, created := taskHTTP(t, d, "POST", "/api/v1/tasks", `{"operation":"delete","paths":["/a","/b"]}`)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	<-s.start
	identityMu.Lock()
	identity = [32]byte{2}
	identityMu.Unlock()
	close(s.release)
	final := awaitHTTPTask(t, d, created.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.boundCalls) != 1 || final.Succeeded != 1 || final.Items[1].Retryable || !strings.Contains(final.Items[1].Error, "configuration changed") {
		t.Fatalf("calls=%v task=%+v", s.boundCalls, final)
	}
}

func TestTaskExecutionHonorsCurrentLocksAndRedactsUncertainFailures(t *testing.T) {
	for _, locked := range []bool{true, false} {
		t.Run(map[bool]string{true: "locked", false: "upstream failure"}[locked], func(t *testing.T) {
			s := &taskStorageFake{batchStorageFake: &batchStorageFake{entries: map[string]model.RemoteEntry{"/a": fileEntry("a", "a")}}, err: model.NewWpsAPIError("secret signed URL", 503, model.WpsCategoryHTTP)}
			d := taskHTTPDispatcher(t, s, nil)
			if locked {
				if _, err := d.locks.Acquire("/a", "0", "", 3600, ""); err != nil {
					t.Fatal(err)
				}
			}
			w, created := taskHTTP(t, d, "POST", "/api/v1/tasks", `{"operation":"delete","paths":["/a"]}`)
			if w.Code != 202 {
				t.Fatal(w.Body.String())
			}
			final := awaitHTTPTask(t, d, created.ID)
			if strings.Contains(final.Items[0].Error, "secret") {
				t.Fatal(final)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if locked {
				if len(s.boundCalls) != 0 || final.Items[0].Status != 423 || !final.Items[0].Retryable {
					t.Fatal(final)
				}
			} else {
				if final.Items[0].Status != 502 || final.Items[0].Retryable || final.Items[0].State != "interrupted" {
					t.Fatal(final)
				}
			}
		})
	}
}

func TestTasksRequireAuthenticationAndSameOrigin(t *testing.T) {
	s := &taskStorageFake{batchStorageFake: &batchStorageFake{}}
	d := taskHTTPDispatcher(t, s, nil)
	chain, err := NewChain(ChainConfig{Router: readRouter(t, d), Health: func(http.ResponseWriter, *http.Request) {}, Auth: BasicAuthConfig{Username: "user", Password: "pass"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, authenticated := range []bool{false, true} {
		r := writeRequest("POST", "/api/v1/tasks", nil, `{"operation":"delete","paths":["/a"]}`)
		want := 401
		if authenticated {
			r.SetBasicAuth("user", "pass")
			r.Header.Set("Origin", "https://foreign.example")
			want = 403
		}
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	}
}
