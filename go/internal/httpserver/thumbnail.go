package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

const (
	thumbnailInputLimit   = 8 << 20
	thumbnailPixelLimit   = 8000000
	thumbnailSideLimit    = 8192
	thumbnailSize         = 256
	thumbnailOutputLimit  = 128 << 10
	thumbnailCacheBytes   = 4 << 20
	thumbnailCacheEntries = 64
	thumbnailCacheTTL     = 5 * time.Minute
	thumbnailWait         = 15 * time.Second
)

type thumbnailCacheEntry struct {
	body          []byte
	expires, used time.Time
}
type thumbnailState struct {
	active      chan struct{}
	waiting     chan struct{}
	mu          sync.Mutex
	cache       map[[32]byte]thumbnailCacheEntry
	bytes       int
	identity    [32]byte
	identitySet bool
}

func (d *RESTDispatcher) thumbnailState() *thumbnailState {
	d.thumbnailOnce.Do(func() {
		d.thumbnails = &thumbnailState{active: make(chan struct{}, 1), waiting: make(chan struct{}, 8), cache: make(map[[32]byte]thumbnailCacheEntry)}
	})
	return d.thumbnails
}

func (s *thumbnailState) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.waiting <- struct{}{}:
	default:
		return nil, model.NewStorageError(model.KindServiceBusy, "thumbnail queue is full")
	}
	defer func() { <-s.waiting }()
	timer := time.NewTimer(thumbnailWait)
	defer timer.Stop()
	select {
	case s.active <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.active
			return nil, err
		}
		return func() { <-s.active }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, model.NewStorageError(model.KindServiceBusy, "thumbnail renderer is busy")
	}
}

func (d *RESTDispatcher) doThumbnail(w http.ResponseWriter, r *http.Request, path string) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	canonical, err := canonicalRemotePath(path)
	if err != nil {
		return err
	}
	state := d.thumbnailState()
	release, err := state.acquire(r.Context())
	if err != nil {
		return err
	}
	defer release()
	var identity [32]byte
	if d.taskIdentity != nil {
		identity, err = d.taskIdentity()
		if err != nil {
			return model.NewStorageError(model.KindIOFailure, "thumbnail identity is unavailable")
		}
	}
	entry, err := d.downloads.Metadata(canonical)
	if err != nil {
		return err
	}
	if entry.Kind != model.KindFile {
		return model.NewStorageError(model.KindNotFolder, "thumbnail source is not a file")
	}
	if !thumbnailExtension(entry.Name) {
		return model.NewStorageError(model.KindUnsupportedOperation, "thumbnails support JPEG, PNG and GIF files")
	}
	if entry.Size != nil && (*entry.Size < 0 || *entry.Size > thumbnailInputLimit) {
		return thumbnailTooLarge()
	}
	cacheable := d.taskIdentity != nil && entry.ID != "" && (nonemptyThumbnailValue(entry.Etag) || nonemptyThumbnailValue(entry.ModifiedAt))
	var key [32]byte
	var body []byte
	if cacheable {
		encoded, _ := json.Marshal(struct {
			Identity [32]byte
			Path     string
			Entry    model.PublicEntry
		}{identity, canonical, entry.Public()})
		key = sha256.Sum256(encoded)
		body = state.get(key, identity)
	}
	generated := body == nil
	if generated {
		body, err = renderThumbnail(r.Context(), d.downloads, canonical, entry)
		if err != nil {
			return err
		}
	}
	// Identity can change during metadata lookup even on a cache hit.
	if d.taskIdentity != nil {
		current, err := d.taskIdentity()
		if err != nil || current != identity {
			return model.NewStorageError(model.KindAlreadyExists, "account or storage changed while generating thumbnail")
		}
	}
	if generated && cacheable {
		state.put(key, body)
	}
	if err := r.Context().Err(); err != nil {
		return err
	}
	writeResponse(w, r, http.StatusOK, body, "image/jpeg", map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'self'"}, false)
	return nil
}

func thumbnailExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return true
	}
	return false
}
func nonemptyThumbnailValue(value *string) bool { return value != nil && *value != "" }
func thumbnailTooLarge() error {
	return model.NewStorageError(model.KindInsufficientStorage, "thumbnail source exceeds the 8 MiB, 8 megapixel or 8192-pixel side limit")
}
func thumbnailInvalid() error {
	return model.NewStorageError(model.KindBadRequest, "thumbnail source is not a complete supported raster image")
}

