// Package budget enforces the process-wide resource limits shared by every
// mounted space: concurrent uploads, concurrent downloads, inbound
// connections, and the upload spool reserve.
//
// The application wiring creates exactly one Budget and injects it into
// every space. Per-space slot accounting would let the effective limits
// scale with the number of mounted spaces (the D-03 defect); here two
// spaces with the default limits share two upload slots and four download
// slots, not four and eight.
package budget

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// Defaults mirror storage.py (max_uploads, max_downloads,
// transfer_wait_timeout), server.py (max_connections), and client.py's
// upload spool configuration.
const (
	DefaultMaxUploads          = 2
	DefaultMaxDownloads        = 4
	DefaultMaxConnections      = 64
	DefaultTransferWaitSeconds = 30.0
	DefaultSpoolMemoryBytes    = int64(8 << 20)
	DefaultSpoolMinFreeBytes   = int64(512 << 20)
)

// Config carries the process-wide limits. Zero values are rejected by New;
// DefaultConfig provides the Python defaults.
type Config struct {
	MaxUploads          int
	MaxDownloads        int
	MaxConnections      int
	TransferWaitTimeout float64 // seconds a transfer may wait for a slot

	UploadSpoolMemory  int64  // spool reservations below this never touch disk accounting
	UploadSpoolDir     string // empty means the OS temp directory
	UploadMinFreeBytes int64  // head-room kept free under every spool reservation
}

// DefaultConfig mirrors the Python defaults for all limits.
func DefaultConfig() Config {
	return Config{
		MaxUploads:          DefaultMaxUploads,
		MaxDownloads:        DefaultMaxDownloads,
		MaxConnections:      DefaultMaxConnections,
		TransferWaitTimeout: DefaultTransferWaitSeconds,
		UploadSpoolMemory:   DefaultSpoolMemoryBytes,
		UploadMinFreeBytes:  DefaultSpoolMinFreeBytes,
	}
}

// New validates the configuration and returns the process budget.
func New(config Config) (*Budget, error) {
	if config.MaxUploads <= 0 {
		return nil, errors.New("max_uploads must be positive")
	}
	if config.MaxDownloads <= 0 {
		return nil, errors.New("max_downloads must be positive")
	}
	if config.MaxConnections <= 0 {
		return nil, errors.New("max_connections must be positive")
	}
	if config.TransferWaitTimeout <= 0 {
		return nil, errors.New("transfer_wait_timeout must be positive")
	}
	if config.UploadSpoolMemory < 0 {
		return nil, errors.New("upload_spool_memory must not be negative")
	}
	if config.UploadMinFreeBytes < 0 {
		return nil, errors.New("upload_min_free_bytes must not be negative")
	}
	return &Budget{
		uploads:            newSlotPool(config.MaxUploads),
		downloads:          newSlotPool(config.MaxDownloads),
		connections:        newSlotPool(config.MaxConnections),
		transferWait:       secondsDuration(config.TransferWaitTimeout),
		uploadSpoolMemory:  config.UploadSpoolMemory,
		uploadSpoolDir:     config.UploadSpoolDir,
		uploadMinFreeBytes: config.UploadMinFreeBytes,
		diskFree:           diskFreeBytes,
	}, nil
}

// Budget is the single process-wide resource gate.
type Budget struct {
	uploads     *slotPool
	downloads   *slotPool
	connections *slotPool

	transferWait time.Duration

	uploadSpoolMemory  int64
	uploadSpoolDir     string
	uploadMinFreeBytes int64

	spoolMu            sync.Mutex
	reservedSpoolBytes int64

	// diskFree is a seam for tests; production builds use the
	// platform-specific diskFreeBytes.
	diskFree func(path string) (int64, error)
}

// slotPool is a counting semaphore whose tokens live in a buffered channel:
// receiving acquires, sending releases, and pairing is guaranteed because
// every acquire returns at most one release function.
type slotPool struct {
	tokens  chan struct{}
	waiting atomic.Int64
}

func newSlotPool(capacity int) *slotPool {
	p := &slotPool{tokens: make(chan struct{}, capacity)}
	for i := 0; i < capacity; i++ {
		p.tokens <- struct{}{}
	}
	return p
}

// acquire blocks until a slot frees, the context ends, or the wait budget
// elapses. The returned release function is safe to defer on every return
// path and is a no-op after its first call.
func (p *slotPool) acquire(ctx context.Context, wait time.Duration) (func(), error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	p.waiting.Add(1)
	defer p.waiting.Add(-1)
	select {
	case <-p.tokens:
		return sync.OnceFunc(func() { p.tokens <- struct{}{} }), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errWaitTimedOut
	}
}

