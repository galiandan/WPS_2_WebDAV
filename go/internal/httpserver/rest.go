package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/update"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// RootNameSetter is the storage surface the root-name glue needs. Python
// tolerates storages without set_root_name (a getattr check), so a nil
// storage is allowed and simply skips propagation.
type RootNameSetter interface {
	SetRootName(name string) error
}

// StorageLocation is the public description of a browser-visible WPS space or
// the one current WebDAV mapping. WPS IDs stay server-side; the UI only needs
// the virtual path and the selected folder path.
type StorageLocation struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	RootPath string `json:"root_path"`
}

// StorageLocationController backs the storage-location settings UI. Browse
// always starts at the original WPS space root, while Select persists the
// chosen folder as the new WebDAV root.
type StorageLocationController interface {
	Locations() (mode string, locations []StorageLocation, err error)
	Browse(path string) ([]model.RemoteEntry, error)
	Select(path string) error
}

// UpdateController is deliberately narrow: the HTTP layer can inspect and
// start an update, but it never receives a command, path, or Docker handle.
type UpdateController interface {
	Check(context.Context) (update.Status, error)
	Status() update.Status
	Start() error
}

// CurrentStorageLocationController optionally supplies the one location
// currently mapped to WebDAV. It is separate so small test controllers and
// older integrations can keep the original Locations contract.
type CurrentStorageLocationController interface {
	CurrentLocation() (StorageLocation, error)
}

// RootNameController mirrors AdapterApplication's web root name glue: the
// settings store is the hot source of truth, and every effective name
// change propagates to the storage virtual root without a restart and
// without any WPS call.
type RootNameController struct {
	settings *workspace.WebSettings
	storage  RootNameSetter

	mu      sync.Mutex
	current string
}

// NewRootNameController seeds the current name from the settings store and
// pushes it into the storage once, mirroring AdapterApplication.__post_init__.
func NewRootNameController(settings *workspace.WebSettings, storage RootNameSetter) (*RootNameController, error) {
	controller := &RootNameController{settings: settings, storage: storage, current: workspace.DefaultRootName}
	if settings != nil {
		name, err := settings.Name()
		if err != nil {
			return nil, err
		}
		controller.current = name
	}
	if storage != nil {
		if err := storage.SetRootName(controller.current); err != nil {
			return nil, err
		}
	}
	return controller, nil
}

// Current hot-reads the settings file and propagates a changed name into
// the storage, mirroring current_web_root_name. The current name is only
// touched under the mutex; the settings store keeps its own lock.
func (c *RootNameController) Current() (string, error) {
	if c.settings == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.current, nil
	}
	hot, err := c.settings.Name()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hot != c.current {
		if c.storage != nil {
			if err := c.storage.SetRootName(hot); err != nil {
				return "", err
			}
		}
		c.current = hot
	}
	return c.current, nil
}

