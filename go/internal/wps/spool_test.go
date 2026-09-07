package wps

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestSpooledFileStaysInMemoryBelowThreshold(t *testing.T) {
	spool := newSpooledFile(64, t.TempDir())
	if _, err := spool.Write([]byte("in-memory")); err != nil {
		t.Fatal(err)
	}
	if spool.file != nil {
		t.Fatal("a spool below the threshold must never touch the disk")
	}
	if spool.size != int64(len("in-memory")) {
		t.Fatalf("size = %d", spool.size)
	}
	spool.close()
}

func TestSpooledFileRollsOverAboveThreshold(t *testing.T) {
	dir := t.TempDir()
	spool := newSpooledFile(8, dir)
	if _, err := spool.Write([]byte("nine bytes")); err != nil { // 10 bytes > 8
		t.Fatal(err)
	}
	if spool.file == nil {
		t.Fatal("the spool never rolled over to disk")
	}
	if spill := spool.spillPath(); !strings.HasPrefix(spill, dir+string(os.PathSeparator)) {
		t.Fatalf("spill path = %q, want a file inside %q", spill, dir)
	}
	reader, err := spool.reopen()
	if err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "nine bytes" {
		t.Fatalf("rewound content = %q", back)
	}
	spool.close()
	if _, err := os.Stat(spool.spillPath()); !os.IsNotExist(err) {
		t.Fatalf("the spilled file survived close: %v", err)
	}
}

func TestSpooledFileExactThresholdStaysInMemory(t *testing.T) {
	spool := newSpooledFile(10, t.TempDir())
	if _, err := spool.Write([]byte("nine bytes")); err != nil {
		t.Fatal(err)
	}
	if spool.file != nil {
		t.Fatal("a spool ending exactly at the threshold must stay in memory")
	}
	spool.close()
}

func TestSpooledFileZeroThresholdAlwaysSpills(t *testing.T) {
	spool := newSpooledFile(0, t.TempDir())
	if _, err := spool.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if spool.file == nil {
		t.Fatal("a zero threshold must spill on the first byte")
	}
	spool.close()
}

func TestSpooledFileRolloverFailureSurfacesAsIOFailure(t *testing.T) {
	missing := t.TempDir() + string(os.PathSeparator) + "missing"
	spool := newSpooledFile(4, missing)
	if _, err := spool.Write([]byte("too long")); err == nil {
		t.Fatal("expected the rollover into a missing directory to fail")
	} else if err.Error() != "upload spool rollover failed" {
		t.Fatalf("error = %q", err.Error())
	}
	spool.close()
}

// recordingLimiter captures the reservation sequence so tests can verify
// the coordinated budget grows with the stream and is always released.
type recordingLimiter struct {
	reserves []int64
	releases []int64
}

func (l *recordingLimiter) ReserveSpool(total int64, current int64) (int64, error) {
	l.reserves = append(l.reserves, total)
	return total, nil
}

func (l *recordingLimiter) ReleaseSpool(reserved int64) {
	l.releases = append(l.releases, reserved)
}

var _ SpoolLimiter = (*recordingLimiter)(nil)
