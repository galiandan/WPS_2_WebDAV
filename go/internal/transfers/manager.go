// Package transfers journals bounded background URL imports and ZIP artifacts.
// Only the executor handles file bytes; the journal never exposes source URLs.
package transfers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

const (
	MaxRecords    = 100
	MaxQueued     = 16
	MaxStateBytes = 8 << 20
	ArtifactTTL   = time.Hour
)

var (
	ErrInvalid        = errors.New("invalid transfer request")
	ErrNotFound       = errors.New("transfer not found")
	ErrDenied         = errors.New("transfer owner or storage changed")
	ErrBusy           = errors.New("transfer queue is full")
	ErrState          = errors.New("transfer persistence is unavailable")
	ErrNotRetryable   = errors.New("transfer cannot be safely retried")
	ErrNotCancellable = errors.New("upload has started and cannot be safely cancelled")
)

type Binding struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}
type Spec struct {
	OwnerID       string    `json:"owner_id"`
	PolicyVersion uint64    `json:"policy_version,string"`
	Identity      string    `json:"identity"`
	Kind          string    `json:"kind"`
	URL           string    `json:"url,omitempty"`
	Name          string    `json:"name"`
	Destination   string    `json:"destination,omitempty"`
	DestinationID string    `json:"destination_id,omitempty"`
	Sources       []Binding `json:"sources,omitempty"`
}
type Progress struct {
	Stage                 string
	BytesDone, BytesTotal int64
	FilesDone, FilesTotal int
	RemoteWrite           bool
}
type ProgressFn func(Progress) error
type Artifact struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Result struct {
	Status      int
	Error       string
	SafeToRetry bool
	Artifact    *Artifact
}
type Task struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	Name              string `json:"name"`
	Destination       string `json:"destination,omitempty"`
	State             string `json:"state"`
	Stage             string `json:"stage"`
	BytesDone         int64  `json:"bytes_done"`
	BytesTotal        int64  `json:"bytes_total"`
	FilesDone         int    `json:"files_done"`
	FilesTotal        int    `json:"files_total"`
	Error             string `json:"error,omitempty"`
	CanRetry          bool   `json:"can_retry"`
	CanCancel         bool   `json:"can_cancel"`
	ArtifactReady     bool   `json:"artifact_ready"`
	ArtifactExpiresAt string `json:"artifact_expires_at,omitempty"`
	ArtifactSize      int64  `json:"artifact_size,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	CancelRequested   bool   `json:"cancel_requested"`
	RetriedFrom       string `json:"retried_from,omitempty"`
}
type record struct {
	Task                      Task      `json:"task"`
	Spec                      Spec      `json:"spec"`
	RemoteWrite               bool      `json:"remote_write"`
	SafeRetry                 bool      `json:"safe_retry"`
	RetriedAs                 string    `json:"retried_as,omitempty"`
	Artifact                  *Artifact `json:"artifact,omitempty"`
	lastPersist, lastValidate time.Time
}
type diskState struct {
	Version int       `json:"version"`
	Records []*record `json:"records"`
}
type Config struct {
	File     string
	Validate func(Spec) error
	Execute  func(context.Context, string, Spec, ProgressFn) Result
	Cleanup  func(string) error
	Prune    func([]string) error
}
type Manager struct {
	mu                       sync.Mutex
	config                   Config
	records                  []*record
	write                    func(string, string) (int64, error)
	now                      func() time.Time
	ctx                      context.Context
	cancel                   context.CancelFunc
	activeCancel             context.CancelFunc
	activeID                 string
	wake                     chan struct{}
	done                     chan struct{}
	closed, persistenceError bool
}

func New(config Config) (*Manager, error) {
	if config.File == "" || config.Validate == nil || config.Execute == nil || config.Cleanup == nil || config.Prune == nil {
		return nil, ErrInvalid
	}
	payload, _, err := securefile.ReadJSONState(config.File, MaxStateBytes)
	if err != nil {
		return nil, ErrState
	}
	state := diskState{Version: 1, Records: make([]*record, 0)}
	if payload != nil {
		raw, _ := json.Marshal(payload)
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 || len(state.Records) > MaxRecords {
			return nil, ErrState
		}
	}
	seen := map[string]bool{}
	keep := []string{}
	now := time.Now().UTC()
	for _, r := range state.Records {
		if r == nil || !validID(r.Task.ID) || seen[r.Task.ID] || validateSpec(r.Spec) != nil || r.Task.Kind != r.Spec.Kind || r.Task.Name != r.Spec.Name || r.Task.Destination != r.Spec.Destination || !validState(r.Task.State) || !validProgress(Progress{Stage: r.Task.Stage, BytesDone: r.Task.BytesDone, BytesTotal: r.Task.BytesTotal, FilesDone: r.Task.FilesDone, FilesTotal: r.Task.FilesTotal}) || len(r.Task.Error) > 512 {
			return nil, ErrState
		}
		if !validTimestamp(r.Task.CreatedAt) || !validTimestamp(r.Task.UpdatedAt) || r.Task.RetriedFrom != "" && !validID(r.Task.RetriedFrom) || r.RetriedAs != "" && !validID(r.RetriedAs) || r.RemoteWrite && r.Spec.Kind != "fetch" {
			return nil, ErrState
		}
		seen[r.Task.ID] = true
		if !terminal(r.Task.State) {
			r.Task.State = "interrupted"
			r.Task.Stage = "interrupted"
			r.Task.Error = "server restarted before transfer finished; verify any submitted upload"
			r.SafeRetry = !r.RemoteWrite
			r.Task.UpdatedAt = now.Format(time.RFC3339Nano)
		}
		if r.RemoteWrite {
			r.SafeRetry = false
		}
		if r.Artifact != nil {
			updated, _ := time.Parse(time.RFC3339Nano, r.Task.UpdatedAt)
			if r.Spec.Kind != "archive" || r.Task.State != "completed" || !validArtifact(*r.Artifact) || r.Artifact.Name != r.Spec.Name || r.Artifact.ExpiresAt.After(updated.Add(ArtifactTTL)) {
				return nil, ErrState
			}
			if r.Artifact.ExpiresAt.After(now) {
				keep = append(keep, r.Task.ID)
			} else {
				r.Artifact = nil
			}
		}
	}
	if err := config.Prune(keep); err != nil {
		return nil, ErrState
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{config: config, records: state.Records, write: writeDurable, now: time.Now, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if err := m.persistLocked(); err != nil {
		cancel()
		return nil, err
	}
	go m.worker()
	return m, nil
}

func validID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func validTimestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero()
}
func canonicalPath(value string) bool {
	if len(value) > 4096 {
		return false
	}
	parts, err := storage.SplitRemotePath(value)
	if err != nil {
		return false
	}
	canonical, _ := storage.JoinRemotePath(parts, false)
	return canonical == value
}
func validateSpec(spec Spec) error {
	ownerValid := spec.OwnerID == "installation" || validID(spec.OwnerID) && spec.PolicyVersion > 0
	if !ownerValid || !validHash(spec.Identity) || !utf8.ValidString(spec.Name) {
		return ErrInvalid
	}
	if _, err := storage.JoinRemotePath([]string{spec.Name}, false); err != nil {
		return ErrInvalid
	}
	if spec.Kind == "fetch" {
		if len(spec.URL) > 8192 || !utf8.ValidString(spec.URL) || len(spec.Sources) != 0 || !canonicalPath(spec.Destination) || !validBindingID(spec.DestinationID) {
			return ErrInvalid
		}
		parsed, err := url.Parse(spec.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.Fragment != "" {
			return ErrInvalid
		}
		for _, c := range spec.URL {
			if c <= 32 || c == 127 {
				return ErrInvalid
			}
		}
		return nil
	}
	if spec.Kind != "archive" || spec.URL != "" || spec.Destination != "" || spec.DestinationID != "" || len(spec.Sources) == 0 || len(spec.Sources) > 100 || !strings.HasSuffix(strings.ToLower(spec.Name), ".zip") {
		return ErrInvalid
	}
	total := 0
	seen := []string{}
	for _, source := range spec.Sources {
		if !canonicalPath(source.Path) || source.Path == "/" || !validBindingID(source.ID) {
			return ErrInvalid
		}
		for _, previous := range seen {
			if source.Path == previous || strings.HasPrefix(source.Path, previous+"/") || strings.HasPrefix(previous, source.Path+"/") {
				return ErrInvalid
			}
		}
		seen = append(seen, source.Path)
		total += len(source.Path) + len(source.ID)
	}
	if total > 32<<10 {
		return ErrInvalid
	}
	return nil
}
func validBindingID(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func validProgress(p Progress) bool {
	if p.BytesDone < 0 || p.BytesTotal < 0 || p.BytesDone > 1<<40 || p.BytesTotal > 1<<40 || p.FilesDone < 0 || p.FilesTotal < 0 || p.FilesTotal > 10000 || p.FilesDone > p.FilesTotal || p.BytesTotal > 0 && p.BytesDone > p.BytesTotal {
		return false
	}
	switch p.Stage {
	case "queued", "preparing", "downloading", "uploading", "packaging", "ready", "cancelled", "interrupted", "failed":
		return true
	}
	return false
}
func validArtifact(a Artifact) bool {
	_, err := storage.JoinRemotePath([]string{a.Name}, false)
	return err == nil && strings.HasSuffix(strings.ToLower(a.Name), ".zip") && a.Size >= 0 && a.Size <= 1<<30 && !a.ExpiresAt.IsZero()
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "interrupted"
}
func validState(state string) bool { return terminal(state) || state == "queued" || state == "running" }
func copySpec(spec Spec) Spec      { spec.Sources = append([]Binding(nil), spec.Sources...); return spec }
func writeDurable(file, body string) (int64, error) {
	mtime, err := securefile.WriteAtomic(file, body)
	if err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	return mtime, dir.Sync()
}
func (m *Manager) persistLocked() error {
	raw, err := json.Marshal(diskState{Version: 1, Records: m.records})
	if err != nil || len(raw) > MaxStateBytes {
		m.persistenceError = true
		return ErrState
	}
	if _, err := m.write(m.config.File, string(raw)); err != nil {
		m.persistenceError = true
		return ErrState
	}
	return nil
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) findLocked(id string) *record {
	for _, r := range m.records {
		if r.Task.ID == id {
			return r
		}
	}
	return nil
}
func owned(r *record, owner string, version uint64) bool {
	return r != nil && r.Spec.OwnerID == owner && r.Spec.PolicyVersion == version
}

func (m *Manager) publicLocked(r *record) Task {
	out := r.Task
	out.CanRetry = terminal(out.State) && out.State != "completed" && r.SafeRetry && !r.RemoteWrite && r.RetriedAs == "" && !m.persistenceError && !m.closed
	out.CanCancel = !terminal(out.State) && !r.RemoteWrite && !out.CancelRequested && !m.closed && !m.persistenceError
	out.ArtifactReady = out.State == "completed" && r.Artifact != nil && m.now().Before(r.Artifact.ExpiresAt)
	out.ArtifactExpiresAt = ""
	out.ArtifactSize = 0
	if r.Artifact != nil {
		out.ArtifactExpiresAt = r.Artifact.ExpiresAt.Format(time.RFC3339Nano)
		out.ArtifactSize = r.Artifact.Size
	}
	return out
}
func (m *Manager) Submit(spec Spec) (Task, error) { return m.submit(spec, "") }
func (m *Manager) submit(spec Spec, retriedFrom string) (Task, error) {
	if validateSpec(spec) != nil {
		return Task{}, ErrInvalid
	}
	if err := m.config.Validate(copySpec(spec)); err != nil {
		return Task{}, ErrDenied
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return Task{}, ErrState
	}
	id := hex.EncodeToString(random)
	m.mu.Lock()
	if m.closed || m.persistenceError {
		m.mu.Unlock()
		return Task{}, ErrState
	}
	queued := 0
	for _, r := range m.records {
		if !terminal(r.Task.State) {
			queued++
		}
	}
	if queued >= MaxQueued {
		m.mu.Unlock()
		return Task{}, ErrBusy
	}
	var original *record
	if retriedFrom != "" {
		original = m.findLocked(retriedFrom)
		if !owned(original, spec.OwnerID, spec.PolicyVersion) || !m.publicLocked(original).CanRetry {
			m.mu.Unlock()
			return Task{}, ErrNotRetryable
		}
	}
	previous := append([]*record(nil), m.records...)
	evicted := ""
	if len(m.records) >= MaxRecords {
		index := -1
		for i, r := range m.records {
			if terminal(r.Task.State) && r.Task.ID != retriedFrom && (r.Artifact == nil || !m.now().Before(r.Artifact.ExpiresAt)) {
				index = i
				break
			}
		}
		if index < 0 {
			m.mu.Unlock()
			return Task{}, ErrBusy
		}
		evicted = m.records[index].Task.ID
		m.records = append(append([]*record(nil), m.records[:index]...), m.records[index+1:]...)
	}
	now := m.now().UTC().Format(time.RFC3339Nano)
	r := &record{Spec: copySpec(spec), Task: Task{ID: id, Kind: spec.Kind, Name: spec.Name, Destination: spec.Destination, State: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now, RetriedFrom: retriedFrom}}
	if original != nil {
		original.RetriedAs = id
	}
	m.records = append(m.records, r)
	if err := m.persistLocked(); err != nil {
		m.records = previous
		if original != nil {
			original.RetriedAs = ""
		}
		m.mu.Unlock()
		return Task{}, err
	}
	result := m.publicLocked(r)
	m.mu.Unlock()
	if evicted != "" {
		_ = m.config.Cleanup(evicted)
	}
	m.signal()
	return result, nil
}
func (m *Manager) ListOwned(owner string, version uint64) ([]Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0)
	for _, r := range m.records {
		if owned(r, owner, version) {
			out = append(out, m.publicLocked(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, m.persistenceError
}
func (m *Manager) GetOwned(id, owner string, version uint64) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findLocked(id)
	if !owned(r, owner, version) {
		return Task{}, ErrNotFound
	}
	return m.publicLocked(r), nil
}
func (m *Manager) SpecOwned(id, owner string, version uint64) (Spec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findLocked(id)
	if !owned(r, owner, version) {
		return Spec{}, ErrNotFound
	}
	return copySpec(r.Spec), nil
}
func (m *Manager) RetryOwned(id, owner string, version uint64) (Task, error) {
	spec, err := m.SpecOwned(id, owner, version)
	if err != nil {
		return Task{}, err
	}
	return m.submit(spec, id)
}
func (m *Manager) CancelOwned(id, owner string, version uint64) (Task, error) {
	m.mu.Lock()
	r := m.findLocked(id)
	if !owned(r, owner, version) {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if r.RemoteWrite && !terminal(r.Task.State) {
		m.mu.Unlock()
		return Task{}, ErrNotCancellable
	}
	if terminal(r.Task.State) {
		out := m.publicLocked(r)
		m.mu.Unlock()
		return out, nil
	}
	if m.closed || m.persistenceError {
		m.mu.Unlock()
		return Task{}, ErrState
	}
	previous := *r
	r.Task.CancelRequested = true
	r.Task.UpdatedAt = m.now().UTC().Format(time.RFC3339Nano)
	if r.Task.State == "queued" {
		r.Task.State = "cancelled"
		r.Task.Stage = "cancelled"
		r.SafeRetry = true
	}
	if err := m.persistLocked(); err != nil {
		*r = previous
		m.mu.Unlock()
		return Task{}, err
	}
	cancel := m.activeCancel
	if m.activeID != id {
		cancel = nil
	}
	out := m.publicLocked(r)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if out.State == "cancelled" {
		_ = m.config.Cleanup(id)
	}
	return out, nil
}
func (m *Manager) ArtifactOwned(id, owner string, version uint64) (Artifact, error) {
	spec, err := m.SpecOwned(id, owner, version)
	if err != nil {
		return Artifact{}, err
	}
	if m.config.Validate(spec) != nil {
		return Artifact{}, ErrDenied
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findLocked(id)
	if !owned(r, owner, version) || !m.publicLocked(r).ArtifactReady {
		return Artifact{}, ErrNotFound
	}
	return *r.Artifact, nil
}

func (m *Manager) worker() {
	defer close(m.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.expire()
		case <-m.wake:
		}
		for m.runNext() {
		}
	}
}
func (m *Manager) runNext() bool {
	m.mu.Lock()
	if m.closed || m.persistenceError {
		m.mu.Unlock()
		return false
	}
	var r *record
	for _, candidate := range m.records {
		if candidate.Task.State == "queued" {
			r = candidate
			break
		}
	}
	if r == nil {
		m.mu.Unlock()
		return false
	}
	r.Task.State = "running"
	r.Task.Stage = "preparing"
	r.Task.UpdatedAt = m.now().UTC().Format(time.RFC3339Nano)
	if m.persistLocked() != nil {
		m.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
	m.activeID, m.activeCancel = r.Task.ID, cancel
	spec, id := copySpec(r.Spec), r.Task.ID
	m.mu.Unlock()
	result := m.execute(ctx, id, spec)
	cancel()
	m.mu.Lock()
	m.activeCancel = nil
	m.activeID = ""
	r.Task.Error = redactError(result.Error)
	r.SafeRetry = result.SafeToRetry && !r.RemoteWrite
	finished := m.now().UTC()
	r.Task.UpdatedAt = finished.Format(time.RFC3339Nano)
	if m.closed || m.persistenceError {
		r.Task.State = "interrupted"
		r.Task.Stage = "interrupted"
		r.SafeRetry = !r.RemoteWrite
		r.Task.Error = "transfer interrupted; verify any submitted upload"
	} else if r.Task.CancelRequested && !r.RemoteWrite {
		r.Task.State = "cancelled"
		r.Task.Stage = "cancelled"
		r.SafeRetry = true
	} else if result.Status >= 200 && result.Status < 300 && result.Error == "" {
		r.Task.State = "completed"
		r.Task.Stage = "ready"
		r.SafeRetry = false
		if result.Artifact != nil {
			artifact := *result.Artifact
			if artifact.ExpiresAt.IsZero() {
				artifact.ExpiresAt = finished.Add(ArtifactTTL)
			}
			if spec.Kind != "archive" || !validArtifact(artifact) || artifact.Name != spec.Name || !artifact.ExpiresAt.After(finished) || artifact.ExpiresAt.After(finished.Add(ArtifactTTL)) {
				r.Task.State = "failed"
				r.Task.Stage = "failed"
				r.Task.Error = "invalid transfer artifact"
				r.SafeRetry = !r.RemoteWrite
			} else {
				r.Artifact = &artifact
			}
		}
	} else {
		r.Task.State = "failed"
		r.Task.Stage = "failed"
		if r.RemoteWrite {
			r.Task.State = "interrupted"
			r.Task.Stage = "interrupted"
		}
		if r.Task.Error == "" {
			r.Task.Error = "transfer failed"
		}
	}
	if spec.Kind == "archive" && r.Task.State == "completed" && r.Artifact == nil {
		r.Task.State = "failed"
		r.Task.Stage = "failed"
		r.Task.Error = "archive was not published"
		r.SafeRetry = true
	}
	keep := r.Artifact != nil && r.Task.State == "completed"
	if m.persistLocked() != nil {
		r.Task.State = "interrupted"
		r.Task.Stage = "interrupted"
		r.Artifact = nil
		keep = false
	}
	more := !m.closed && !m.persistenceError
	m.mu.Unlock()
	if !keep {
		_ = m.config.Cleanup(id)
	}
	return more
}
func (m *Manager) execute(ctx context.Context, id string, spec Spec) (result Result) {
	defer func() {
		if recover() != nil {
			result = Result{Status: 500, Error: "transfer execution failed; verify any submitted upload"}
		}
	}()
	if err := m.config.Validate(copySpec(spec)); err != nil {
		return Result{Status: 403, Error: "transfer owner or storage changed", SafeToRetry: false}
	}
	return m.config.Execute(ctx, id, spec, func(progress Progress) error { return m.report(ctx, id, progress) })
}
func (m *Manager) report(ctx context.Context, id string, p Progress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validProgress(p) {
		return ErrInvalid
	}
	m.mu.Lock()
	r := m.findLocked(id)
	if r == nil || r.Task.State != "running" || m.closed || m.persistenceError {
		m.mu.Unlock()
		return ErrState
	}
	if r.Task.CancelRequested && !r.RemoteWrite {
		m.mu.Unlock()
		return context.Canceled
	}
	validate := p.RemoteWrite || r.Task.Stage != p.Stage || m.now().Sub(r.lastValidate) >= time.Second
	spec := copySpec(r.Spec)
	m.mu.Unlock()
	if validate && m.config.Validate(spec) != nil {
		return ErrDenied
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r = m.findLocked(id)
	if r == nil || r.Task.State != "running" || m.closed || m.persistenceError {
		return ErrState
	}
	if r.Task.CancelRequested && !r.RemoteWrite {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.RemoteWrite && (r.Spec.Kind != "fetch" || p.Stage != "uploading") {
		return ErrInvalid
	}
	changed := r.Task.Stage != p.Stage
	previous := *r
	r.Task.Stage = p.Stage
	r.Task.BytesDone, r.Task.BytesTotal = p.BytesDone, p.BytesTotal
	r.Task.FilesDone, r.Task.FilesTotal = p.FilesDone, p.FilesTotal
	r.Task.UpdatedAt = m.now().UTC().Format(time.RFC3339Nano)
	if validate {
		r.lastValidate = m.now()
	}
	if p.RemoteWrite {
		r.RemoteWrite = true
		r.SafeRetry = false
	}
	if changed || p.RemoteWrite || m.now().Sub(r.lastPersist) >= time.Second {
		if err := m.persistLocked(); err != nil {
			*r = previous
			return err
		}
		r.lastPersist = m.now()
	}
	return nil
}
func redactError(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return "transfer failed; source details were withheld"
	}
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func (m *Manager) expire() {
	m.mu.Lock()
	ids := []string{}
	for _, r := range m.records {
		if r.Artifact != nil && !m.now().Before(r.Artifact.ExpiresAt) {
			ids = append(ids, r.Task.ID)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		if m.config.Cleanup(id) != nil {
			continue
		}
		m.mu.Lock()
		r := m.findLocked(id)
		if r != nil && r.Artifact != nil && !m.now().Before(r.Artifact.ExpiresAt) {
			r.Artifact = nil
			_ = m.persistLocked()
		}
		m.mu.Unlock()
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.done
		return
	}
	m.closed = true
	m.cancel()
	for _, r := range m.records {
		if r.Task.State == "queued" {
			r.Task.State = "interrupted"
			r.Task.Stage = "interrupted"
			r.SafeRetry = true
			r.Task.Error = "server stopped before transfer started"
			r.Task.UpdatedAt = m.now().UTC().Format(time.RFC3339Nano)
		}
	}
	_ = m.persistLocked()
	m.mu.Unlock()
	m.signal()
	<-m.done
}
