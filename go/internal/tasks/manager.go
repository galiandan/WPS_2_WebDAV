// Package tasks persists bounded, explicitly retried background file work.
// It never replays queued or uncertain work automatically after a restart.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
)

const (
	MaxRecords       = 100
	MaxQueued        = 16
	MaxItems         = 100
	MaxPathBytes     = 4096
	MaxTaskPathBytes = 32 << 10
	MaxStateBytes    = 16 << 20
)

var (
	ErrUnavailable  = errors.New("task persistence is unavailable; no new work will start")
	ErrBusy         = errors.New("task queue is full")
	ErrNotFound     = errors.New("task not found")
	ErrInvalid      = errors.New("invalid task request")
	ErrNotRetryable = errors.New("task has no safely retryable items")
)

type Binding struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}
type Spec struct {
	OwnerID       string    `json:"owner_id,omitempty"`
	PolicyVersion uint64    `json:"policy_version,omitempty"`
	Operation     string    `json:"operation"`
	Destination   string    `json:"destination,omitempty"`
	DestinationID string    `json:"destination_id,omitempty"`
	Identity      string    `json:"identity"`
	Sources       []Binding `json:"sources"`
}
type Result struct {
	Status      int
	Error       string
	SafeToRetry bool
}
type Item struct {
	Path      string `json:"path"`
	State     string `json:"state"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	Started   bool   `json:"started"`
	Retryable bool   `json:"retryable"`
	RetriedAs string `json:"retried_as,omitempty"`
}
type Task struct {
	ID              string `json:"id"`
	Operation       string `json:"operation"`
	Destination     string `json:"destination,omitempty"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	CancelRequested bool   `json:"cancel_requested"`
	RetriedFrom     string `json:"retried_from,omitempty"`
	Items           []Item `json:"items"`
	Total           int    `json:"total"`
	Completed       int    `json:"completed"`
	Succeeded       int    `json:"succeeded"`
	Failed          int    `json:"failed"`
	RetryableCount  int    `json:"retryable_count"`
}
type record struct {
	Task Task `json:"task"`
	Spec Spec `json:"spec"`
}
type diskState struct {
	Version int       `json:"version"`
	Records []*record `json:"records"`
}

type Config struct {
	File     string
	Execute  func(context.Context, Spec, Binding) Result
	Validate func(Spec) error
}
type Manager struct {
	mu               sync.Mutex
	file             string
	records          []*record
	execute          func(context.Context, Spec, Binding) Result
	validate         func(Spec) error
	write            func(string, string) (int64, error)
	wake             chan struct{}
	done             chan struct{}
	ctx              context.Context
	cancel           context.CancelFunc
	closed           bool
	persistenceError bool
}

