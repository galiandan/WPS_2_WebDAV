package httpserver

import (
	"net/http"
	"time"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// RESTReadStorage is the read-only storage surface the REST GET routes use.
// Both the single-space Storage and the MultiSpace view satisfy it; the
// write and download surfaces arrive with their own stages.
type RESTReadStorage interface {
	Metadata(path string) (model.RemoteEntry, error)
	ListPath(path string) ([]model.RemoteEntry, error)
}

// StatusRootIDSource supplies the root id the status preflight reports for.
// The single-space view exposes a plain root id while the multi-space view
// resolves its first mount, so the value arrives through a method.
type StatusRootIDSource interface {
	StatusRootID() (string, error)
}

// StatusChecker is the redacted WPS session preflight. It mirrors the
// Python client's check_status: a cached, deliberately coarse result that
// never exposes credentials or identifiers.
type StatusChecker interface {
	CheckStatus(rootID string) (model.WpsStatus, error)
}

// statusShapeError marks a root/probe failure that Python's current_wps_status
// catches (AttributeError, OSError, TypeError, ValueError): the caller
// answers the redacted invalid_response status instead of an HTTP error.
type statusShapeError struct{}

func (*statusShapeError) Error() string { return "wps status shape is invalid" }

// StatusController mirrors AdapterApplication.current_wps_status: without a
// checker the adapter reports not_configured, and shape failures while
// resolving the root or probing answer the redacted invalid_response
// status. WpsApiError values are not shape failures — they propagate to the
// error table exactly like Python's uncaught WpsApiError.
type StatusController struct {
	roots   StatusRootIDSource
	checker StatusChecker
	now     func() int
}

// NewStatusController wires the preflight. A nil checker keeps the
// not_configured answer; roots only matters once a checker exists.
func NewStatusController(roots StatusRootIDSource, checker StatusChecker) *StatusController {
	return &StatusController{roots: roots, checker: checker, now: func() int { return int(time.Now().Unix()) }}
}

// Current runs the preflight for the status route.
func (c *StatusController) Current() (model.WpsStatus, error) {
	checkedAt := c.now()
	if c.checker == nil {
		return notConfiguredWpsStatus(checkedAt), nil
	}
	rootID, err := c.statusRootID()
	if err != nil {
		if _, isAPI := model.AsWpsAPIError(err); isAPI {
			return model.WpsStatus{}, err
		}
		return invalidResponseWpsStatus(checkedAt), nil
	}
	status, err := c.checker.CheckStatus(rootID)
	if err != nil {
		if _, isAPI := model.AsWpsAPIError(err); isAPI {
			return model.WpsStatus{}, err
		}
		return invalidResponseWpsStatus(checkedAt), nil
	}
	return status, nil
}

func (c *StatusController) statusRootID() (string, error) {
	if c.roots == nil {
		return "", &statusShapeError{}
	}
	return c.roots.StatusRootID()
}

func notConfiguredWpsStatus(checkedAt int) model.WpsStatus {
	return model.WpsStatus{
		Status:        "not_configured",
		Wps:           "not_configured",
		Workspace:     "not_configured",
		AccountType:   "unknown",
		LastCheckedAt: model.Ptr(checkedAt),
	}
}

func invalidResponseWpsStatus(checkedAt int) model.WpsStatus {
	return model.WpsStatus{
		Status:        "invalid_response",
		Wps:           "unknown",
		Workspace:     "unknown",
		AccountType:   "unknown",
		LastCheckedAt: model.Ptr(checkedAt),
	}
}

// listPayload keeps the Python response key order (path, entries).
type listPayload struct {
	Path    string              `json:"path"`
	Entries []model.PublicEntry `json:"entries"`
}

// metadataPayload keeps the Python response key order (path, entry).
type metadataPayload struct {
	Path  string            `json:"path"`
	Entry model.PublicEntry `json:"entry"`
}

// doEntries mirrors the entries/list GET routes: metadata first (a file
// answers 409 "the requested path is not a folder"), then the listing.
func (d *RESTDispatcher) doEntries(w http.ResponseWriter, r *http.Request, path string) error {
	entry, err := d.read.Metadata(path)
	if err != nil {
		return err
	}
	if entry.Kind != model.KindFolder {
		return model.NewStorageError(model.KindNotFolder, "the requested path is not a folder")
	}
	children, err := d.read.ListPath(path)
	if err != nil {
		return err
	}
	payload := listPayload{Path: path, Entries: make([]model.PublicEntry, 0, len(children))}
	for _, item := range children {
		payload.Entries = append(payload.Entries, item.Public())
	}
	return sendJSON(w, r, http.StatusOK, payload, d.limits, nil)
}

// doMetadata mirrors the metadata GET route: no kind check, the entry is
// echoed with the raw request path.
func (d *RESTDispatcher) doMetadata(w http.ResponseWriter, r *http.Request, path string) error {
	entry, err := d.read.Metadata(path)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, metadataPayload{Path: path, Entry: entry.Public()}, d.limits, nil)
}
