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

func TestMediaPreviewStreamsAndHonorsRange(t *testing.T) {
	for _, name := range []string{"photo.PNG", "document.pdf", "movie.MP4", "music.mp3", "track.FLAC", "movie.webm"} {
		for _, partial := range []bool{false, true} {
			store := &downloadStorageFake{entry: downloadFileEntry(), payload: "0123456789"}
			store.entry.Name = name
			store.entry.Size = model.Ptr(int64(10))
			router := newDownloadRouter(t, store, DownloadLimits{PreviewMaxBytes: 2, StreamChunkSize: 3})
			request := newTestRequest("GET", "/api/v1/preview?path=%2Fmedia")
			want, status := "0123456789", 200
			if partial {
				request.Header.Set("Range", "bytes=2-5")
				want, status = "2345", 206
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != status || recorder.Body.String() != want {
				t.Fatalf("%s partial=%v: status=%d body=%q", name, partial, recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Content-Type") != previewMediaType(name) || recorder.Header().Get("Content-Disposition") != "inline" {
				t.Fatalf("incorrect inline media headers: %v", recorder.Header())
			}
			if recorder.Header().Get("X-Content-Type-Options") != "nosniff" || recorder.Header().Get("Cache-Control") != "no-store, no-transform" {
				t.Fatal("missing media protection headers")
			}
			if partial && recorder.Header().Get("Content-Range") != "bytes 2-5/10" {
				t.Fatal("missing range")
			}
		}
	}
}

func TestMediaPreviewRejectsActiveFormatsBeforeOpening(t *testing.T) {
	for _, name := range []string{"page.xhtml", "image.svg", "photo.png.exe", "file.bin"} {
		store := downloadStorage(t, newFakeStream("active content", nil))
		store.entry.Name = name
		recorder := httptest.NewRecorder()
		newDownloadRouter(t, store, DownloadLimits{}).ServeHTTP(recorder, newTestRequest("GET", "/api/v1/preview?path=%2Fmedia"))
		if recorder.Code != 501 || len(store.opened) != 0 {
			t.Fatalf("active format opened: %s status=%d", name, recorder.Code)
		}
	}
}

func TestCodePreviewRemainsBoundedInertBytes(t *testing.T) {
	for _, name := range []string{"source.go", "source.ts", "source.CPP", "source.py", "README.markdown", "page.html"} {
		store := downloadStorage(t, newFakeStream("<script>alert(1)</script>", nil))
		store.entry.Name = name
		response := httptest.NewRecorder()
		newDownloadRouter(t, store, DownloadLimits{PreviewMaxBytes: 8}).ServeHTTP(response, newTestRequest("GET", "/api/v1/preview?path=%2Fsource"))
		if response.Code != 200 || response.Header().Get("Content-Type") != "application/octet-stream" || response.Body.String() != "<script>" || response.Header().Get("X-Preview-Truncated") != "true" {
			t.Fatalf("%s: status=%d headers=%v body=%q", name, response.Code, response.Header(), response.Body.String())
		}
		if isPreviewableText(name) {
			t.Fatalf("code unexpectedly editable: %s", name)
		}
	}
}
