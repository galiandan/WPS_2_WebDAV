// The download adapter pins the wiring between the signed-object client
// and the storage Downloader surface.

package storage

import (
	"context"
	"errors"
	"testing"
)

// NewDownloader(nil) compiles only while *wps.Client's OpenDownload return
// value keeps satisfying the storage DownloadStream interface.
var _ Downloader = NewDownloader(nil)

type canceledContextDownloader struct {
	Downloader
	entered chan struct{}
}

func (d *canceledContextDownloader) OpenDownloadContext(ctx context.Context, _ string, _ int64, _ *int64, _ *string) (DownloadStream, error) {
	close(d.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDownloadCancellationReleasesSlotDuringOpen(t *testing.T) {
	s := newTestStorage(t, newFakeClient(), nil)
	d := &canceledContextDownloader{entered: make(chan struct{})}
	s.downloader = d
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.OpenPath(ctx, "/top.txt", 0, nil); done <- err }()
	<-d.entered
	if s.budget.Stats().DownloadsActive != 1 {
		t.Fatal("open did not hold slot")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("open error=%v", err)
	}
	if s.budget.Stats().DownloadsActive != 0 {
		t.Fatal("canceled open leaked download slot")
	}
}
