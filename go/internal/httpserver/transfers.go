package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/auth"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/budget"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/remotefetch"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/securefile"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/transfers"
)

const transferFileLimit int64 = 1 << 30
const transferDiskLimit int64 = 2 << 30

type remoteFetcher interface {
	Fetch(context.Context, string, io.Writer, int64, func(int64)) (remotefetch.Result, error)
}
type TransferConfig struct {
	File, Directory string
	Budget          *budget.Budget
	ResolveOwner    func(string, uint64) (auth.Principal, error)
	DispatcherFor   func(auth.Principal) (*RESTDispatcher, error)
	Identity        func(auth.Principal, []string) (string, error)
	Fetcher         remoteFetcher
}
type TransferController struct {
	config  TransferConfig
	manager *transfers.Manager
	diskMu  sync.Mutex
}

func NewTransferController(config TransferConfig) (*TransferController, error) {
	if config.Budget == nil || config.ResolveOwner == nil || config.DispatcherFor == nil || config.Identity == nil || !filepath.IsAbs(config.Directory) {
		return nil, errChainConfig("transfer configuration is incomplete")
	}
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		return nil, errors.New("transfer directory is unavailable")
	}
	info, err := os.Lstat(config.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("transfer directory must be private")
	}
	if err := securefile.ValidateStatePath(filepath.Join(config.Directory, ".validation")); err != nil {
		return nil, errors.New("transfer directory must have a safe private path")
	}
	if config.Fetcher == nil {
		config.Fetcher = remotefetch.NewClient(remotefetch.Config{})
	}
	c := &TransferController{config: config}
	c.manager, err = transfers.New(transfers.Config{File: config.File, Validate: c.validate, Execute: c.execute, Cleanup: c.cleanup, Prune: c.prune})
	if err != nil {
		return nil, err
	}
	return c, nil
}
func (c *TransferController) Close() {
	if c != nil && c.manager != nil {
		c.manager.Close()
	}
}
func transferPaths(spec transfers.Spec) []string {
	if spec.Kind == "fetch" {
		return []string{path.Dir(spec.Destination)}
	}
	paths := make([]string, 0, len(spec.Sources))
	for _, item := range spec.Sources {
		paths = append(paths, item.Path)
	}
	return paths
}
func (c *TransferController) owner(spec transfers.Spec) (auth.Principal, *RESTDispatcher, error) {
	owner, err := c.config.ResolveOwner(spec.OwnerID, spec.PolicyVersion)
	if err != nil || owner.ID != spec.OwnerID || owner.PolicyVersion != spec.PolicyVersion || !owner.Permissions.Read {
		return auth.Principal{}, nil, transfers.ErrDenied
	}
	if spec.Kind == "fetch" && !owner.Permissions.Upload {
		return auth.Principal{}, nil, transfers.ErrDenied
	}
	identity, err := c.config.Identity(owner, transferPaths(spec))
	if err != nil || identity != spec.Identity {
		return auth.Principal{}, nil, transfers.ErrDenied
	}
	dispatcher, err := c.config.DispatcherFor(owner)
	if err != nil {
		return auth.Principal{}, nil, transfers.ErrDenied
	}
	return owner, dispatcher, nil
}
func (c *TransferController) validate(spec transfers.Spec) error {
	_, _, err := c.owner(spec)
	return err
}
func (c *TransferController) Serve(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	actor, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		return transferError(transfers.ErrDenied)
	}
	current, err := c.config.ResolveOwner(actor.ID, actor.PolicyVersion)
	if err != nil || current != actor {
		return transferError(transfers.ErrDenied)
	}
	parts := strings.Split(route.Suffix, "/")
	if len(parts) > 3 {
		return transferError(transfers.ErrNotFound)
	}
	if len(parts) == 1 && r.Method == "POST" {
		spec, err := c.readSpec(w, r, actor)
		if err != nil || spec == nil {
			return err
		}
		task, err := c.manager.Submit(*spec)
		if err != nil {
			return transferError(err)
		}
		return sendJSON(w, r, 202, map[string]any{"task": task}, DefaultControlLimits(), nil)
	}
	if err := discardBody(w, r, DefaultControlLimits()); err != nil {
		return err
	}
	if len(parts) == 1 && r.Method == "GET" {
		tasks, failed := c.manager.ListOwned(actor.ID, actor.PolicyVersion)
		return sendJSON(w, r, 200, map[string]any{"tasks": tasks, "persistence_error": failed}, DefaultControlLimits(), nil)
	}
	if len(parts) == 2 && r.Method == "GET" {
		task, err := c.manager.GetOwned(parts[1], actor.ID, actor.PolicyVersion)
		if err != nil {
			return transferError(err)
		}
		return sendJSON(w, r, 200, map[string]any{"task": task}, DefaultControlLimits(), nil)
	}
	if len(parts) == 3 {
		if parts[2] == "download" && r.Method == "GET" {
			return c.downloadArtifact(w, r, parts[1], actor)
		}
		if r.Method == "POST" {
			var task transfers.Task
			var err error
			switch parts[2] {
			case "cancel":
				task, err = c.manager.CancelOwned(parts[1], actor.ID, actor.PolicyVersion)
			case "retry":
				task, err = c.manager.RetryOwned(parts[1], actor.ID, actor.PolicyVersion)
			default:
				return transferError(transfers.ErrNotFound)
			}
			if err != nil {
				return transferError(err)
			}
			status := 200
			if parts[2] == "retry" {
				status = 202
			}
			return sendJSON(w, r, status, map[string]any{"task": task}, DefaultControlLimits(), nil)
		}
	}
	return transferError(transfers.ErrNotFound)
}
func (c *TransferController) readSpec(w http.ResponseWriter, r *http.Request, actor auth.Principal) (*transfers.Spec, error) {
	limits := DefaultControlLimits()
	limits.MaxControlBody = 64 << 10
	body, err := readJSONBody(w, r, limits)
	if err != nil || body == nil {
		return nil, err
	}
	kind, _ := body["kind"].(string)
	spec := &transfers.Spec{Kind: kind, OwnerID: actor.ID, PolicyVersion: actor.PolicyVersion}
	d, err := c.config.DispatcherFor(actor)
	if err != nil {
		return nil, transferError(transfers.ErrDenied)
	}
	d.invalidateTextMetadata()
	switch kind {
	case "fetch":
		if !actor.Permissions.Upload {
			return nil, transferError(transfers.ErrDenied)
		}
		if len(body) != 3 {
			return nil, errBadRequest("fetch requires kind, url and destination")
		}
		spec.URL, _ = body["url"].(string)
		spec.Destination, _ = body["destination"].(string)
		if len(spec.URL) == 0 || len(spec.URL) > 8192 {
			return nil, errBadRequest("invalid source URL")
		}
		if err := remotefetch.ValidateURL(spec.URL); err != nil {
			return nil, errBadRequest("source must be a public HTTP or HTTPS URL without credentials")
		}
		spec.Destination, err = canonicalRemotePath(spec.Destination)
		if err != nil || spec.Destination == "/" {
			return nil, errBadRequest("select a new destination file")
		}
		spec.Name = path.Base(spec.Destination)
		spec.Identity, err = c.config.Identity(actor, []string{path.Dir(spec.Destination)})
		if err != nil {
			return nil, transferError(transfers.ErrDenied)
		}
		parent, err := transferMetadata(d, path.Dir(spec.Destination))
		if err != nil {
			return nil, err
		}
		if parent.Kind != model.KindFolder {
			return nil, model.NewStorageError(model.KindNotFolder, "destination parent is not a folder")
		}
		spec.DestinationID = parent.ID
		if _, err := d.read.Metadata(spec.Destination); err == nil {
			return nil, model.NewStorageError(model.KindAlreadyExists, "destination already exists")
		} else if !storageEntryNotFound(err) {
			return nil, err
		}
	case "archive":
		if !actor.Permissions.Read {
			return nil, transferError(transfers.ErrDenied)
		}
		if len(body) != 2 {
			return nil, errBadRequest("archive requires kind and paths")
		}
		paths, err := selectionPaths(body["paths"])
		if err != nil {
			return nil, err
		}
		spec.Identity, err = c.config.Identity(actor, paths)
		if err != nil {
			return nil, transferError(transfers.ErrDenied)
		}
		spec.Name = "files.zip"
		if len(paths) == 1 {
			spec.Name = path.Base(paths[0]) + ".zip"
		}
		for _, source := range paths {
			entry, err := transferMetadata(d, source)
			if err != nil {
				return nil, err
			}
			spec.Sources = append(spec.Sources, transfers.Binding{Path: source, ID: entry.ID})
		}
	default:
		return nil, errBadRequest("kind must be fetch or archive")
	}
	identity, err := c.config.Identity(actor, transferPaths(*spec))
	if err != nil || identity != spec.Identity {
		return nil, transferError(transfers.ErrDenied)
	}
	return spec, nil
}
func transferMetadata(d *RESTDispatcher, path string) (model.RemoteEntry, error) {
	if read, ok := d.read.(interface {
		BatchMetadata(string) (model.RemoteEntry, error)
	}); ok {
		return read.BatchMetadata(path)
	}
	return d.read.Metadata(path)
}
func transferError(err error) error {
	switch {
	case errors.Is(err, transfers.ErrDenied):
		return model.NewStorageError(model.KindPermissionDenied, "access denied")
	case errors.Is(err, transfers.ErrNotFound):
		return model.NewStorageError(model.KindEntryNotFound, "transfer not found")
	case errors.Is(err, transfers.ErrBusy):
		return model.NewStorageError(model.KindServiceBusy, "transfer queue is full")
	case errors.Is(err, transfers.ErrNotCancellable):
		return model.NewStorageError(model.KindAlreadyExists, "upload has started; wait for its result")
	case errors.Is(err, transfers.ErrNotRetryable):
		return model.NewStorageError(model.KindAlreadyExists, "verify the remote result before creating another task")
	case errors.Is(err, transfers.ErrInvalid):
		return errBadRequest("invalid transfer request")
	default:
		return model.NewStorageError(model.KindIOFailure, "transfer state is unavailable")
	}
}
func (c *TransferController) execute(ctx context.Context, id string, spec transfers.Spec, report transfers.ProgressFn) transfers.Result {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, d, err := c.owner(spec)
	if err != nil {
		return transferFailure("account or storage changed", false)
	}
	if spec.Kind == "fetch" {
		return c.fetch(ctx, id, spec, d, report)
	}
	return c.archive(ctx, id, spec, d, report)
}
func transferFailure(message string, retry bool) transfers.Result {
	return transfers.Result{Status: 502, Error: message, SafeToRetry: retry}
}
func (c *TransferController) destination(d *RESTDispatcher, spec transfers.Spec) error {
	d.invalidateTextMetadata()
	parent, err := transferMetadata(d, path.Dir(spec.Destination))
	if err != nil || parent.ID != spec.DestinationID || parent.Kind != model.KindFolder {
		return transfers.ErrDenied
	}
	if _, err := d.read.Metadata(spec.Destination); err == nil {
		return model.NewStorageError(model.KindAlreadyExists, "destination exists")
	} else if !storageEntryNotFound(err) {
		return err
	}
	allowed, err := d.allowsTree(spec.Destination, nil)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("destination is locked")
	}
	return nil
}
func (c *TransferController) fetch(ctx context.Context, id string, spec transfers.Spec, d *RESTDispatcher, report transfers.ProgressFn) transfers.Result {
	if err := c.destination(d, spec); err != nil {
		return transferFailure("destination changed, exists or is locked", true)
	}
	limit := transferFileLimit
	if d.maxUploadBytes > 0 && d.maxUploadBytes < limit {
		limit = d.maxUploadBytes
	}
	writer, err := c.newWriter(ctx, id, limit, report, "downloading")
	if err != nil {
		return transferFailure("temporary storage is unavailable or full", true)
	}
	defer func() { writer.close(); c.removeSuffix(id, ".part") }()
	release, err := c.config.Budget.AcquireDownload(ctx)
	if err != nil {
		return transferFailure("download resources are busy", true)
	}
	defer release()
	result, err := c.config.Fetcher.Fetch(ctx, spec.URL, writer, limit, nil)
	release()
	if err != nil {
		return transferFailure("source download failed, was rejected, or exceeded its limit", true)
	}
	if err := writer.file.Sync(); err != nil {
		return transferFailure("temporary download could not be saved", true)
	}
	writer.releaseReservation()
	if err := c.validate(spec); err != nil {
		return transferFailure("account or storage changed", false)
	}
	if err := c.destination(d, spec); err != nil {
		return transferFailure("destination changed, exists or is locked", true)
	}
	if err := report(transfers.Progress{Stage: "uploading", BytesTotal: result.Bytes, RemoteWrite: true}); err != nil {
		return transferFailure("transfer stopped before upload", true)
	}
	if _, err := writer.file.Seek(0, io.SeekStart); err != nil {
		return transferFailure("temporary file could not be read", true)
	}
	reader := &transferUploadReader{reader: writer.file, ctx: ctx, total: result.Bytes, report: report, validate: func() error { return c.validate(spec) }}
	defer d.invalidateSearch()
	defer d.invalidateTextMetadata()
	d.invalidateSearch()
	_, err = d.uploads.UploadPath(ctx, spec.Destination, reader, storage.UploadOptions{Size: &result.Bytes, ContentType: result.ContentType, Overwrite: false, ExpectedParentID: spec.DestinationID})
	if err != nil {
		return transferFailure("upload result is not confirmed; check the destination before creating another task", false)
	}
	return transfers.Result{Status: 200}
}