var errWaitTimedOut = errors.New("budget wait timed out")

// AcquireUpload reserves one upload slot. Waiting is bounded by the budget's
// transfer wait and the caller's context; both failures surface as the
// Python busy error so callers cannot distinguish them, matching the
// reference behaviour where a timed-out wait is the only failure mode.
func (b *Budget) AcquireUpload(ctx context.Context) (func(), error) {
	release, err := b.uploads.acquire(ctx, b.transferWait)
	if err != nil {
		return nil, model.NewStorageError(model.KindServiceBusy, "too many uploads are active")
	}
	return release, nil
}

// AcquireDownload reserves one download slot with the same semantics as
// AcquireUpload.
func (b *Budget) AcquireDownload(ctx context.Context) (func(), error) {
	release, err := b.downloads.acquire(ctx, b.transferWait)
	if err != nil {
		return nil, model.NewStorageError(model.KindServiceBusy, "too many downloads are active")
	}
	return release, nil
}

// TryAcquireConnection reserves one inbound connection slot without
// blocking, mirroring server.py's accept-time gate: a caller that is refused
// closes the socket immediately and never releases anything (D-09).
func (b *Budget) TryAcquireConnection() (func(), bool) {
	select {
	case <-b.connections.tokens:
		return sync.OnceFunc(func() { b.connections.tokens <- struct{}{} }), true
	default:
		return nil, false
	}
}

// ReserveSpool reserves spool bytes for one upload and returns the upload's
// total reservation, mirroring client.py's _reserve_spool_bytes: uploads at
// or below the in-memory threshold never reserve, and larger uploads must
// fit into the spool directory's free space minus every other active
// reservation. Passing a prior reservation as current resizes it in one
// atomic step.
func (b *Budget) ReserveSpool(total int64, current int64) (int64, error) {
	if total <= b.uploadSpoolMemory {
		return current, nil
	}
	required := total + b.uploadMinFreeBytes
	spoolDir := b.uploadSpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	b.spoolMu.Lock()
	defer b.spoolMu.Unlock()
	free, err := b.diskFree(spoolDir)
	if err != nil {
		return current, model.NewStorageError(model.KindInsufficientStorage, "upload spool directory is unavailable")
	}
	others := b.reservedSpoolBytes - current
	if free-others < required {
		return current, model.NewStorageError(model.KindInsufficientStorage, "not enough free space for concurrent upload spools")
	}
	b.reservedSpoolBytes += required - current
	return required, nil
}

// ReleaseSpool drops a reservation, clamping the process total at zero
// exactly like client.py's _release_spool_bytes.
func (b *Budget) ReleaseSpool(reserved int64) {
	if reserved <= 0 {
		return
	}
	b.spoolMu.Lock()
	defer b.spoolMu.Unlock()
	b.reservedSpoolBytes -= reserved
	if b.reservedSpoolBytes < 0 {
		b.reservedSpoolBytes = 0
	}
}

// Stats exposes observability counters. Only counts and byte totals travel
// here — never paths, names, or identifiers.
type Stats struct {
	MaxUploads         int
	MaxDownloads       int
	MaxConnections     int
	UploadsActive      int
	UploadsWaiting     int64
	DownloadsActive    int
	DownloadsWaiting   int64
	ConnectionsActive  int
	SpoolReservedBytes int64
}

// Stats returns a snapshot of the current counters.
func (b *Budget) Stats() Stats {
	b.spoolMu.Lock()
	reserved := b.reservedSpoolBytes
	b.spoolMu.Unlock()
	return Stats{
		MaxUploads:         cap(b.uploads.tokens),
		MaxDownloads:       cap(b.downloads.tokens),
		MaxConnections:     cap(b.connections.tokens),
		UploadsActive:      cap(b.uploads.tokens) - len(b.uploads.tokens),
		UploadsWaiting:     b.uploads.waiting.Load(),
		DownloadsActive:    cap(b.downloads.tokens) - len(b.downloads.tokens),
		DownloadsWaiting:   b.downloads.waiting.Load(),
		ConnectionsActive:  cap(b.connections.tokens) - len(b.connections.tokens),
		SpoolReservedBytes: reserved,
	}
}

func secondsDuration(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}
