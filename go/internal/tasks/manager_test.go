package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func taskFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "tasks.json")
}
func taskSpec(paths ...string) Spec {
	s := Spec{Operation: "delete", Identity: strings.Repeat("a", 64)}
	for _, p := range paths {
		s.Sources = append(s.Sources, Binding{Path: p, ID: "id" + p})
	}
	return s
}
func managerFor(t *testing.T, execute func(context.Context, Spec, Binding) Result) *Manager {
	t.Helper()
	m, err := New(Config{File: taskFile(t), Execute: execute, Validate: func(Spec) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
func awaitTask(t *testing.T, m *Manager, id string, condition func(Task) bool) Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if condition(v) {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	v, _ := m.Get(id)
	t.Fatalf("task did not reach expected state: %+v", v)
	return Task{}
}
func awaitFinal(t *testing.T, m *Manager, id string) Task {
	return awaitTask(t, m, id, func(task Task) bool { return terminal(task.State) })
}

func TestCancelRecordsActualInFlightSuccessAndRetryOnlyUnstarted(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var calls []string
	m := managerFor(t, func(ctx context.Context, spec Spec, source Binding) Result {
		mu.Lock()
		calls = append(calls, source.Path)
		first := len(calls) == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
			if ctx.Err() != nil {
				t.Error("cancel should not cancel in-flight operation")
			}
		}
		return Result{Status: 200}
	})
	created, err := m.Submit(taskSpec("/a", "/b"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancelled, err := m.Cancel(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled.CancelRequested || cancelled.Items[0].State != "running" || cancelled.Items[1].State != "cancelled" {
		t.Fatal(cancelled)
	}
	close(release)
	finished := awaitFinal(t, m, created.ID)
	if finished.State != "cancelled" || finished.Succeeded != 1 || finished.RetryableCount != 1 {
		t.Fatal(finished)
	}
	retry, err := m.Retry(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Items) != 1 || retry.Items[0].Path != "/b" {
		t.Fatal(retry)
	}
	if got := awaitFinal(t, m, retry.ID); got.Succeeded != 1 {
		t.Fatal(got)
	}
	if _, err := m.Retry(created.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("original task replayed child success: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "/a,/b" {
		t.Fatal(calls)
	}
}

func TestRestartInterruptsQueuedAndRunningWithoutReplay(t *testing.T) {
	file := taskFile(t)
	spec := taskSpec("/done", "/uncertain", "/pending")
	r := &record{Spec: spec, Task: Task{ID: strings.Repeat("a", 32), Operation: "delete", State: "running", CreatedAt: now(), Items: []Item{{Path: "/done", State: "succeeded", Started: true}, {Path: "/uncertain", State: "running", Started: true}, {Path: "/pending", State: "queued"}}}}
	data, _ := json.Marshal(diskState{Version: 1, Records: []*record{r}})
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	m, err := New(Config{File: file, Validate: func(Spec) error { return nil }, Execute: func(context.Context, Spec, Binding) Result { calls.Add(1); return Result{Status: 200} }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	view, err := m.Get(r.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "interrupted" || view.RetryableCount != 1 || view.Items[1].Retryable || !view.Items[2].Retryable || calls.Load() != 0 {
		t.Fatal(view)
	}
	retry, err := m.Retry(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Items) != 1 || retry.Items[0].Path != "/pending" {
		t.Fatal(retry)
	}
	awaitFinal(t, m, retry.ID)
	if calls.Load() != 1 {
		t.Fatalf("replayed uncertain work: %d", calls.Load())
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"state":"interrupted"`) {
		t.Fatal("restart interruption was not persisted")
	}
}

func TestUnsafeFailuresAndPanicsCannotRetry(t *testing.T) {
	m := managerFor(t, func(_ context.Context, _ Spec, source Binding) Result {
		if source.Path == "/panic" {
			panic("secret")
		}
		if source.Path == "/locked" {
			return Result{Status: 423, Error: "locked", SafeToRetry: true}
		}
		return Result{Status: 502, Error: "upstream failed"}
	})
	created, err := m.Submit(taskSpec("/timeout", "/panic", "/locked"))
	if err != nil {
		t.Fatal(err)
	}
	view := awaitFinal(t, m, created.ID)
	if view.RetryableCount != 1 || view.Items[0].Retryable || view.Items[1].Retryable || strings.Contains(view.Items[1].Error, "secret") {
		t.Fatal(view)
	}
	if view.Items[0].State != "interrupted" || view.Items[1].State != "interrupted" {
		t.Fatal(view)
	}
}

func TestPersistenceFailureNeverStartsUnrecordedItem(t *testing.T) {
	var calls atomic.Int32
	m := managerFor(t, func(context.Context, Spec, Binding) Result { calls.Add(1); return Result{Status: 200} })
	m.mu.Lock()
	m.write = func(string, string) (int64, error) { return 0, errors.New("disk full secret") }
	m.mu.Unlock()
	if _, err := m.Submit(taskSpec("/a")); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	list, failed := m.List()
	if !failed || len(list) != 0 || calls.Load() != 0 {
		t.Fatalf("list=%v failed=%t calls=%d", list, failed, calls.Load())
	}
}

func TestPersistenceFailureAfterSideEffectPreservesActualResultAndStopsQueue(t *testing.T) {
	var calls atomic.Int32
	var m *Manager
	m = managerFor(t, func(context.Context, Spec, Binding) Result {
		calls.Add(1)
		m.mu.Lock()
		m.write = func(string, string) (int64, error) { return 0, errors.New("disk full") }
		m.mu.Unlock()
		return Result{Status: 200}
	})
	created, err := m.Submit(taskSpec("/a", "/b"))
	if err != nil {
		t.Fatal(err)
	}
	view := awaitFinal(t, m, created.ID)
	_, failed := m.List()
	if !failed || view.Items[0].State != "succeeded" || view.Items[1].Started || calls.Load() != 1 {
		t.Fatalf("task=%+v failed=%t calls=%d", view, failed, calls.Load())
	}
}

func TestQueueBoundAndSingleWorkerUnderConcurrentSubmission(t *testing.T) {
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	m := managerFor(t, func(context.Context, Spec, Binding) Result {
		now := active.Add(1)
		for {
			max := maxActive.Load()
			if now <= max || maxActive.CompareAndSwap(max, now) {
				break
			}
		}
		<-release
		active.Add(-1)
		return Result{Status: 200}
	})
	var wg sync.WaitGroup
	var accepted, rejected atomic.Int32
	for i := 0; i < MaxQueued+10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Submit(taskSpec("/a"))
			if err == nil {
				accepted.Add(1)
			} else if errors.Is(err, ErrBusy) {
				rejected.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != MaxQueued || rejected.Load() != 10 {
		t.Fatalf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	close(release)
	list, _ := m.List()
	for _, task := range list {
		awaitFinal(t, m, task.ID)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("workers=%d", maxActive.Load())
	}
}

func TestRetryValidatesOriginalIdentityAndSourceBinding(t *testing.T) {
	identity := "first"
	var mu sync.Mutex
	file := taskFile(t)
	m, err := New(Config{File: file, Validate: func(spec Spec) error {
		mu.Lock()
		defer mu.Unlock()
		if identity != "first" {
			return errors.New("changed identity")
		}
		if spec.Sources[0].ID != "id/a" {
			t.Error("retry lost original source ID")
		}
		return nil
	}, Execute: func(context.Context, Spec, Binding) Result {
		return Result{Status: 423, Error: "locked", SafeToRetry: true}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	created, err := m.Submit(taskSpec("/a"))
	if err != nil {
		t.Fatal(err)
	}
	awaitFinal(t, m, created.ID)
	mu.Lock()
	identity = "second"
	mu.Unlock()
	if _, err := m.Retry(created.ID); err == nil {
		t.Fatal("retry accepted different account")
	}
}

func TestCorruptStateRejectedAndLimitsChecked(t *testing.T) {
	file := taskFile(t)
	if err := os.WriteFile(file, []byte(`{"version":55,"records":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{File: file, Validate: func(Spec) error { return nil }, Execute: func(context.Context, Spec, Binding) Result { return Result{Status: 200} }}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	for _, spec := range []Spec{taskSpec("/../escape"), taskSpec("/"), taskSpec("/" + strings.Repeat("x", MaxPathBytes)), taskSpec("/a", "/a")} {
		if validateSpec(spec) == nil {
			t.Fatal("accepted invalid spec", spec)
		}
	}
}

func TestRestartRejectsContradictoryItemStateWithoutRewritingIt(t *testing.T) {
	for _, item := range []Item{
		{State: "running", Started: false},
		{State: "succeeded", Started: false},
		{State: "failed", Started: false},
		{State: "queued", Started: true},
		{State: "cancelled", Started: true},
		{State: "completed", Started: true},
	} {
		t.Run(item.State, func(t *testing.T) {
			file := taskFile(t)
			item.Path = "/file"
			state := diskState{Version: 1, Records: []*record{{
				Spec: taskSpec("/file"),
				Task: Task{ID: strings.Repeat("a", 32), Operation: "delete", State: "running", Items: []Item{item}},
			}}}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			manager, err := New(Config{File: file, Validate: func(Spec) error { return nil }, Execute: func(context.Context, Spec, Binding) Result {
				t.Error("inconsistent persisted task executed")
				return Result{Status: 200}
			}})
			if manager != nil {
				manager.Close()
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("error = %v, want unavailable", err)
			}
			after, err := os.ReadFile(file)
			if err != nil || string(after) != string(data) {
				t.Fatalf("invalid journal was rewritten: %q, %v", after, err)
			}
		})
	}
}
