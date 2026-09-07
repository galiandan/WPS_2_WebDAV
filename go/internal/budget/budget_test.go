package budget

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func mustBudget(t *testing.T, config Config) *Budget {
	t.Helper()
	budget, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return budget
}

func TestDefaultConfigMirrorsPython(t *testing.T) {
	config := DefaultConfig()
	if config.MaxUploads != 2 || config.MaxDownloads != 4 || config.MaxConnections != 64 {
		t.Fatalf("slot defaults drifted: %+v", config)
	}
	if config.TransferWaitTimeout != 30.0 {
		t.Fatalf("transfer wait default = %v", config.TransferWaitTimeout)
	}
	if config.UploadSpoolMemory != 8<<20 || config.UploadMinFreeBytes != 512<<20 {
		t.Fatalf("spool defaults drifted: %+v", config)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	valid := DefaultConfig()
	cases := []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{name: "uploads", mutate: func(c *Config) { c.MaxUploads = 0 }, message: "max_uploads must be positive"},
		{name: "downloads", mutate: func(c *Config) { c.MaxDownloads = 0 }, message: "max_downloads must be positive"},
		{name: "connections", mutate: func(c *Config) { c.MaxConnections = 0 }, message: "max_connections must be positive"},
		{name: "wait", mutate: func(c *Config) { c.TransferWaitTimeout = 0 }, message: "transfer_wait_timeout must be positive"},
		{name: "spool memory", mutate: func(c *Config) { c.UploadSpoolMemory = -1 }, message: "upload_spool_memory must not be negative"},
		{name: "min free", mutate: func(c *Config) { c.UploadMinFreeBytes = -1 }, message: "upload_min_free_bytes must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.mutate(&config)
			_, err := New(config)
			if err == nil || err.Error() != tc.message {
				t.Fatalf("New error = %v, want %q", err, tc.message)
			}
		})
	}
}