func renderThumbnail(ctx context.Context, downloads DownloadStorage, path string, entry model.RemoteEntry) ([]byte, error) {
	stream, err := downloads.OpenPath(ctx, path, 0, nil)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	if stream.HTTPStatus() != http.StatusOK || stream.ContentRange() != nil {
		return nil, thumbnailInvalid()
	}
	length := stream.ContentLength()
	if length != nil && (*length < 0 || *length > thumbnailInputLimit) {
		return nil, thumbnailTooLarge()
	}
	body, err := io.ReadAll(io.LimitReader(&thumbnailContextReader{ctx: ctx, reader: stream}, thumbnailInputLimit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, model.NewStorageError(model.KindIOFailure, "thumbnail input read failed")
	}
	if len(body) > thumbnailInputLimit {
		return nil, thumbnailTooLarge()
	}
	if (length != nil && int64(len(body)) != *length) || (entry.Size != nil && int64(len(body)) != *entry.Size) {
		return nil, thumbnailInvalid()
	}
	config, format, err := image.DecodeConfig(&thumbnailContextReader{ctx: ctx, reader: bytes.NewReader(body)})
	if err != nil {
		return nil, thumbnailInvalid()
	}
	if format != "jpeg" && format != "png" && format != "gif" {
		return nil, thumbnailInvalid()
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > thumbnailSideLimit || config.Height > thumbnailSideLimit || int64(config.Width)*int64(config.Height) > thumbnailPixelLimit {
		return nil, thumbnailTooLarge()
	}
	decoded, decodedFormat, err := image.Decode(&thumbnailContextReader{ctx: ctx, reader: bytes.NewReader(body)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || decodedFormat != format {
		return nil, thumbnailInvalid()
	}
	bounds := decoded.Bounds()
	canvasBounds := image.Rect(0, 0, config.Width, config.Height)
	if format == "gif" && bounds != canvasBounds {
		if !bounds.In(canvasBounds) || bounds.Empty() {
			return nil, thumbnailInvalid()
		}
		canvas := image.NewRGBA(canvasBounds)
		draw.Draw(canvas, bounds, decoded, bounds.Min, draw.Src)
		decoded = canvas
	} else if bounds != canvasBounds {
		return nil, thumbnailInvalid()
	}
	resized, err := resizeThumbnail(ctx, decoded)
	if err != nil {
		return nil, err
	}
	output := &thumbnailOutput{ctx: ctx}
	if err := jpeg.Encode(output, resized, &jpeg.Options{Quality: 75}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, model.NewStorageError(model.KindInsufficientStorage, "thumbnail output exceeds the size limit")
	}
	return output.Bytes(), nil
}

type thumbnailContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *thumbnailContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type thumbnailOutput struct {
	bytes.Buffer
	ctx context.Context
}

func (w *thumbnailOutput) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > thumbnailOutputLimit-w.Len() {
		return 0, errors.New("thumbnail output limit")
	}
	return w.Buffer.Write(p)
}

// Box averaging preserves small details when shrinking a large image. Each
// source pixel contributes to one output cell; scratch space is one pixel.
// RGBA() returns alpha-premultiplied channels, so compositing on white is safe.
func resizeThumbnail(ctx context.Context, source image.Image) (*image.RGBA, error) {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := width, height
	if width > thumbnailSize || height > thumbnailSize {
		if width >= height {
			outWidth = thumbnailSize
			outHeight = height * thumbnailSize / width
		} else {
			outHeight = thumbnailSize
			outWidth = width * thumbnailSize / height
		}
		if outWidth < 1 {
			outWidth = 1
		}
		if outHeight < 1 {
			outHeight = 1
		}
	}
	output := image.NewRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < outHeight; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		y0, y1 := y*height/outHeight, (y+1)*height/outHeight
		for x := 0; x < outWidth; x++ {
			x0, x1 := x*width/outWidth, (x+1)*width/outWidth
			var red, green, blue, count uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := source.At(bounds.Min.X+sx, bounds.Min.Y+sy).RGBA()
					red += uint64(r + 65535 - a)
					green += uint64(g + 65535 - a)
					blue += uint64(b + 65535 - a)
					count++
				}
			}
			output.SetRGBA(x, y, color.RGBA{R: uint8(red / count >> 8), G: uint8(green / count >> 8), B: uint8(blue / count >> 8), A: 255})
		}
	}
	return output, nil
}

func (s *thumbnailState) get(key, identity [32]byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.identitySet || s.identity != identity {
		s.cache = make(map[[32]byte]thumbnailCacheEntry)
		s.bytes = 0
		s.identity = identity
		s.identitySet = true
	}
	now := time.Now()
	for k, entry := range s.cache {
		if !entry.expires.After(now) {
			delete(s.cache, k)
			s.bytes -= len(entry.body)
		}
	}
	entry, ok := s.cache[key]
	if !ok {
		return nil
	}
	entry.used = now
	s.cache[key] = entry
	return entry.body
}
func (s *thumbnailState) put(key [32]byte, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.cache[key]; ok {
		delete(s.cache, key)
		s.bytes -= len(old.body)
	}
	for len(s.cache) >= thumbnailCacheEntries || s.bytes+len(body) > thumbnailCacheBytes {
		var oldest [32]byte
		var when time.Time
		for k, entry := range s.cache {
			if when.IsZero() || entry.used.Before(when) {
				oldest, when = k, entry.used
			}
		}
		if when.IsZero() {
			return
		}
		s.bytes -= len(s.cache[oldest].body)
		delete(s.cache, oldest)
	}
	now := time.Now()
	s.cache[key] = thumbnailCacheEntry{body: body, expires: now.Add(thumbnailCacheTTL), used: now}
	s.bytes += len(body)
}
