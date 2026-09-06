package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/workspace"
)

// RootNameSetter is the storage surface the root-name glue needs. Python
// tolerates storages without set_root_name (a getattr check), so a nil
// storage is allowed and simply skips propagation.
type RootNameSetter interface {
	SetRootName(name string) error
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

// RESTDispatcher routes the /api/v1 suffixes. B603 delivers the settings
// pair and B604 the session import; the read-only routes (status, entries,
// metadata, download) arrive with the read-only stage and the remaining
// write routes (upload, folders, entries) with the write stages — every
// not-yet-implemented suffix answers Python's "unknown REST route" 404 in
// the meantime.
type RESTDispatcher struct {
	limits   ControlLimits
	rootName *RootNameController
	session  *SessionImporter
}

// NewRESTDispatcher wires the dispatcher; a zero limits value selects the
// AdapterApplication defaults.
func NewRESTDispatcher(limits ControlLimits, rootName *RootNameController, session *SessionImporter) (*RESTDispatcher, error) {
	if session == nil {
		return nil, errChainConfig("a session importer is required")
	}
	if limits.MaxControlBody <= 0 || limits.MaxResponseBody <= 0 {
		limits = DefaultControlLimits()
	}
	return &RESTDispatcher{limits: limits, rootName: rootName, session: session}, nil
}

// ServeREST fits Handlers.REST in the router.
func (d *RESTDispatcher) ServeREST(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	switch r.Method {
	case "GET":
		return d.doGet(w, r, route)
	case "PATCH":
		return d.doPatch(w, r, route)
	case "POST":
		if route.Suffix == "session/import" {
			return d.session.Import(w, r)
		}
		// The folders creation route lands with the write stages.
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	default:
		// PUT/POST/DELETE route suffixes land with the write stages;
		// unknown routes discard the body first, exactly like Python.
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
		return nil
	}
}

// settingsPayload keeps the Python response key order (status, name).
type settingsPayload struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func (d *RESTDispatcher) doGet(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if route.Suffix == "settings" {
		if err := discardBody(w, r, d.limits); err != nil {
			return err
		}
		name, err := d.rootName.Current()
		if err != nil {
			return err
		}
		return sendJSON(w, r, http.StatusOK, settingsPayload{Status: "ok", Name: name}, d.limits, nil)
	}
	// Python parses the path query before dispatching the known routes, so
	// a malformed path parameter errors even for unknown suffixes.
	if _, err := queryPath(route.Query); err != nil {
		return err
	}
	sendError(w, r, http.StatusNotFound, "unknown REST route", true, nil, false)
	return nil
}

func (d *RESTDispatcher) doPatch(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
	if route.Suffix != "settings" {
		// The entries/files rename-move routes land with the write stages.
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