// TestTransferSlotsNMinusOneNAndNPlusOne covers the completion condition:
// one fewer than capacity always proceeds, capacity proceeds, and one more
// than capacity must wait and can be cancelled or time out.
func TestTransferSlotsNMinusOneNAndNPlusOne(t *testing.T) {
	budget := mustBudget(t, Config{
		MaxUploads:          2,
		MaxDownloads:        2,
		MaxConnections:      4,
		TransferWaitTimeout: 0.05,
	})

	release1, err := budget.AcquireUpload(context.Background())
	if err != nil {
		t.Fatalf("N-1 acquire: %v", err)
	}
	release2, err := budget.AcquireUpload(context.Background())
	if err != nil {
		t.Fatalf("N acquire: %v", err)
	}

	// N+1: blocked, observable as waiting, times out into the busy error.
	blocked := make(chan error, 1)
	go func() {
		_, err := budget.AcquireUpload(context.Background())
		blocked <- err
	}()
	waitForWaiting(t, budget, 1)
	err = <-blocked
	if err == nil || err.Error() != "too many uploads are active" {
		t.Fatalf("N+1 acquire error = %v", err)
	}
	if storageErr, ok := model.AsStorageError(err); !ok || storageErr.Kind != model.KindServiceBusy {
		t.Fatalf("N+1 error is not ServiceBusy: %v", err)
	}

	release1()
	if got := budget.Stats().UploadsActive; got != 1 {
		t.Fatalf("active after one release = %d, want 1", got)
	}
	release2()
	// Release is idempotent, so an extra call cannot over-fill the pool.
	release2()
	if got := budget.Stats().UploadsActive; got != 0 {
		t.Fatalf("active after releases = %d, want 0", got)
	}
	release3, err := budget.AcquireUpload(context.Background())
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release3()

	// Cancellation while waiting returns promptly with the busy error.
	held, err := budget.AcquireDownload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	held2, err := budget.AcquireDownload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, err = budget.AcquireDownload(ctx)
	if err == nil || err.Error() != "too many downloads are active" {
		t.Fatalf("cancelled download acquire error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
	held()
	held2()
}

func waitForWaiting(t *testing.T, budget *Budget, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if budget.Stats().UploadsWaiting == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting count never reached %d", want)
}

// TestBudgetSharedAcrossSpaces pins the D-03 decision: one process budget
// means two spaces share two upload slots, so the second space's uploads
// wait instead of doubling the limit.
func TestBudgetSharedAcrossSpaces(t *testing.T) {
	budget := mustBudget(t, Config{MaxUploads: 2, MaxDownloads: 4, MaxConnections: 64, TransferWaitTimeout: 0.05})

	spaceA := make([]func(), 0, 2)
	for i := 0; i < 2; i++ {
		release, err := budget.AcquireUpload(context.Background())
		if err != nil {
			t.Fatalf("space A upload %d: %v", i, err)
		}
		spaceA = append(spaceA, release)
	}
	_, err := budget.AcquireUpload(context.Background())
	if err == nil || err.Error() != "too many uploads are active" {
		t.Fatalf("space B upload error = %v, want busy", err)
	}
	for _, release := range spaceA {
		release()
	}
	// Same for downloads at the default capacity of four.
	downloads := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		release, err := budget.AcquireDownload(context.Background())
		if err != nil {
			t.Fatalf("download %d: %v", i, err)
		}
		downloads = append(downloads, release)
	}
	if _, err := budget.AcquireDownload(context.Background()); err == nil || err.Error() != "too many downloads are active" {
		t.Fatalf("fifth download error = %v", err)
	}
	for _, release := range downloads {
		release()
	}
}

// TestConnectionGateRefusesWithoutBlocking mirrors server.py's accept-time
// behaviour: over capacity the caller is refused immediately (D-09) and
// never releases anything.
func TestConnectionGateRefusesWithoutBlocking(t *testing.T) {
	budget := mustBudget(t, DefaultConfig())
	var releases []func()
	for i := 0; i < 64; i++ {
		release, ok := budget.TryAcquireConnection()
		if !ok {
			t.Fatalf("connection %d refused", i)
		}
		releases = append(releases, release)
	}
	if _, ok := budget.TryAcquireConnection(); ok {
		t.Fatal("connection 65 accepted")
	}
	if got := budget.Stats().ConnectionsActive; got != 64 {
		t.Fatalf("active connections = %d, want 64", got)
	}
	releases[0]()
	if got := budget.Stats().ConnectionsActive; got != 63 {
		t.Fatalf("active after release = %d, want 63", got)
	}
	release, ok := budget.TryAcquireConnection()
	if !ok {
		t.Fatal("connection refused after release")
	}
	release()
	release() // idempotent
	if got := budget.Stats().ConnectionsActive; got != 63 {
		t.Fatalf("active after double release = %d, want 63", got)
	}
}

func TestStatsCarryOnlyCounts(t *testing.T) {
	budget := mustBudget(t, DefaultConfig())
	stats := budget.Stats()
	if stats.MaxUploads != 2 || stats.MaxDownloads != 4 || stats.MaxConnections != 64 {
		t.Fatalf("stats capacities drifted: %+v", stats)
	}
	if stats.UploadsActive != 0 || stats.UploadsWaiting != 0 || stats.DownloadsActive != 0 ||
		stats.DownloadsWaiting != 0 || stats.ConnectionsActive != 0 || stats.SpoolReservedBytes != 0 {
		t.Fatalf("fresh budget not idle: %+v", stats)
	}
}

func newSpoolBudget(t *testing.T, free int64, calls *[]string) *Budget {
	t.Helper()
	budget := mustBudget(t, Config{
		MaxUploads:          2,
		MaxDownloads:        4,
		MaxConnections:      64,
		TransferWaitTimeout: 30,
		UploadSpoolMemory:   100,
		UploadMinFreeBytes:  50,
	})
	budget.diskFree = func(path string) (int64, error) {
		if calls != nil {
			*calls = append(*calls, path)
		}
		return free, nil
	}
	return budget
}

func TestSpoolReservationBasics(t *testing.T) {
	var calls []string
	budget := newSpoolBudget(t, 1000, &calls)

	// At or below the in-memory threshold nothing is reserved and the disk
	// is never consulted.
	reserved, err := budget.ReserveSpool(100, 0)
	if err != nil || reserved != 0 {
		t.Fatalf("ReserveSpool(100) = %d, %v", reserved, err)
	}
	if len(calls) != 0 {
		t.Fatalf("disk consulted for in-memory upload: %v", calls)
	}

	reserved, err = budget.ReserveSpool(101, 0)
	if err != nil || reserved != 151 { // 101 + 50 head-room
		t.Fatalf("ReserveSpool(101) = %d, %v", reserved, err)
	}
	if got := budget.Stats().SpoolReservedBytes; got != 151 {
		t.Fatalf("reserved bytes = %d, want 151", got)
	}
	if len(calls) != 1 || calls[0] != os.TempDir() {
		t.Fatalf("disk calls = %v, want [%s]", calls, os.TempDir())
	}

	// Growing an existing reservation accounts for what it already holds.
	reserved, err = budget.ReserveSpool(400, 151)
	if err != nil || reserved != 450 {
		t.Fatalf("ReserveSpool(400, 151) = %d, %v", reserved, err)
	}
	if got := budget.Stats().SpoolReservedBytes; got != 450 {
		t.Fatalf("reserved after resize = %d, want 450", got)
	}

	budget.ReleaseSpool(450)
	if got := budget.Stats().SpoolReservedBytes; got != 0 {
		t.Fatalf("reserved after release = %d, want 0", got)
	}
	budget.ReleaseSpool(10) // clamped at zero like the Python max(0, ...)
	if got := budget.Stats().SpoolReservedBytes; got != 0 {
		t.Fatalf("reserved after over-release = %d, want 0", got)
	}
}

func TestSpoolConcurrentReservationsShareFreeSpace(t *testing.T) {
	budget := newSpoolBudget(t, 1000, nil)

	first, err := budget.ReserveSpool(600, 0)
	if err != nil || first != 650 { // 600 + 50 head-room
		t.Fatalf("first reserve = %d, %v", first, err)
	}
	// 1000 free minus the 650 already reserved cannot fit 550 more.
	_, err = budget.ReserveSpool(500, 0)
	if err == nil || err.Error() != "not enough free space for concurrent upload spools" {
		t.Fatalf("second reserve error = %v", err)
	}
	kind, ok := model.AsStorageError(err)
	if !ok || kind.Kind != model.KindInsufficientStorage {
		t.Fatalf("second reserve error kind = %v", err)
	}

	budget.ReleaseSpool(first)
	second, err := budget.ReserveSpool(500, 0)
	if err != nil || second != 550 {
		t.Fatalf("reserve after release = %d, %v", second, err)
	}
}

func TestSpoolFreeSpaceBoundaryIsInclusive(t *testing.T) {
	budget := newSpoolBudget(t, 200, nil)
	// free == required passes the strict < comparison, like Python.
	if _, err := budget.ReserveSpool(150, 0); err != nil { // 150 + 50 == 200
		t.Fatalf("boundary reserve failed: %v", err)
	}
	budget.ReleaseSpool(200)

	short := newSpoolBudget(t, 199, nil)
	if _, err := short.ReserveSpool(150, 0); err == nil {
		t.Fatal("reserve succeeded one byte under the limit")
	}
}

func TestSpoolDiskUnavailable(t *testing.T) {
	budget := mustBudget(t, Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 1, TransferWaitTimeout: 30, UploadSpoolMemory: 100, UploadMinFreeBytes: 0})
	budget.diskFree = func(path string) (int64, error) { return 0, errors.New("statfs failed") }
	_, err := budget.ReserveSpool(200, 0)
	if err == nil || err.Error() != "upload spool directory is unavailable" {
		t.Fatalf("ReserveSpool error = %v", err)
	}
	if got := budget.Stats().SpoolReservedBytes; got != 0 {
		t.Fatalf("failed reserve left %d reserved", got)
	}
}