// Set validates, persists, and propagates a new name, mirroring
// set_web_root_name. The value arrives untyped straight from the JSON body.
func (c *RootNameController) Set(value any) (string, error) {
	name, err := workspace.ValidateRootName(value)
	if err != nil {
		return "", err
	}
	if c.settings != nil {
		if _, err := c.settings.SetName(name); err != nil {
			return "", err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storage != nil {
		if err := c.storage.SetRootName(name); err != nil {
			return "", err
		}
	}
	c.current = name
	return name, nil
}

// RESTMutations is the write storage surface the REST write routes share.
// The single-space Storage and the MultiSpace view both satisfy it; test
// fakes scope exactly what each route observes.
type RESTMutations interface {
	CreateFolderPath(path string) (model.RemoteEntry, error)
	DeletePath(path string) error
	RenamePath(path string, name string) (model.RemoteEntry, error)
	MovePath(path string, destination string) (model.RemoteEntry, error)
	MoveToParentPath(path string, parentPath string) (model.RemoteEntry, error)
}

// RESTDispatcher routes the /api/v1 suffixes. B603 delivers the settings
// pair, B604 the session import, B700 the read-only routes (status,
// entries/list, metadata), B801 the download and bounded text preview routes, B1001 the upload pair,
// and stage 12 the folders/delete/rename-move routes together with the
// process-local lock store every mutation consults.
type RESTDispatcher struct {
	limits    ControlLimits
	rootName  *RootNameController
	session   *SessionImporter
	webAuth   *WebAuthController
	read      RESTReadStorage
	status    *StatusController
	download  DownloadLimits
	downloads DownloadStorage
	uploads   UploadStorage
	mutations RESTMutations
	locks     *DavLockStore
	// maxUploadBytes mirrors the declared-upload gate reading
	// client.config.max_upload_bytes; zero disables the check like the
	// Python getattr fallback.
	maxUploadBytes int64
	locations      StorageLocationController
	updater        UpdateController
}

// SetStorageLocations enables the authenticated storage-location settings
// routes without changing the established dispatcher constructor contract.
func (d *RESTDispatcher) SetStorageLocations(controller StorageLocationController) {
	d.locations = controller
}

// SetUpdater enables the authenticated update status and start routes.
func (d *RESTDispatcher) SetUpdater(controller UpdateController) {
	d.updater = controller
}

// NewRESTDispatcher wires the dispatcher; a zero limits value selects the
// AdapterApplication defaults and a nil status controller keeps the
// not_configured preflight answer. The download, upload, and mutation
// surfaces are the same storage, but they are declared separately so test
// fakes can scope what each route observes.
func NewRESTDispatcher(limits ControlLimits, rootName *RootNameController, session *SessionImporter, read RESTReadStorage, status *StatusController, download DownloadLimits, downloads DownloadStorage, uploads UploadStorage, mutations RESTMutations, locks *DavLockStore, maxUploadBytes int64) (*RESTDispatcher, error) {
	if session == nil {
		return nil, errChainConfig("a session importer is required")
	}
	if read == nil {
		return nil, errChainConfig("a storage is required")
	}
	if downloads == nil {
		return nil, errChainConfig("a download storage is required")
	}
	if uploads == nil {
		return nil, errChainConfig("an upload storage is required")
	}
	if mutations == nil {
		return nil, errChainConfig("a mutation storage is required")
	}
	if locks == nil {
		return nil, errChainConfig("a lock store is required")
	}
	if limits.MaxControlBody <= 0 || limits.MaxResponseBody <= 0 {
		limits = DefaultControlLimits()
	}
	if status == nil {
		status = NewStatusController(nil, nil)
	}
	return &RESTDispatcher{limits: limits, rootName: rootName, session: session, read: read, status: status, download: download, downloads: downloads, uploads: uploads, mutations: mutations, locks: locks, maxUploadBytes: maxUploadBytes}, nil
}

// ServeREST fits Handlers.REST in the router.
func (d *RESTDispatcher) ServeREST(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if strings.HasPrefix(route.Suffix, "auth/") || route.Suffix == "auth" {
		return d.serveWebAuth(w, r, route)
	}
	switch r.Method {
	case "GET":
		return d.doGet(w, r, route)
	case "PATCH":
		return d.doPatch(w, r, route)
	case "POST":
		if route.Suffix == "session/import" {
			return d.session.Import(w, r)
		}
		if route.Suffix == "update" {
			return d.doUpdateStart(w, r)
		}
		if route.Suffix == "folders" || route.Suffix == "folder" {
			return d.doRestFolders(w, r, route)
		}
		// Every other suffix discards the body and answers the unknown
		// route exactly like Python.
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	case "PUT":
		// _do_rest_put serves exactly upload and files; every other suffix
		// discards the body and answers the unknown-route 404.
		if route.Suffix == "upload" || route.Suffix == "files" {
			return d.doRestPut(w, r, route)
		}
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	case "DELETE":
		if route.Suffix == "entries" || route.Suffix == "files" || route.Suffix == "delete" {
			return d.doRestDelete(w, r, route)
		}
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	default:
		// Any other mutation method is unreachable through the router's
		// REST dispatch; keep Python's unknown-route answer as the floor.
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	}
}

// doRestFolders mirrors _do_rest_post's folder branch: the body is
// discarded first, then the lock check, then the create answers 201 with
// the {"path", "entry"} payload.
func (d *RESTDispatcher) doRestFolders(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	path, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	allowed, err := checkLocks(w, r, d.locks, true, path)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	entry, err := d.mutations.CreateFolderPath(path)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, uploadPayload{Path: path, Entry: entry.Public()}, d.limits, nil)
}

// doRestDelete mirrors _do_rest_delete: the body is discarded first, then
// the lock check, then the delete answers 204.
func (d *RESTDispatcher) doRestDelete(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	path, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	allowed, err := checkLocks(w, r, d.locks, true, path)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	if err := d.mutations.DeletePath(path); err != nil {
		return err
	}
	writeResponse(w, r, http.StatusNoContent, nil, contentTypeText, nil, false)
	return nil
}

// settingsPayload keeps the Python response key order (status, name).
type settingsPayload struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

type storageLocationsPayload struct {
	Status    string            `json:"status"`
	Mode      string            `json:"mode"`
	Locations []StorageLocation `json:"locations"`
	Current   *StorageLocation  `json:"current,omitempty"`
}

func (d *RESTDispatcher) doGet(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	// Python answers status and settings before reading the path query, so
	// both tolerate missing or malformed path parameters.
	switch route.Suffix {
	case "status":
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		status, err := d.status.Current()
		if err != nil {
			return err
		}
		return sendJSON(w, r, http.StatusOK, status, d.limits, nil)
	case "settings":
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		name, err := d.rootName.Current()
		if err != nil {
			return err
		}
		return sendJSON(w, r, http.StatusOK, settingsPayload{Status: "ok", Name: name}, d.limits, nil)
	case "update":
		return d.doUpdateStatus(w, r)
	case "storage":
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		return d.doStorageLocations(w, r)
	}
	// Python parses the path query before dispatching the known routes, so
	// a malformed path parameter errors even for unknown suffixes.
	path, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	switch route.Suffix {
	case "entries", "list":
		return d.doEntries(w, r, path)
	case "storage/entries":
		return d.doStorageEntries(w, r, path)
	case "metadata":
		return d.doMetadata(w, r, path)
	case "download":
		return sendDownload(w, r, path, true, d.downloads, d.download.chunkSize())
	case "preview":
		return sendPreview(w, r, path, d.downloads, d.download)
	}
	// The download route lands with the download stage.
	sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
	return nil
}

func (d *RESTDispatcher) doUpdateStatus(w http.ResponseWriter, r *http.Request) error {
	if d.updater == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "update is unavailable")
	}
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	status, err := d.updater.Check(r.Context())
	if err != nil {
		// Keep update outages separate from file operations. Returning the
		// cached state lets the page remain usable when the mirror is down.
		status = d.updater.Status()
		status.Message = "暂时无法检查更新"
	}
	return sendJSON(w, r, http.StatusOK, status, d.limits, nil)
}

