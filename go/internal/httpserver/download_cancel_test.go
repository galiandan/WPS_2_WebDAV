package httpserver

import (
	"context"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

type cancelBlockingStream struct {
	*fakeDownloadStream
	reader  *io.PipeReader
	entered chan struct{}
	once    sync.Once
}

func (s *cancelBlockingStream) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	return s.reader.Read(p)
}
func (s *cancelBlockingStream) Close() error { s.reader.Close(); return s.fakeDownloadStream.Close() }
func TestDownloadCancelDuringBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	storage := &downloadStorageFake{entry: model.RemoteEntry{ID: "1", Name: "test.txt", Kind: model.KindFile}, stream: stream}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sendDownload(httptest.NewRecorder(), request, "/test", false, storage, 1024)
	}()
	<-stream.entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		reader.Close()
		<-done
		t.Fatal("canceled download stays blocked in Read until upstream is closed externally")
	}
}
