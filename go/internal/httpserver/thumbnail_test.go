package httpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

func thumbnailRaster(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x%200 + 20), G: uint8(y%200 + 20), B: 90, A: 255})
		}
	}
	var output bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&output, source)
	case "jpeg":
		err = jpeg.Encode(&output, source, &jpeg.Options{Quality: 80})
	case "gif":
		err = gif.Encode(&output, source, nil)
	default:
		t.Fatal("unknown test format")
	}
	if err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func thumbnailStorageFor(body []byte, name string) *downloadStorageFake {
	entry := fileEntry("image-id", name)
	entry.Size = model.Ptr(int64(len(body)))
	return &downloadStorageFake{entry: entry, payload: string(body)}
}
func thumbnailRequest(t *testing.T, d *RESTDispatcher) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	readRouter(t, d).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/thumbnail?path=/photo", nil))
	return w
}

func TestThumbnailRouteResizesSafeRasterFormats(t *testing.T) {
	for _, format := range []string{"jpeg", "png", "gif"} {
		t.Run(format, func(t *testing.T) {
			input := thumbnailRaster(t, format, 513, 257)
			fake := thumbnailStorageFor(input, "photo."+format)
			d := newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake)
			w := thumbnailRequest(t, d)
			if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			config, kind, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			if err != nil || kind != "jpeg" || config.Width != 256 || config.Height != 128 || w.Body.Len() > thumbnailOutputLimit {
				t.Fatalf("config=%+v format=%q bytes=%d err=%v", config, kind, w.Body.Len(), err)
			}
			if len(fake.opened) != 1 {
				t.Fatal(fake.opened)
			}
		})
	}
}

func TestThumbnailSmallTransparencyAndGIFCanvas(t *testing.T) {
	t.Run("transparent PNG becomes white without upscaling", func(t *testing.T) {
		var input bytes.Buffer
		if err := png.Encode(&input, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
			t.Fatal(err)
		}
		fake := thumbnailStorageFor(input.Bytes(), "transparent.png")
		body, err := renderThumbnail(context.Background(), fake, "/photo", fake.entry)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := jpeg.Decode(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := decoded.At(0, 0).RGBA()
		if decoded.Bounds() != image.Rect(0, 0, 3, 2) || r < 64000 || g < 64000 || b < 64000 {
			t.Fatalf("bounds=%v color=%d,%d,%d", decoded.Bounds(), r, g, b)
		}
	})
	t.Run("GIF first frame is placed on its logical canvas", func(t *testing.T) {
		palette := color.Palette{color.RGBA{255, 0, 0, 255}, color.Transparent}
		frame := image.NewPaletted(image.Rect(2, 1, 4, 3), palette)
		var input bytes.Buffer
		if err := gif.EncodeAll(&input, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0}, Config: image.Config{Width: 6, Height: 4, ColorModel: palette}}); err != nil {
			t.Fatal(err)
		}
		fake := thumbnailStorageFor(input.Bytes(), "offset.gif")
		body, err := renderThumbnail(context.Background(), fake, "/photo", fake.entry)
		if err != nil {
			t.Fatal(err)
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(body))
		if err != nil || config.Width != 6 || config.Height != 4 {
			t.Fatalf("config=%+v err=%v", config, err)
		}
	})
}

func TestThumbnailRejectsUnsupportedAndActiveFilesBeforeOpen(t *testing.T) {
	for _, name := range []string{"image.svg", "page.html", "image.webp", "image.avif", "image.bmp", "file.txt"} {
		t.Run(name, func(t *testing.T) {
			fake := thumbnailStorageFor([]byte("<svg onload='alert(1)'></svg>"), name)
			w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
			if w.Code != 501 || len(fake.opened) != 0 {
				t.Fatalf("status=%d opened=%v", w.Code, fake.opened)
			}
		})
	}
	for _, active := range []string{"<svg onload='alert(1)'></svg>", "<!doctype html><script>alert(1)</script>"} {
		fake := thumbnailStorageFor([]byte(active), "disguised.jpg")
		w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
		if w.Code != 400 || w.Header().Get("Content-Type") == "image/jpeg" {
			t.Fatalf("status=%d headers=%v", w.Code, w.Header())
		}
	}
}

