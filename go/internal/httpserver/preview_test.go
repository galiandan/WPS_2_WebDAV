package httpserver

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func TestPreviewTextExtensions(t *testing.T) {
	for _, name := range []string{"a.txt", "a.LOG", "a.md", "a.csv", "a.json", "a.xml", "a.yaml", "a.yml", "a.ini", "a.conf", "a.toml"} {
		if !isPreviewableText(name) {
			t.Errorf("text rejected: %s", name)
		}
	}
	for _, name := range []string{"a.bin", "a.html", "a.txt.exe", "a", "a.pdf"} {
		if isPreviewableText(name) {
			t.Errorf("non-text allowed: %s", name)
		}
	}
}

func TestPreviewPreservesEncodedBytesAndLimit(t *testing.T) {
	for _, body := range [][]byte{{}, {0xd6, 0xd0, 0xce, 0xc4}, {0xff, 0xfe, 0x2d, 0x4e}, []byte("中文")} {
		for _, limit := range []int64{3, 20} {
			stream := newFakeStream(string(body), model.Ptr(int64(len(body))))
			router := newDownloadRouter(t, downloadStorage(t, stream), DownloadLimits{PreviewMaxBytes: limit})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/preview?path=%2Fbench-one.txt"))
			want := body
			truncated := "false"
			if int64(len(want)) > limit {
				want = want[:limit]
				truncated = "true"
			}
			if recorder.Code != 200 || !bytes.Equal(recorder.Body.Bytes(), want) {
				t.Fatalf("raw bytes changed: status=%d got=%x want=%x", recorder.Code, recorder.Body.Bytes(), want)
			}
			if recorder.Header().Get("X-Preview-Truncated") != truncated {
				t.Fatal("incorrect truncation")
			}
			if recorder.Header().Get("X-Preview-Limit") == "" {
				t.Fatal("missing limit")
			}
			if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing privacy/security headers")
			}
		}
	}
}

func TestPreviewCancelDuringBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	storage := downloadStorage(t, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sendPreview(httptest.NewRecorder(), request, "/test", storage, DownloadLimits{})
	}()
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("preview never started reading")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		reader.Close()
		<-done
		t.Fatal("canceled preview did not interrupt upstream read")
	}
}