// TestSpoolReservationsRace hammers the process-wide spool view from many
// goroutines: with 1000 free bytes and 650-byte requirements (600 + 50
// head-room) exactly one reservation fits.
func TestSpoolReservationsRace(t *testing.T) {
	budget := newSpoolBudget(t, 1000, nil)
	const workers = 24
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := budget.ReserveSpool(600, 0); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successes = %d, want 1", successes.Load())
	}
	if got := budget.Stats().SpoolReservedBytes; got != 650 {
		t.Fatalf("reserved = %d, want 650", got)
	}
}

// TestTransferSlotsRace acquires and releases both transfer kinds from many
// goroutines under the race detector; capacity is never exceeded.
func TestTransferSlotsRace(t *testing.T) {
	budget := mustBudget(t, DefaultConfig())
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for i := 0; i < 20; i++ {
				if release, err := budget.AcquireUpload(ctx); err == nil {
					release()
				}
				if release, err := budget.AcquireDownload(ctx); err == nil {
					release()
				}
				if release, ok := budget.TryAcquireConnection(); ok {
					release()
				}
			}
		}()
	}
	wg.Wait()
	stats := budget.Stats()
	if stats.UploadsActive != 0 || stats.DownloadsActive != 0 || stats.ConnectionsActive != 0 ||
		stats.UploadsWaiting != 0 || stats.DownloadsWaiting != 0 {
		t.Fatalf("pools not empty after race: %+v", stats)
	}
}

// TestTransferWaitUsesConfiguredTimeout keeps the timing assertion in a
// synthetic clock so the suite stays fast: a full transfer wait is consumed
// without real sleeping.
func TestTransferWaitUsesConfiguredTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := mustBudget(t, Config{MaxUploads: 1, MaxDownloads: 1, MaxConnections: 1, TransferWaitTimeout: 30})
		held, err := budget.AcquireUpload(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := budget.AcquireUpload(context.Background())
			done <- err
		}()
		synctest.Wait()
		time.Sleep(30 * time.Second)
		err = <-done
		if err == nil || !strings.Contains(err.Error(), "too many uploads are active") {
			t.Fatalf("timed wait error = %v", err)
		}
		held()
	})
}