func oversizedThumbnailPNG(t *testing.T, width, height uint32) []byte {
	data := thumbnailRaster(t, "png", 1, 1)
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestThumbnailDimensionsAreCheckedBeforeDecodeAllocation(t *testing.T) {
	for _, dimensions := range [][2]uint32{{4000, 3000}, {thumbnailSideLimit + 1, 1}, {1, thumbnailSideLimit + 1}} {
		fake := thumbnailStorageFor(oversizedThumbnailPNG(t, dimensions[0], dimensions[1]), "large.png")
		w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
		if w.Code != 507 || !strings.Contains(w.Body.String(), "megapixel") {
			t.Fatalf("dimensions=%v status=%d body=%s", dimensions, w.Code, w.Body.String())
		}
	}
}

func TestThumbnailInputBoundsTruncationAndDeclaredLength(t *testing.T) {
	t.Run("metadata oversize refuses before opening", func(t *testing.T) {
		fake := thumbnailStorageFor([]byte("unused"), "large.png")
		fake.entry.Size = model.Ptr(int64(thumbnailInputLimit + 1))
		w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
		if w.Code != 507 || len(fake.opened) != 0 {
			t.Fatalf("status=%d opened=%v", w.Code, fake.opened)
		}
	})
	t.Run("unknown-length stream is bounded and closed", func(t *testing.T) {
		stream := newFakeStream(strings.Repeat("x", thumbnailInputLimit+1024), nil)
		fake := &downloadStorageFake{entry: nullFileEntry("id", "large.png"), stream: stream}
		w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
		if w.Code != 507 || stream.closeCalls() != 1 {
			t.Fatalf("status=%d closes=%d", w.Code, stream.closeCalls())
		}
		if consumed := thumbnailInputLimit + 1024 - stream.data.(*strings.Reader).Len(); consumed != thumbnailInputLimit+1 {
			t.Fatalf("consumed=%d", consumed)
		}
	})
	for _, format := range []string{"jpeg", "png", "gif"} {
		t.Run("truncated "+format, func(t *testing.T) {
			input := thumbnailRaster(t, format, 40, 40)
			fake := thumbnailStorageFor(input[:len(input)/2], "broken."+format)
			w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
			if w.Code != 400 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("length mismatch fails", func(t *testing.T) {
		input := thumbnailRaster(t, "png", 3, 3)
		stream := newFakeStream(string(input), model.Ptr(int64(len(input)+1)))
		fake := &downloadStorageFake{entry: nullFileEntry("id", "image.png"), stream: stream}
		w := thumbnailRequest(t, newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake))
		if w.Code != 400 || stream.closeCalls() != 1 {
			t.Fatalf("status=%d close=%d", w.Code, stream.closeCalls())
		}
	})
}

func TestThumbnailCancellationClosesBlockedUpstream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancelBlockingStream{fakeDownloadStream: newFakeStream("", nil), reader: reader, entered: make(chan struct{})}
	fake := &downloadStorageFake{entry: nullFileEntry("id", "image.png"), stream: stream}
	d := newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- d.doThumbnail(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx), "/photo")
	}()
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stream read did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock source")
	}
	if stream.closeCalls() != 1 {
		t.Fatalf("closes=%d", stream.closeCalls())
	}
	if len(d.thumbnailState().active) != 0 {
		t.Fatal("renderer slot leaked")
	}
}

func TestThumbnailQueueBoundsAndCancellation(t *testing.T) {
	s := (&RESTDispatcher{}).thumbnailState()
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	for i := 0; i < cap(s.waiting); i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.acquire(ctx)
			if release != nil {
				release()
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("wait err=%v", err)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.waiting) < cap(s.waiting) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.waiting) != cap(s.waiting) {
		t.Fatal("queue did not fill")
	}
	if release, err := s.acquire(context.Background()); err == nil {
		release()
		t.Fatal("accepted beyond queue capacity")
	}
	for _, cancel := range cancels {
		cancel()
	}
	wg.Wait()
	if len(s.waiting) != 0 {
		t.Fatal("waiting slots leaked")
	}
}