func (d *RESTDispatcher) doUpdateStart(w http.ResponseWriter, r *http.Request) error {
	if d.updater == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "update is unavailable")
	}
	if err := discardBody(w, r, d.limits); err != nil {
		return err
	}
	if err := d.updater.Start(); err != nil {
		if errors.Is(err, update.ErrInProgress) {
			return sendJSON(w, r, http.StatusConflict, map[string]any{
				"error": "update is already in progress",
				"code":  "update_in_progress",
				"state": d.updater.Status().State,
			}, d.limits, nil)
		}
		return err
	}
	return sendJSON(w, r, http.StatusAccepted, d.updater.Status(), d.limits, nil)
}

func (d *RESTDispatcher) doStorageLocations(w http.ResponseWriter, r *http.Request) error {
	if d.locations == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "storage location settings are unavailable")
	}
	mode, locations, err := d.locations.Locations()
	if err != nil {
		return err
	}
	var current *StorageLocation
	if controller, ok := d.locations.(CurrentStorageLocationController); ok {
		location, err := controller.CurrentLocation()
		if err != nil {
			return err
		}
		current = &location
	}
	return sendJSON(w, r, http.StatusOK, storageLocationsPayload{
		Status:    "ok",
		Mode:      mode,
		Locations: locations,
		Current:   current,
	}, d.limits, nil)
}

func (d *RESTDispatcher) doStorageEntries(w http.ResponseWriter, r *http.Request, path string) error {
	if d.locations == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "storage location settings are unavailable")
	}
	entries, err := d.locations.Browse(path)
	if err != nil {
		return err
	}
	payload := listPayload{Path: path, Entries: make([]model.PublicEntry, 0, len(entries))}
	for _, item := range entries {
		payload.Entries = append(payload.Entries, item.Public())
	}
	return sendJSON(w, r, http.StatusOK, payload, d.limits, nil)
}

