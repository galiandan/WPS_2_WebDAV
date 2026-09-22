package transfers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fetchSpec() Spec {
	return Spec{OwnerID: "installation", Identity: strings.Repeat("a", 64), Kind: "fetch", URL: "https://example.com/source?token=private", Name: "file.txt", Destination: "/", DestinationID: "parent-id"}
}

func archiveSpec() Spec {
	spec := fetchSpec()
	spec.Kind, spec.Name, spec.URL, spec.Destination, spec.DestinationID = "archive", "files.zip", "", "", ""
	spec.Sources = []Binding{{Path: "/folder", ID: "folder-id"}}
	return spec
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{
		File: filepath.Join(dir, "transfers.json"), Validate: func(Spec) error { return nil },
		Execute: func(context.Context, string, Spec, ProgressFn) Result { return Result{Status: 200} },
		Cleanup: func(string) error { return nil }, Prune: func([]string) error { return nil },
	}
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()
	m, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func await[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
		var value T
		return value
	}
}

func awaitTask(t *testing.T, m *Manager, id string, spec Spec, state string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := m.GetOwned(id, spec.OwnerID, spec.PolicyVersion)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == state {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := m.GetOwned(id, spec.OwnerID, spec.PolicyVersion)
	t.Fatalf("wanted state %s, got %+v", state, task)
	return Task{}
}

func readJournal(t *testing.T, file string) diskState {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var state diskState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func diskRecord(spec Spec, id, state string) *record {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return &record{Spec: copySpec(spec), Task: Task{ID: id, Kind: spec.Kind, Name: spec.Name, Destination: spec.Destination, State: state, Stage: "queued", CreatedAt: now, UpdatedAt: now}}
}

func writeJournal(t *testing.T, file string, records ...*record) {
	t.Helper()
	raw, err := json.Marshal(diskState{Version: 1, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeDurable(file, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerIsolationAndPrivateJournal(t *testing.T) {
	config := testConfig(t)
	config.Execute = func(ctx context.Context, _ string, _ Spec, _ ProgressFn) Result {
		<-ctx.Done()
		return Result{Status: 499}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	spec.OwnerID, spec.PolicyVersion = strings.Repeat("b", 32), 1<<54+17
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []struct {
		id      string
		version uint64
	}{{strings.Repeat("c", 32), spec.PolicyVersion}, {spec.OwnerID, spec.PolicyVersion + 1}, {"installation", 0}} {
		if _, err := m.GetOwned(task.ID, owner.id, owner.version); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get leaked to wrong owner: %v", err)
		}
		if _, err := m.CancelOwned(task.ID, owner.id, owner.version); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cancel leaked to wrong owner: %v", err)
		}
		if _, err := m.RetryOwned(task.ID, owner.id, owner.version); !errors.Is(err, ErrNotFound) {
			t.Fatalf("retry leaked to wrong owner: %v", err)
		}
		if list, _ := m.ListOwned(owner.id, owner.version); len(list) != 0 {
			t.Fatal("list leaked another owner's transfer")
		}
	}
	list, unavailable := m.ListOwned(spec.OwnerID, spec.PolicyVersion)
	if unavailable || len(list) != 1 {
		t.Fatalf("owned list = %v, %v", list, unavailable)
	}
	public, _ := json.Marshal(list)
	for _, secret := range []string{spec.URL, "private", spec.Identity, spec.OwnerID, spec.DestinationID, "policy_version", "owner_id"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public task contains private field %q", secret)
		}
	}
	m.Close()
	info, err := os.Stat(config.File)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions: %v, %v", info, err)
	}
	reopened := newTestManager(t, config)
	persisted, err := reopened.SpecOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	if err != nil || persisted.PolicyVersion != spec.PolicyVersion || persisted.URL != spec.URL {
		t.Fatalf("private journal changed identity: %+v, %v", persisted, err)
	}
	interrupted, _ := reopened.GetOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	if interrupted.State != "interrupted" || !interrupted.CanRetry {
		t.Fatalf("restart state: %+v", interrupted)
	}
}

func TestInstallationCredentialVersionIsBoundAndPersisted(t *testing.T) {
	config := testConfig(t)
	m := newTestManager(t, config)
	spec := fetchSpec()
	spec.PolicyVersion = 1<<54 + 31
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, m, task.ID, spec, "completed")
	if _, err := m.GetOwned(task.ID, spec.OwnerID, spec.PolicyVersion+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("previous administrator credential policy retained access: %v", err)
	}
	m.Close()
	reopened := newTestManager(t, config)
	persisted, err := reopened.SpecOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	if err != nil || persisted.PolicyVersion != spec.PolicyVersion {
		t.Fatalf("administrator policy changed during persistence: %+v, %v", persisted, err)
	}
}

func TestCancelRunningAndQueuedRetryOnce(t *testing.T) {
	config := testConfig(t)
	started := make(chan string, MaxQueued)
	config.Execute = func(ctx context.Context, id string, _ Spec, progress ProgressFn) Result {
		if err := progress(Progress{Stage: "downloading", BytesDone: 3, BytesTotal: 10}); err != nil {
			return Result{Status: 500}
		}
		started <- id
		<-ctx.Done()
		return Result{Status: 499, SafeToRetry: true}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	active, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := await(t, started); got != active.ID {
		t.Fatal("wrong active transfer")
	}
	queued, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := m.CancelOwned(queued.ID, spec.OwnerID, spec.PolicyVersion)
	if err != nil || cancelled.State != "cancelled" || !cancelled.CanRetry || cancelled.CanCancel {
		t.Fatalf("cancel queued: %+v, %v", cancelled, err)
	}
	if _, err := m.CancelOwned(active.ID, spec.OwnerID, spec.PolicyVersion); err != nil {
		t.Fatal(err)
	}
	finished := awaitTask(t, m, active.ID, spec, "cancelled")
	if finished.BytesDone != 3 || !finished.CanRetry {
		t.Fatalf("cancel running: %+v", finished)
	}
	retry, err := m.RetryOwned(active.ID, spec.OwnerID, spec.PolicyVersion)
	if err != nil || retry.ID == active.ID || retry.RetriedFrom != active.ID {
		t.Fatalf("retry: %+v, %v", retry, err)
	}
	if _, err := m.RetryOwned(active.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("duplicate retry: %v", err)
	}
	if got := await(t, started); got != retry.ID {
		t.Fatalf("cancelled queued transfer executed: %s", got)
	}
}

func TestRemoteWriteIsDurableAndCannotBeCancelledOrRetried(t *testing.T) {
	config := testConfig(t)
	uploading := make(chan error, 1)
	release := make(chan struct{})
	config.Execute = func(ctx context.Context, _ string, _ Spec, report ProgressFn) Result {
		uploading <- report(Progress{Stage: "uploading", RemoteWrite: true})
		select {
		case <-ctx.Done():
		case <-release:
		}
		return Result{Status: 500, SafeToRetry: true}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := await(t, uploading); err != nil {
		t.Fatal(err)
	}
	state := readJournal(t, config.File)
	if !state.Records[0].RemoteWrite {
		t.Fatal("upload was allowed before durable remote-write marker")
	}
	if _, err := m.CancelOwned(task.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("unsafe cancel allowed: %v", err)
	}
	close(release)
	finished := awaitTask(t, m, task.ID, spec, "interrupted")
	if finished.CanRetry || finished.CanCancel {
		t.Fatalf("uncertain upload offered replay: %+v", finished)
	}
	if _, err := m.RetryOwned(task.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("unsafe retry allowed: %v", err)
	}
	m.Close()
	reopened := newTestManager(t, config)
	restored, _ := reopened.GetOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	if restored.CanRetry || restored.State != "interrupted" {
		t.Fatalf("restart forgot remote write: %+v", restored)
	}
}

func TestCancelWinsWhileRemoteWriteValidationIsBlocked(t *testing.T) {
	config := testConfig(t)
	var validations atomic.Int32
	validating, resume := make(chan struct{}), make(chan struct{})
	config.Validate = func(Spec) error {
		if validations.Add(1) == 3 { // submit, execute, then remote-write progress
			close(validating)
			<-resume
		}
		return nil
	}
	reported := make(chan error, 1)
	config.Execute = func(_ context.Context, _ string, _ Spec, progress ProgressFn) Result {
		err := progress(Progress{Stage: "uploading", RemoteWrite: true})
		reported <- err
		return Result{Status: 499, SafeToRetry: true}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	await(t, validating)
	_, cancelErr := m.CancelOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	close(resume)
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if err := await(t, reported); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled job acquired remote-write permission: %v", err)
	}
	awaitTask(t, m, task.ID, spec, "cancelled")
	state := readJournal(t, config.File)
	if state.Records[0].RemoteWrite {
		t.Fatal("cancelled job was marked as uploading")
	}
}

func TestPersistenceFailurePreventsRemoteWrite(t *testing.T) {
	config := testConfig(t)
	started, release := make(chan struct{}), make(chan struct{})
	reported := make(chan error, 1)
	config.Execute = func(ctx context.Context, _ string, _ Spec, progress ProgressFn) Result {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return Result{Status: 499}
		}
		reported <- progress(Progress{Stage: "uploading", RemoteWrite: true})
		return Result{Status: 500, SafeToRetry: true}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	await(t, started)
	m.mu.Lock()
	m.write = func(string, string) (int64, error) { return 0, errors.New("disk full") }
	m.mu.Unlock()
	close(release)
	if err := await(t, reported); !errors.Is(err, ErrState) {
		t.Fatalf("upload allowed without journal write: %v", err)
	}
	finished := awaitTask(t, m, task.ID, spec, "interrupted")
	if finished.CanRetry || finished.CanCancel {
		t.Fatalf("failed journal permitted actions: %+v", finished)
	}
	if _, unavailable := m.ListOwned(spec.OwnerID, spec.PolicyVersion); !unavailable {
		t.Fatal("persistence failure hidden from task center")
	}
	if _, err := m.Submit(spec); !errors.Is(err, ErrState) {
		t.Fatalf("accepted another job after persistence failure: %v", err)
	}
}

func TestRestartNeverReplaysAndPrunesOnlyExpiredArtifacts(t *testing.T) {
	config := testConfig(t)
	spec, archive := fetchSpec(), archiveSpec()
	queued := diskRecord(spec, strings.Repeat("1", 32), "queued")
	running := diskRecord(spec, strings.Repeat("2", 32), "running")
	running.RemoteWrite = true
	ready := diskRecord(archive, strings.Repeat("3", 32), "completed")
	ready.Artifact = &Artifact{Name: archive.Name, Size: 128, ExpiresAt: time.Now().Add(30 * time.Minute)}
	expired := diskRecord(archive, strings.Repeat("4", 32), "completed")
	expired.Artifact = &Artifact{Name: archive.Name, Size: 128, ExpiresAt: time.Now().Add(-time.Minute)}
	writeJournal(t, config.File, queued, running, ready, expired)
	var executed atomic.Bool
	config.Execute = func(context.Context, string, Spec, ProgressFn) Result {
		executed.Store(true)
		return Result{Status: 200}
	}
	var kept []string
	config.Prune = func(ids []string) error { kept = append([]string(nil), ids...); return nil }
	m := newTestManager(t, config)
	if len(kept) != 1 || kept[0] != ready.Task.ID {
		t.Fatalf("wrong artifacts retained: %v", kept)
	}
	for _, source := range []*record{queued, running} {
		task, _ := m.GetOwned(source.Task.ID, spec.OwnerID, spec.PolicyVersion)
		if task.State != "interrupted" || task.Stage != "interrupted" || task.CanRetry == source.RemoteWrite {
			t.Fatalf("wrong restart state: %+v", task)
		}
	}
	if _, err := m.ArtifactOwned(expired.Task.ID, archive.OwnerID, archive.PolicyVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired artifact available: %v", err)
	}
	m.Close()
	if executed.Load() {
		t.Fatal("restart replayed queued/running operation")
	}
}

func TestArtifactRevalidationExpiryAndCleanupRetry(t *testing.T) {
	config := testConfig(t)
	var denied, cleanupFails atomic.Bool
	var cleaned atomic.Int32
	config.Validate = func(Spec) error {
		if denied.Load() {
			return ErrDenied
		}
		return nil
	}
	config.Execute = func(_ context.Context, _ string, spec Spec, _ ProgressFn) Result {
		return Result{Status: 200, Artifact: &Artifact{Name: spec.Name, Size: 1234}}
	}
	config.Cleanup = func(string) error {
		cleaned.Add(1)
		if cleanupFails.Load() {
			return errors.New("busy")
		}
		return nil
	}
	m := newTestManager(t, config)
	spec := archiveSpec()
	task, err := m.Submit(spec)
	if err != nil {
		t.Fatal(err)
	}
	ready := awaitTask(t, m, task.ID, spec, "completed")
	if !ready.ArtifactReady || ready.ArtifactSize != 1234 || ready.CanRetry {
		t.Fatalf("bad published artifact: %+v", ready)
	}
	artifact, err := m.ArtifactOwned(task.ID, spec.OwnerID, spec.PolicyVersion)
	if err != nil || artifact.Name != spec.Name {
		t.Fatalf("artifact: %+v, %v", artifact, err)
	}
	denied.Store(true)
	if _, err := m.ArtifactOwned(task.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked binding can read artifact: %v", err)
	}
	denied.Store(false)
	m.mu.Lock()
	m.now = func() time.Time { return artifact.ExpiresAt }
	m.mu.Unlock()
	if _, err := m.ArtifactOwned(task.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expiry waits for cleanup timer: %v", err)
	}
	cleanupFails.Store(true)
	m.expire()
	if readJournal(t, config.File).Records[0].Artifact == nil {
		t.Fatal("failed cleanup lost retry metadata")
	}
	cleanupFails.Store(false)
	m.expire()
	if readJournal(t, config.File).Records[0].Artifact != nil || cleaned.Load() != 2 {
		t.Fatal("artifact cleanup was not retried")
	}
}

func TestValidationAtExecutionProgressAndRetry(t *testing.T) {
	t.Run("execution", func(t *testing.T) {
		config := testConfig(t)
		var calls, executed atomic.Int32
		config.Validate = func(Spec) error {
			if calls.Add(1) > 1 {
				return ErrDenied
			}
			return nil
		}
		config.Execute = func(context.Context, string, Spec, ProgressFn) Result { executed.Add(1); return Result{Status: 200} }
		m := newTestManager(t, config)
		spec := fetchSpec()
		task, err := m.Submit(spec)
		if err != nil {
			t.Fatal(err)
		}
		failed := awaitTask(t, m, task.ID, spec, "failed")
		if executed.Load() != 0 || failed.CanRetry {
			t.Fatalf("revoked job ran: %+v", failed)
		}
	})
	t.Run("progress and retry", func(t *testing.T) {
		config := testConfig(t)
		var denied atomic.Bool
		config.Validate = func(Spec) error {
			if denied.Load() {
				return ErrDenied
			}
			return nil
		}
		reported := make(chan error, 1)
		config.Execute = func(_ context.Context, _ string, _ Spec, progress ProgressFn) Result {
			denied.Store(true)
			reported <- progress(Progress{Stage: "downloading"})
			return Result{Status: 403, SafeToRetry: true}
		}
		m := newTestManager(t, config)
		spec := fetchSpec()
		task, err := m.Submit(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := await(t, reported); !errors.Is(err, ErrDenied) {
			t.Fatalf("revoked progress: %v", err)
		}
		awaitTask(t, m, task.ID, spec, "failed")
		if _, err := m.RetryOwned(task.ID, spec.OwnerID, spec.PolicyVersion); !errors.Is(err, ErrDenied) {
			t.Fatalf("revoked retry: %v", err)
		}
	})
}

func TestQueueBoundAndTerminalHistoryEviction(t *testing.T) {
	config := testConfig(t)
	config.Execute = func(ctx context.Context, _ string, _ Spec, _ ProgressFn) Result {
		<-ctx.Done()
		return Result{Status: 499}
	}
	m := newTestManager(t, config)
	spec := fetchSpec()
	for i := 0; i < MaxQueued; i++ {
		if _, err := m.Submit(spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Submit(spec); !errors.Is(err, ErrBusy) {
		t.Fatalf("unbounded queue: %v", err)
	}
	m.Close()

	config = testConfig(t)
	records := make([]*record, MaxRecords)
	for i := range records {
		id := strings.Repeat("0", 30) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16])
		records[i] = diskRecord(spec, id, "failed")
	}
	writeJournal(t, config.File, records...)
	var evicted string
	config.Cleanup = func(id string) error { evicted = id; return nil }
	config.Execute = func(ctx context.Context, _ string, _ Spec, _ ProgressFn) Result {
		<-ctx.Done()
		return Result{Status: 499}
	}
	m = newTestManager(t, config)
	if _, err := m.Submit(spec); err != nil {
		t.Fatal(err)
	}
	if evicted != records[0].Task.ID {
		t.Fatalf("wrong history eviction: %s", evicted)
	}
	if list, _ := m.ListOwned(spec.OwnerID, spec.PolicyVersion); len(list) != MaxRecords {
		t.Fatalf("history length %d", len(list))
	}
}

func TestPanicAndArtifactFailuresAreCleaned(t *testing.T) {
	for _, test := range []struct {
		name    string
		spec    Spec
		execute func(context.Context, string, Spec, ProgressFn) Result
		retry   bool
	}{
		{"panic", fetchSpec(), func(context.Context, string, Spec, ProgressFn) Result { panic("https://secret.invalid/token") }, false},
		{"archive missing", archiveSpec(), func(context.Context, string, Spec, ProgressFn) Result { return Result{Status: 200} }, true},
		{"artifact too large", archiveSpec(), func(context.Context, string, Spec, ProgressFn) Result {
			return Result{Status: 200, Artifact: &Artifact{Name: "files.zip", Size: 1<<30 + 1}}
		}, true},
		{"error URL", fetchSpec(), func(context.Context, string, Spec, ProgressFn) Result {
			return Result{Status: 500, Error: "failed HTTPS://secret.invalid/token"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			config.Execute = test.execute
			cleaned := make(chan string, 1)
			config.Cleanup = func(id string) error { cleaned <- id; return nil }
			m := newTestManager(t, config)
			task, err := m.Submit(test.spec)
			if err != nil {
				t.Fatal(err)
			}
			failed := awaitTask(t, m, task.ID, test.spec, "failed")
			if failed.CanRetry != test.retry || failed.ArtifactReady || strings.Contains(failed.Error, "secret") {
				t.Fatalf("unsafe failure: %+v", failed)
			}
			if got := await(t, cleaned); got != task.ID {
				t.Fatal("missing partial-file cleanup")
			}
		})
	}
}

func TestInvalidSpecificationsAndJournalsFailClosed(t *testing.T) {
	mutations := map[string]func(*Spec){
		"owner":          func(s *Spec) { s.OwnerID = "other" },
		"policy":         func(s *Spec) { s.OwnerID = strings.Repeat("b", 32); s.PolicyVersion = 0 },
		"name traversal": func(s *Spec) { s.Name = "../file" },
		"fragment":       func(s *Spec) { s.URL += "#secret" },
		"userinfo":       func(s *Spec) { s.URL = "https://user:pass@example.com/file" },
		"scheme":         func(s *Spec) { s.URL = "file:///etc/passwd" },
		"URL space":      func(s *Spec) { s.URL = "https://example.com/a b" },
		"URL UTF8":       func(s *Spec) { s.URL = "https://example.com/\xff" },
		"destination":    func(s *Spec) { s.Destination = "/../folder" },
		"parent ID":      func(s *Spec) { s.DestinationID = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			spec := fetchSpec()
			mutate(&spec)
			if validateSpec(spec) == nil {
				t.Fatal("invalid specification accepted")
			}
		})
	}
	spec := archiveSpec()
	spec.Sources = append(spec.Sources, Binding{Path: "/folder/child", ID: "child"})
	if validateSpec(spec) == nil {
		t.Fatal("overlapping sources accepted")
	}
	for name, mutate := range map[string]func(*record){
		"timestamp":            func(r *record) { r.Task.CreatedAt = "invalid" },
		"retry ID":             func(r *record) { r.RetriedAs = "../../secret" },
		"negative bytes":       func(r *record) { r.Task.BytesDone = -1 },
		"archive remote write": func(r *record) { r.RemoteWrite = true },
		"artifact expiry": func(r *record) {
			r.Artifact = &Artifact{Name: r.Spec.Name, Size: 1, ExpiresAt: time.Now().Add(2 * ArtifactTTL)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig(t)
			r := diskRecord(archiveSpec(), strings.Repeat("a", 32), "completed")
			mutate(r)
			writeJournal(t, config.File, r)
			if m, err := New(config); !errors.Is(err, ErrState) {
				if m != nil {
					m.Close()
				}
				t.Fatalf("invalid journal accepted: %v", err)
			}
		})
	}
}