func TestThumbnailOutputLimitAndResizeCancellation(t *testing.T) {
	output := &thumbnailOutput{ctx: context.Background()}
	if _, err := output.Write(make([]byte, thumbnailOutputLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{1}); err == nil || output.Len() != thumbnailOutputLimit {
		t.Fatal("output exceeded cap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resizeThumbnail(ctx, image.NewRGBA(image.Rect(0, 0, 10, 10))); !errors.Is(err, context.Canceled) {
		t.Fatalf("resize err=%v", err)
	}
}

func TestThumbnailCacheTracksRevisionAndCredentialIdentity(t *testing.T) {
	input := thumbnailRaster(t, "png", 5, 5)
	fake := thumbnailStorageFor(input, "image.png")
	d := newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake)
	identity := [32]byte{1}
	d.taskIdentity = func() ([32]byte, error) { return identity, nil }
	for i := 0; i < 2; i++ {
		if w := thumbnailRequest(t, d); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if len(fake.opened) != 1 {
		t.Fatalf("cache did not reuse render: %v", fake.opened)
	}
	fake.entry.Etag = model.Ptr("revision-2")
	thumbnailRequest(t, d)
	if len(fake.opened) != 2 {
		t.Fatal("metadata revision did not invalidate cache")
	}
	identity = [32]byte{2}
	thumbnailRequest(t, d)
	if len(fake.opened) != 3 || len(d.thumbnailState().cache) != 1 {
		t.Fatal("credential switch reused prior cache")
	}
	fake.entry.Etag = nil
	fake.entry.ModifiedAt = nil
	thumbnailRequest(t, d)
	thumbnailRequest(t, d)
	if len(fake.opened) != 5 {
		t.Fatal("metadata without revision was cached")
	}
}

func TestThumbnailCacheHitRechecksAccountBeforeResponse(t *testing.T) {
	input := thumbnailRaster(t, "png", 5, 5)
	fake := thumbnailStorageFor(input, "image.png")
	d := newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake)
	d.taskIdentity = func() ([32]byte, error) { return [32]byte{1}, nil }
	if w := thumbnailRequest(t, d); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	calls := 0
	d.taskIdentity = func() ([32]byte, error) {
		calls++
		if calls == 1 {
			return [32]byte{1}, nil
		}
		return [32]byte{2}, nil
	}
	w := thumbnailRequest(t, d)
	if w.Code != 409 || len(fake.opened) != 1 || w.Header().Get("Content-Type") == "image/jpeg" {
		t.Fatalf("stale cache returned: status=%d opened=%v", w.Code, fake.opened)
	}
}

func TestThumbnailCacheMemoryEntriesAndExpiryAreBounded(t *testing.T) {
	s := (&RESTDispatcher{}).thumbnailState()
	identity := [32]byte{1}
	s.get([32]byte{}, identity)
	for i := 0; i < thumbnailCacheEntries+1; i++ {
		s.put([32]byte{byte(i)}, make([]byte, 1024))
	}
	if len(s.cache) > thumbnailCacheEntries || s.bytes > thumbnailCacheBytes {
		t.Fatal("entry cache unbounded")
	}
	for i := 0; i < 50; i++ {
		s.put([32]byte{byte(i), 2}, make([]byte, thumbnailOutputLimit))
	}
	if len(s.cache) > thumbnailCacheEntries || s.bytes > thumbnailCacheBytes {
		t.Fatal("byte cache unbounded")
	}
	key := [32]byte{222}
	s.put(key, []byte("expired"))
	entry := s.cache[key]
	entry.expires = time.Now().Add(-time.Second)
	s.cache[key] = entry
	if s.get(key, identity) != nil {
		t.Fatal("expired thumbnail returned")
	}
	s.get([32]byte{}, [32]byte{3})
	if s.bytes != 0 || len(s.cache) != 0 {
		t.Fatal("old account cache retained")
	}
}

func TestThumbnailRouteRequiresAuthentication(t *testing.T) {
	fake := thumbnailStorageFor(thumbnailRaster(t, "png", 1, 1), "image.png")
	d := newReadDispatcherDownloads(t, &fakeReadStorage{}, nil, fake)
	chain, err := NewChain(ChainConfig{Router: readRouter(t, d), Health: func(http.ResponseWriter, *http.Request) {}, Auth: BasicAuthConfig{Username: "user", Password: "pass"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	chain.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/thumbnail?path=/photo", nil))
	if w.Code != 401 || len(fake.opened) != 0 {
		t.Fatalf("status=%d opened=%v", w.Code, fake.opened)
	}
}