func (d *RESTDispatcher) doStorageSelect(w http.ResponseWriter, r *http.Request) error {
	if d.locations == nil {
		return model.NewStorageError(model.KindUnsupportedOperation, "storage location settings are unavailable")
	}
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	if len(payload) != 1 {
		return errBadRequest("JSON field 'path' is required")
	}
	path, ok := payload["path"].(string)
	if !ok {
		return errBadRequest("JSON field 'path' must be a string")
	}
	if err := d.locations.Select(path); err != nil {
		return err
	}
	return d.doStorageLocations(w, r)
}

func (d *RESTDispatcher) doPatch(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if route.Suffix == "storage" {
		return d.doStorageSelect(w, r)
	}
	if route.Suffix != "settings" {
		// The entries/files rename-move routes; every other suffix discards
		// the body and answers the unknown-route 404 like Python.
		if route.Suffix == "entries" || route.Suffix == "files" {
			return d.doRestEntriesPatch(w, r, route)
		}
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	}
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	if _, ok := payload["name"]; !ok || len(payload) != 1 {
		return errBadRequest("JSON field 'name' is required")
	}
	name, err := d.rootName.Set(payload["name"])
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, settingsPayload{Status: "ok", Name: name}, d.limits, nil)
}

// doRestEntriesPatch mirrors _do_rest_patch's entries/files branch. A
// destination can be protected by a lock independently of the source, so
// the exact child path (rename), the raw destination, or both the parent
// and the moved child path are all lock-checked — with the source entry's
// metadata resolved first for the parent_path branch — before any WPS
// mutation runs.
func (d *RESTDispatcher) doRestEntriesPatch(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	path, err := queryPath(route.Query)
	if err != nil {
		return err
	}
	payload, err := readJSONBody(w, r, d.limits)
	if err != nil || payload == nil {
		return err
	}
	var nameKeys []string
	for _, key := range []string{"name", "fname"} {
		if _, ok := payload[key]; ok {
			nameKeys = append(nameKeys, key)
		}
	}
	var moveKeys []string
	for _, key := range []string{"destination", "parent_path"} {
		if _, ok := payload[key]; ok {
			moveKeys = append(moveKeys, key)
		}
	}
	if len(nameKeys) > 0 && len(moveKeys) > 0 {
		return errBadRequest("choose either a new name or a move destination")
	}
	if len(nameKeys) > 1 || len(moveKeys) > 1 {
		return errBadRequest("request contains multiple mutation targets")
	}

	sourceParts, err := storage.SplitRemotePath(path)
	if err != nil {
		return err
	}
	lockPaths := []string{path}
	hasName := false
	var name string
	hasDestination := false
	var destination string
	var parentPath string
	switch {
	case len(nameKeys) == 1:
		value, isString := payload[nameKeys[0]].(string)
		if !isString {
			return errBadRequest("JSON field 'name' is required")
		}
		hasName = true
		name = value
		renamed, err := storage.JoinRemotePath(slices.Concat(sourceParts[:len(sourceParts)-1], []string{name}), false)
		if err != nil {
			return err
		}
		lockPaths = append(lockPaths, renamed)
	default:
		if _, ok := payload["destination"]; ok {
			value, isString := payload["destination"].(string)
			if !isString {
				return errBadRequest("JSON field 'destination' must be a path")
			}
			hasDestination = true
			destination = value
			if _, err := storage.SplitRemotePath(destination); err != nil {
				return err
			}
			lockPaths = append(lockPaths, destination)
			break
		}
		if _, ok := payload["parent_path"]; ok {
			value, isString := payload["parent_path"].(string)
			if !isString {
				return errBadRequest("JSON field 'parent_path' must be a path")
			}
			parentPath = value
			parentParts, err := storage.SplitRemotePath(parentPath)
			if err != nil {
				return err
			}
			sourceEntry, err := d.read.Metadata(path)
			if err != nil {
				return err
			}
			child, err := storage.JoinRemotePath(slices.Concat(parentParts, []string{sourceEntry.Name}), false)
			if err != nil {
				return err
			}
			lockPaths = append(lockPaths, parentPath, child)
			break
		}
		return errBadRequest("JSON field 'name', 'destination' or 'parent_path' is required")
	}
	allowed, err := checkLocks(w, r, d.locks, true, lockPaths...)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	var entry model.RemoteEntry
	var newPath string
	switch {
	case hasName:
		entry, err = d.mutations.RenamePath(path, name)
		if err != nil {
			return err
		}
		parts, err := storage.SplitRemotePath(path)
		if err != nil {
			return err
		}
		newPath, err = storage.JoinRemotePath(slices.Concat(parts[:len(parts)-1], []string{entry.Name}), entry.Kind == model.KindFolder)
		if err != nil {
			return err
		}
	case hasDestination:
		entry, err = d.mutations.MovePath(path, destination)
		if err != nil {
			return err
		}
		parts, err := storage.SplitRemotePath(destination)
		if err != nil {
			return err
		}
		newPath, err = storage.JoinRemotePath(parts, entry.Kind == model.KindFolder)
		if err != nil {
			return err
		}
	default:
		entry, err = d.mutations.MoveToParentPath(path, parentPath)
		if err != nil {
			return err
		}
		parts, err := storage.SplitRemotePath(parentPath)
		if err != nil {
			return err
		}
		newPath, err = storage.JoinRemotePath(slices.Concat(parts, []string{entry.Name}), entry.Kind == model.KindFolder)
		if err != nil {
			return err
		}
	}
	return sendJSON(w, r, http.StatusOK, uploadPayload{Path: newPath, Entry: entry.Public()}, d.limits, nil)
}