type transferUploadReader struct {
	reader      io.Reader
	ctx         context.Context
	done, total int64
	report      transfers.ProgressFn
	validate    func() error
}

func (r *transferUploadReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.validate(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.done += int64(n)
	if progressErr := r.report(transfers.Progress{Stage: "uploading", BytesDone: r.done, BytesTotal: r.total}); progressErr != nil {
		return 0, progressErr
	}
	return n, err
}
func (c *TransferController) archive(ctx context.Context, id string, spec transfers.Spec, d *RESTDispatcher, report transfers.ProgressFn) transfers.Result {
	d.invalidateTextMetadata()
	paths := transferPaths(spec)
	if err := c.checkSources(d, spec); err != nil {
		return transferFailure("selected files changed", false)
	}
	plan, err := d.planArchive(ctx, paths)
	if err != nil {
		return transferFailure("archive could not be planned within its limits", true)
	}
	if err := c.checkSources(d, spec); err != nil {
		return transferFailure("selected files changed", false)
	}
	writer, err := c.newWriter(ctx, id, transferFileLimit, report, "packaging")
	if err != nil {
		return transferFailure("temporary storage is unavailable or full", true)
	}
	complete := false
	defer func() {
		writer.close()
		if !complete {
			c.cleanup(id)
		}
	}()
	estimate := plan.bytes + int64(plan.nameBytes)*2 + int64(len(plan.items))*128 + 1024
	for _, item := range plan.items {
		if item.entry.Kind == model.KindFile && item.entry.Size == nil {
			estimate = -1
			break
		}
	}
	if err := writer.SetExpectedSize(estimate); err != nil {
		return transferFailure("archive exceeds temporary storage limits", true)
	}
	response := &transferArchiveWriter{Writer: writer, header: make(http.Header)}
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://archive.invalid/", nil)
	if err := d.writeArchive(response, request, plan); err != nil {
		return transferFailure("archive generation failed; no complete file was produced", true)
	}
	if err := c.validate(spec); err != nil {
		return transferFailure("account or storage changed", false)
	}
	if err := c.checkSources(d, spec); err != nil {
		return transferFailure("selected files changed", false)
	}
	if err := writer.file.Sync(); err != nil {
		return transferFailure("archive could not be saved", true)
	}
	if err := writer.file.Close(); err != nil {
		return transferFailure("archive could not be saved", true)
	}
	if err := os.Rename(c.filename(id, ".part"), c.filename(id, ".zip")); err != nil {
		return transferFailure("archive could not be saved", true)
	}
	if err := c.syncDir(); err != nil {
		return transferFailure("archive could not be saved", true)
	}
	complete = true
	return transfers.Result{Status: 200, Artifact: &transfers.Artifact{Name: spec.Name, Size: writer.written, ExpiresAt: time.Now().Add(transfers.ArtifactTTL)}}
}
func (c *TransferController) checkSources(d *RESTDispatcher, spec transfers.Spec) error {
	d.invalidateTextMetadata()
	for _, source := range spec.Sources {
		entry, err := transferMetadata(d, source.Path)
		if err != nil || entry.ID != source.ID {
			return transfers.ErrDenied
		}
	}
	return nil
}

type transferArchiveWriter struct {
	io.Writer
	header http.Header
}

func (w *transferArchiveWriter) Header() http.Header { return w.header }
func (w *transferArchiveWriter) WriteHeader(int)     {}
func (c *TransferController) filename(id, suffix string) string {
	return filepath.Join(c.config.Directory, id+suffix)
}
func (c *TransferController) removeSuffix(id, suffix string) error {
	if !validPublicShareID(id) {
		return transfers.ErrInvalid
	}
	err := os.Remove(c.filename(id, suffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (c *TransferController) cleanup(id string) error {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	first := c.removeSuffix(id, ".part")
	second := c.removeSuffix(id, ".zip")
	if first != nil {
		return first
	}
	return second
}
func (c *TransferController) prune(keep []string) error {
	set := make(map[string]bool)
	for _, id := range keep {
		set[id] = true
	}
	files, err := os.ReadDir(c.config.Directory)
	if err != nil {
		return err
	}
	for _, entry := range files {
		name := entry.Name()
		suffix := filepath.Ext(name)
		id := strings.TrimSuffix(name, suffix)
		if validPublicShareID(id) && (suffix == ".part" || suffix == ".zip" && !set[id]) {
			if err := c.removeSuffix(id, suffix); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *TransferController) syncDir() error {
	dir, err := os.Open(c.config.Directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (c *TransferController) diskBytes() (int64, error) {
	entries, err := os.ReadDir(c.config.Directory)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			if info.Size() > transferDiskLimit-total {
				return transferDiskLimit + 1, nil
			}
			total += info.Size()
		}
	}
	return total, nil
}

type transferWriter struct {
	controller                         *TransferController
	ctx                                context.Context
	file                               *os.File
	limit, written, expected, reserved int64
	reservedBytes                      int64
	stage                              string
	report                             transfers.ProgressFn
}

func (c *TransferController) newWriter(ctx context.Context, id string, limit int64, report transfers.ProgressFn, stage string) (*transferWriter, error) {
	if !validPublicShareID(id) {
		return nil, transfers.ErrInvalid
	}
	if err := securefile.ValidateStatePath(c.filename(id, ".part")); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(c.filename(id, ".part"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return &transferWriter{controller: c, ctx: ctx, file: file, limit: limit, expected: -1, report: report, stage: stage}, nil
}
func (w *transferWriter) SetExpectedSize(size int64) error {
	if size < -1 || size > w.limit || size >= 0 && size < w.written {
		return transfers.ErrInvalid
	}
	w.expected = size
	if err := w.reserve(max(size, w.written)); err != nil {
		return err
	}
	total := size
	if total < 0 {
		total = 0
	}
	return w.report(transfers.Progress{Stage: w.stage, BytesTotal: total})
}
func (w *transferWriter) reserve(size int64) error {
	c := w.controller
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	used, err := c.diskBytes()
	if err != nil {
		return err
	}
	if size-w.written > transferDiskLimit-used {
		return errors.New("transfer disk quota exceeded")
	}
	reserved, err := c.config.Budget.ReserveDisk(c.config.Directory, size, w.reserved)
	if err != nil {
		return err
	}
	w.reserved = reserved
	w.reservedBytes = size
	return nil
}
func (w *transferWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-w.written {
		return 0, errors.New("transfer file limit exceeded")
	}
	next := w.written + int64(len(p))
	if w.expected >= 0 && next > w.expected {
		return 0, errors.New("transfer exceeds the expected size")
	}
	if next > w.reservedBytes {
		// Reserve every byte before writing. Chunk rounding limits disk probes;
		// an exact fallback still permits a final chunk near the quota boundary.
		target := min((next+(1<<20)-1)/(1<<20)*(1<<20), w.limit)
		if err := w.reserve(target); err != nil {
			if target == next {
				return 0, err
			}
			if err := w.reserve(next); err != nil {
				return 0, err
			}
		}
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	total := w.expected
	if total < 0 {
		total = 0
	}
	if reportErr := w.report(transfers.Progress{Stage: w.stage, BytesDone: w.written, BytesTotal: total}); reportErr != nil {
		return n, reportErr
	}
	return n, err
}
func (w *transferWriter) releaseReservation() {
	w.controller.config.Budget.ReleaseSpool(w.reserved)
	w.reserved = 0
	w.reservedBytes = 0
}
func (w *transferWriter) close() { w.file.Close(); w.releaseReservation() }
func (c *TransferController) downloadArtifact(w http.ResponseWriter, r *http.Request, id string, actor auth.Principal) error {
	spec, err := c.manager.SpecOwned(id, actor.ID, actor.PolicyVersion)
	if err != nil {
		return transferError(err)
	}
	_, d, err := c.owner(spec)
	if err != nil {
		return transferError(err)
	}
	artifact, err := c.manager.ArtifactOwned(id, actor.ID, actor.PolicyVersion)
	if err != nil {
		return transferError(err)
	}
	release, err := c.config.Budget.AcquireDownload(r.Context())
	if err != nil {
		return err
	}
	defer release()
	file, err := securefile.OpenPrivateRegular(c.filename(id, ".zip"), transferFileLimit)
	if err != nil {
		return model.NewStorageError(model.KindEntryNotFound, "archive result is unavailable")
	}
	defer file.Close()
	stopCancel := context.AfterFunc(r.Context(), func() { file.Close() })
	defer stopCancel()
	actual, err := file.Stat()
	if err != nil || actual.Size() != artifact.Size {
		return transferError(transfers.ErrDenied)
	}
	var rootChecked time.Time
	reader := &guardedArtifact{File: file, validate: func() error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if _, err := c.manager.ArtifactOwned(id, actor.ID, actor.PolicyVersion); err != nil {
			return err
		}
		if err := c.validate(spec); err != nil {
			return err
		}
		// Cached artifacts still require the member's actual assigned root.
		// Policy checks above run on every read; bound fresh WPS root probes
		// to once a second while streaming the local result.
		if !actor.IsAdmin() && time.Since(rootChecked) >= time.Second {
			if _, err := transferMetadata(d, "/"); err != nil {
				return transfers.ErrDenied
			}
			rootChecked = time.Now()
		}
		return nil
	}}
	if err := reader.validate(); err != nil {
		return transferError(err)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"download.zip\"; filename*=UTF-8''%s", pythonQuote(artifact.Name)))
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, artifact.Name, actual.ModTime(), reader)
	return nil
}

type guardedArtifact struct {
	*os.File
	validate func() error
}

func (r *guardedArtifact) Read(p []byte) (int, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	n, err := r.File.Read(p)
	if check := r.validate(); check != nil {
		return 0, check
	}
	return n, err
}