func New(config Config) (*Manager, error) {
	if config.File == "" || config.Execute == nil || config.Validate == nil {
		return nil, ErrInvalid
	}
	if err := securefile.ValidateStatePath(config.File); err != nil {
		return nil, ErrUnavailable
	}
	payload, _, err := securefile.ReadJSONState(config.File, MaxStateBytes)
	if err != nil {
		return nil, ErrUnavailable
	}
	state := diskState{Version: 1, Records: make([]*record, 0)}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil || json.Unmarshal(data, &state) != nil || state.Version != 1 || len(state.Records) > MaxRecords {
			return nil, ErrUnavailable
		}
	}
	seen := make(map[string]bool)
	for _, record := range state.Records {
		if record == nil || !validID(record.Task.ID) || seen[record.Task.ID] || validateSpec(record.Spec) != nil || len(record.Task.Items) != len(record.Spec.Sources) {
			return nil, ErrUnavailable
		}
		seen[record.Task.ID] = true
		if record.Task.Operation != record.Spec.Operation || record.Task.Destination != record.Spec.Destination {
			return nil, ErrUnavailable
		}
		for i, item := range record.Task.Items {
			if item.Path != record.Spec.Sources[i].Path || !validPersistedItem(item) || len(item.Error) > 512 {
				return nil, ErrUnavailable
			}
		}
		if !terminal(record.Task.State) && record.Task.State != "queued" && record.Task.State != "running" {
			return nil, ErrUnavailable
		}
		if !terminal(record.Task.State) {
			record.Task.State = "interrupted"
			record.Task.UpdatedAt = now()
			for i := range record.Task.Items {
				item := &record.Task.Items[i]
				if item.State == "queued" || item.State == "running" {
					item.State = "interrupted"
					if item.Started {
						item.Error = "server stopped while this item was running; verify its result before creating new work"
					} else {
						item.Error = "server stopped before this item started"
					}
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{file: config.File, records: state.Records, execute: config.Execute, validate: config.Validate, write: writeDurable, wake: make(chan struct{}, 1), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	if err := m.persistLocked(); err != nil {
		cancel()
		return nil, err
	}
	go m.worker()
	return m, nil
}

// Sync the parent after the already-fsynced 0600 temporary file is renamed.
// Starting a remote mutation depends on this journal transition being durable.
func writeDurable(file, content string) (int64, error) {
	mtime, err := securefile.WriteAtomic(file, content)
	if err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return 0, err
	}
	return mtime, nil
}

func validateSpec(spec Spec) error {
	if spec.OwnerID != "" && spec.OwnerID != "installation" && (!validID(spec.OwnerID) || spec.PolicyVersion == 0) {
		return ErrInvalid
	}
	if spec.Operation != "copy" && spec.Operation != "move" && spec.Operation != "delete" {
		return ErrInvalid
	}
	if len(spec.Identity) != 64 || len(spec.Sources) == 0 || len(spec.Sources) > MaxItems {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(spec.Identity); err != nil {
		return ErrInvalid
	}
	if spec.Operation != "delete" && (spec.DestinationID == "" || len(spec.DestinationID) > 4096 || !validPath(spec.Destination)) {
		return ErrInvalid
	}
	if spec.Operation == "delete" && (spec.Destination != "" || spec.DestinationID != "") {
		return ErrInvalid
	}
	total := len(spec.Destination)
	seen := make(map[string]bool)
	for _, source := range spec.Sources {
		if source.Path == "/" || !validPath(source.Path) || source.ID == "" || len(source.ID) > 4096 || seen[source.Path] {
			return ErrInvalid
		}
		seen[source.Path] = true
		total += len(source.Path) + len(source.ID)
	}
	if total > MaxTaskPathBytes {
		return ErrInvalid
	}
	return nil
}
func validPath(path string) bool {
	if !strings.HasPrefix(path, "/") || len(path) > MaxPathBytes || !utf8.ValidString(path) {
		return false
	}
	for _, c := range path {
		if c < 32 || c == 127 || c == '\\' {
			return false
		}
	}
	if path == "/" {
		return true
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "interrupted"
}
func validPersistedItem(item Item) bool {
	// State and the durable start marker must agree. Treating a corrupt
	// running record as unstarted could make uncertain remote work retryable.
	switch item.State {
	case "queued", "cancelled":
		return !item.Started
	case "running", "succeeded", "failed":
		return item.Started
	case "interrupted":
		return true // Either an uncertain attempt or work never started.
	default:
		return false // "completed" belongs only to the containing task.
	}
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (m *Manager) Submit(spec Spec) (Task, error) { return m.submit(spec, "") }
func (m *Manager) submit(spec Spec, prior string) (Task, error) {
	if err := validateSpec(spec); err != nil {
		return Task{}, err
	}
	if err := m.validate(spec); err != nil {
		return Task{}, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Task{}, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.persistenceError {
		return Task{}, ErrUnavailable
	}
	queued := 0
	for _, r := range m.records {
		if !terminal(r.Task.State) {
			queued++
		}
	}
	if queued >= MaxQueued {
		return Task{}, ErrBusy
	}
	var parent *record
	if prior != "" {
		parent = m.find(prior)
		if parent == nil {
			return Task{}, ErrNotFound
		}
		eligible := public(parent.Task)
		for _, source := range spec.Sources {
			found := false
			for _, item := range eligible.Items {
				if item.Path == source.Path && item.Retryable {
					found = true
					break
				}
			}
			if !found {
				return Task{}, ErrNotRetryable
			}
		}
	}
	timestamp := now()
	r := &record{Spec: spec, Task: Task{ID: hex.EncodeToString(id), Operation: spec.Operation, Destination: spec.Destination, State: "queued", CreatedAt: timestamp, UpdatedAt: timestamp, RetriedFrom: prior, Items: make([]Item, len(spec.Sources))}}
	r.Spec.Sources = append([]Binding(nil), spec.Sources...)
	for i, s := range spec.Sources {
		r.Task.Items[i] = Item{Path: s.Path, State: "queued"}
	}
	if parent != nil {
		for i := range parent.Task.Items {
			for _, source := range spec.Sources {
				if parent.Task.Items[i].Path == source.Path {
					parent.Task.Items[i].RetriedAs = r.Task.ID
				}
			}
		}
	}
	previous := m.records
	m.records = append(append([]*record(nil), m.records...), r)
	for len(m.records) > MaxRecords {
		for i, old := range m.records {
			if terminal(old.Task.State) {
				m.records = append(m.records[:i], m.records[i+1:]...)
				break
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		m.records = previous
		return Task{}, err
	}
	m.signal()
	return public(r.Task), nil
}

func public(task Task) Task {
	task.Items = append([]Item(nil), task.Items...)
	task.Total = len(task.Items)
	task.Completed = 0
	task.Succeeded = 0
	task.Failed = 0
	task.RetryableCount = 0
	for i := range task.Items {
		item := &task.Items[i]
		if item.State == "succeeded" {
			task.Succeeded++
			task.Completed++
		} else if item.State == "failed" {
			task.Failed++
			task.Completed++
		} else if item.State == "cancelled" || item.State == "interrupted" {
			task.Completed++
			if item.State == "interrupted" && item.Started {
				task.Failed++
			}
		}
		item.Retryable = item.RetriedAs == "" && terminal(task.State) && (item.State == "failed" || (!item.Started && (item.State == "cancelled" || item.State == "interrupted" || item.State == "queued")))
		if item.Retryable {
			task.RetryableCount++
		}
	}
	return task
}
func (m *Manager) List() ([]Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.records))
	for _, r := range m.records {
		out = append(out, public(r.Task))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, m.persistenceError
}
func (m *Manager) find(id string) *record {
	for _, r := range m.records {
		if r.Task.ID == id {
			return r
		}
	}
	return nil
}
func (m *Manager) Get(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	if r == nil {
		return Task{}, ErrNotFound
	}
	return public(r.Task), nil
}
func (m *Manager) Cancel(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	if r == nil {
		return Task{}, ErrNotFound
	}
	if terminal(r.Task.State) {
		return public(r.Task), nil
	}
	r.Task.CancelRequested = true
	r.Task.UpdatedAt = now()
	for i := range r.Task.Items {
		if r.Task.Items[i].State == "queued" {
			r.Task.Items[i].State = "cancelled"
		}
	}
	if r.Task.State == "queued" {
		r.Task.State = "cancelled"
	}
	// Cancellation is honored in memory even if its write fails. The worker
	// stops on persistence failure, and restart never replays this task.
	if err := m.persistLocked(); err != nil {
		return public(r.Task), err
	}
	return public(r.Task), nil
}
func (m *Manager) Retry(id string) (Task, error) {
	m.mu.Lock()
	r := m.find(id)
	if r == nil {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	view := public(r.Task)
	spec := r.Spec
	spec.Sources = nil
	for i, item := range view.Items {
		if item.Retryable {
			spec.Sources = append(spec.Sources, r.Spec.Sources[i])
		}
	}
	m.mu.Unlock()
	if len(spec.Sources) == 0 {
		return Task{}, ErrNotRetryable
	}
	return m.submit(spec, id)
}

func (m *Manager) persistLocked() error {
	data, err := json.Marshal(diskState{Version: 1, Records: m.records})
	if err != nil || len(data) > MaxStateBytes {
		m.persistenceError = true
		return ErrUnavailable
	}
	if _, err = m.write(m.file, string(data)); err != nil {
		m.persistenceError = true
		return ErrUnavailable
	}
	return nil
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) worker() {
	defer close(m.done)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		}
		for {
			if !m.runNext() {
				break
			}
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
	r.Task.UpdatedAt = now()
	if m.persistLocked() != nil {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	for i := range r.Spec.Sources {
		m.mu.Lock()
		if m.closed || m.persistenceError || r.Task.CancelRequested {
			m.finishStopped(r)
			more := !m.closed && !m.persistenceError
			m.mu.Unlock()
			return more
		}
		item := &r.Task.Items[i]
		item.State = "running"
		item.Started = true
		r.Task.UpdatedAt = now()
		if m.persistLocked() != nil {
			item.State = "interrupted"
			item.Started = false
			item.Error = "task state could not be saved before execution"
			r.Task.State = "interrupted"
			m.mu.Unlock()
			return false
		}
		spec := r.Spec
		source := r.Spec.Sources[i]
		m.mu.Unlock()
		result := m.executeSafely(spec, source)
		m.mu.Lock()
		item = &r.Task.Items[i]
		item.Status = result.Status
		item.Error = boundedError(result.Error)
		if result.Status >= 200 && result.Status < 300 && result.Error == "" {
			item.State = "succeeded"
		} else {
			item.State = "failed"
			if !result.SafeToRetry {
				item.State = "interrupted"
			}
			if item.Error == "" {
				item.Error = "operation failed"
			}
		}
		r.Task.UpdatedAt = now()
		if m.persistLocked() != nil {
			m.finishStopped(r)
			m.mu.Unlock()
			return false
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Task.CancelRequested {
		r.Task.State = "cancelled"
	} else {
		r.Task.State = "completed"
		for _, item := range r.Task.Items {
			if item.State != "succeeded" {
				r.Task.State = "failed"
				break
			}
		}
	}
	r.Task.UpdatedAt = now()
	_ = m.persistLocked()
	return !m.closed && !m.persistenceError
}
func (m *Manager) finishStopped(r *record) {
	r.Task.State = "interrupted"
	if r.Task.CancelRequested && !m.closed && !m.persistenceError {
		r.Task.State = "cancelled"
	}
	for i := range r.Task.Items {
		item := &r.Task.Items[i]
		if item.State == "queued" {
			item.State = r.Task.State
		}
	}
	r.Task.UpdatedAt = now()
	_ = m.persistLocked()
}
func boundedError(message string) string {
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func (m *Manager) executeSafely(spec Spec, source Binding) (result Result) {
	defer func() {
		if recover() != nil {
			result = Result{Status: 500, Error: "internal task failure; verify the remote result before retrying"}
		}
	}()
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
	defer cancel()
	return m.execute(ctx, spec, source)
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
			m.finishStopped(r)
		}
	}
	m.mu.Unlock()
	m.signal()
	<-m.done
}

// Spec returns a private snapshot for owner and policy checks at the HTTP
// boundary. It is never serialized into public task responses.
func (m *Manager) Spec(id string) (Spec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	if r == nil {
		return Spec{}, ErrNotFound
	}
	spec := r.Spec
	spec.Sources = append([]Binding(nil), spec.Sources...)
	return spec, nil
}

func OwnerMatches(spec Spec, owner string, version uint64) bool {
	actual := spec.OwnerID
	if actual == "" {
		actual = "installation"
	}
	if owner == "" {
		owner = "installation"
	}
	return actual == owner && (owner == "installation" || spec.PolicyVersion == version)
}

// ListOwned never exposes the names/counts of another account or an older
// root policy. The single worker and history budget remain shared globally.
func (m *Manager) ListOwned(owner string, version uint64) ([]Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0)
	for _, r := range m.records {
		if OwnerMatches(r.Spec, owner, version) {
			out = append(out, public(r.Task))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, m.persistenceError
}