// contentLength mirrors _content_length: transfer coding and duplicate
// headers are already rejected by the request boundary (and Go's
// transport), the length is parsed with Python's int() tolerance for
// surrounding whitespace, and a missing required length answers 411 as
// text directly — returning no error because the response is complete.
func contentLength(w http.ResponseWriter, r *http.Request, required bool) (*int64, error) {
	if len(r.TransferEncoding) > 0 {
		return nil, errBadRequestClose("Transfer-Encoding is not supported; send Content-Length")
	}
	values := r.Header.Values("Content-Length")
	if len(values) > 1 {
		return nil, errBadRequestClose("multiple Content-Length headers are not supported")
	}
	if len(values) == 0 {
		if required {
			sendError(w, r, http.StatusLengthRequired, "Content-Length is required", false, nil, true)
		}
		return nil, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || value < 0 {
		return nil, errBadRequestClose("invalid Content-Length")
	}
	length := int64(value)
	return &length, nil
}

// discardBody mirrors _discard_body: consume up to the declared length in
// 64 KiB chunks, refusing oversized bodies before the first read.
func discardBody(w http.ResponseWriter, r *http.Request, limits ControlLimits) error {
	length, err := contentLength(w, r, false)
	if err != nil || length == nil {
		return err
	}
	if *length > limits.MaxControlBody {
		return errRequestBodyTooLarge()
	}
	buf := make([]byte, 64*1024)
	for remaining := *length; remaining > 0; {
		chunk := buf
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		read, err := r.Body.Read(chunk)
		remaining -= int64(read)
		if err != nil || read == 0 {
			break
		}
	}
	return nil
}

// readJSONBody mirrors _json_body: a required Content-Length (411 answered
// as text when absent — the caller sees a nil payload with no error and
// stops, like Python's None return), the control-body cap, an exact body
// read, and JSON-object validation.
func readJSONBody(w http.ResponseWriter, r *http.Request, limits ControlLimits) (map[string]any, error) {
	return readJSONBodyLimit(w, r, limits.MaxControlBody)
}

// readJSONBodyLimit is readJSONBody with an explicit body cap; session
// import passes its own 512 KiB bound.
func readJSONBodyLimit(w http.ResponseWriter, r *http.Request, maxLength int64) (map[string]any, error) {
	length, err := contentLength(w, r, true)
	if err != nil || length == nil {
		return nil, err
	}
	if *length > maxLength {
		return nil, errRequestBodyTooLarge()
	}
	body := make([]byte, *length)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return nil, errBadRequestClose("request body is shorter than Content-Length")
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, errBadRequest("request body must be valid JSON")
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return nil, errBadRequest("request body must be a JSON object")
	}
	return payload, nil
}

// queryPath mirrors _query_path: the path parameter defaults to "/" when
// absent and must be exactly one non-empty value.
func queryPath(query url.Values) (string, error) {
	values, ok := query["path"]
	if !ok {
		return "/", nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", model.NewStorageError(model.KindInvalidPath, "query parameter 'path' must contain one non-empty path")
	}
	return values[0], nil
}
