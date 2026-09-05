package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// errorRouter builds a router whose REST and DAV handlers return the given
// error, so every domain category can be driven through the mapping in both
// framing contexts.
func errorRouter(t *testing.T, err error) *Router {
	t.Helper()
	router, buildErr := NewRouter(RouterConfig{
		DAVPrefix:  "/dav",
		RESTPrefix: "/api/v1",
		Handlers: Handlers{
			Health:   func(w http.ResponseWriter, r *http.Request) {},
			WebApp:   func(w http.ResponseWriter, r *http.Request) {},
			WebAsset: func(w http.ResponseWriter, r *http.Request, name string) {},
			REST: func(w http.ResponseWriter, r *http.Request, route RESTRoute) error {
				return err
			},
			DAV: func(w http.ResponseWriter, r *http.Request, davPath string) error {
				return err
			},
		},
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return router
}

// TestErrorTableRESTGoldens is the B602 completion gate: every error
// category has a REST golden (compact JSON error body).
func TestErrorTableRESTGoldens(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
		wantRetry  string
		wantClose  bool
	}{
		{
			name:       "invalid path",
			err:        model.NewStorageError(model.KindInvalidPath, "remote paths must start with '/'"),
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"remote paths must start with '/'"}`,
		},
		{
			name:       "entry not found",
			err:        model.NewStorageError(model.KindEntryNotFound, "entry not found: missing.txt"),
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":"entry not found: missing.txt"}`,
		},
		{
			name:       "not a folder",
			err:        model.NewStorageError(model.KindNotFolder, "not a folder: /docs"),
			wantStatus: http.StatusConflict,
			wantBody:   `{"error":"not a folder: /docs"}`,
		},
		{
			name:       "already exists",
			err:        model.NewStorageError(model.KindAlreadyExists, "entry already exists: /docs"),
			wantStatus: http.StatusConflict,
			wantBody:   `{"error":"entry already exists: /docs"}`,
		},
		{
			name:       "ambiguous path",
			err:        model.NewStorageError(model.KindAmbiguousPath, "multiple entries have the name: dup"),
			wantStatus: http.StatusConflict,
			wantBody:   `{"error":"multiple entries have the name: dup"}`,
		},
		{
			name:       "insufficient storage",
			err:        model.NewStorageError(model.KindInsufficientStorage, "COPY exceeds the configured depth limit"),
			wantStatus: http.StatusInsufficientStorage,
			wantBody:   `{"error":"COPY exceeds the configured depth limit"}`,
		},
		{
			name:       "service busy",
			err:        model.NewStorageError(model.KindServiceBusy, "too many downloads are active"),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":"too many downloads are active"}`,
			wantRetry:  "5",
		},
		{
			name:       "unsupported operation",
			err:        model.NewStorageError(model.KindUnsupportedOperation, "cross-space move is not supported"),
			wantStatus: http.StatusNotImplemented,
			wantBody:   `{"error":"cross-space move is not supported"}`,
		},
		{
			name:       "request body too large",
			err:        errRequestBodyTooLarge(),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"error":""}`,
			wantClose:  true,
		},
		{
			name:       "control request error",
			err:        errBadRequest("request body must be valid JSON"),
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"request body must be valid JSON"}`,
		},
		{
			name:       "upstream session expired",
			err:        model.NewWpsAPIError("list entries", 401, model.WpsCategorySessionExpired),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":"WPS session expired; refresh the configured credentials","code":"wps_session_expired","upstream_status":401}`,
			wantRetry:  "60",
		},
		{
			name:       "upstream failure with status",
			err:        model.NewWpsAPIError("list entries", 502, model.WpsCategoryUpstream),
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":"upstream WPS request failed","code":"wps_unavailable","upstream_status":502}`,
		},
		{
			name:       "upstream failure without status",
			err:        model.NewWpsAPIError("read credential file", 0, model.WpsCategoryUpstream),
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":"upstream WPS request failed","code":"wps_unavailable"}`,
		},
		{
			name:       "unclassified error",
			err:        errors.New("unexpected"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":"internal server error"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := errorRouter(t, tc.err)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata?path=%2Fx"))
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := recorder.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			if got := recorder.Header().Get("Connection"); (got == "close") != tc.wantClose {
				t.Errorf("Connection = %q, wantClose %v", got, tc.wantClose)
			}
			if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(tc.wantBody)) {
				t.Errorf("Content-Length = %q, want %d", got, len(tc.wantBody))
			}
		})
	}
}

// TestErrorTableDAVGoldens gives every category the plain-text framing with
// the trailing newline.
func TestErrorTableDAVGoldens(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "invalid path",
			err:        model.NewStorageError(model.KindInvalidPath, "remote path contains a forbidden character"),
			wantStatus: http.StatusBadRequest,
			wantBody:   "remote path contains a forbidden character\n",
		},
		{
			name:       "entry not found",
			err:        model.NewStorageError(model.KindEntryNotFound, "entry not found: missing.txt"),
			wantStatus: http.StatusNotFound,
			wantBody:   "entry not found: missing.txt\n",
		},
		{
			name:       "not a folder",
			err:        model.NewStorageError(model.KindNotFolder, "not a folder: /docs"),
			wantStatus: http.StatusConflict,
			wantBody:   "not a folder: /docs\n",
		},
		{
			name:       "already exists",
			err:        model.NewStorageError(model.KindAlreadyExists, "entry already exists: /docs"),
			wantStatus: http.StatusConflict,
			wantBody:   "entry already exists: /docs\n",
		},
		{
			name:       "ambiguous path",
			err:        model.NewStorageError(model.KindAmbiguousPath, "multiple entries have the name: dup"),
			wantStatus: http.StatusConflict,
			wantBody:   "multiple entries have the name: dup\n",
		},
		{
			name:       "insufficient storage",
			err:        model.NewStorageError(model.KindInsufficientStorage, "COPY exceeds the configured entry limit"),
			wantStatus: http.StatusInsufficientStorage,
			wantBody:   "COPY exceeds the configured entry limit\n",
		},
		{
			name:       "service busy",
			err:        model.NewStorageError(model.KindServiceBusy, "too many uploads are active"),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "too many uploads are active\n",
		},
		{
			name:       "unsupported operation",
			err:        model.NewStorageError(model.KindUnsupportedOperation, "COPY overwrite is disabled because the relay is not atomic"),
			wantStatus: http.StatusNotImplemented,
			wantBody:   "COPY overwrite is disabled because the relay is not atomic\n",
		},
		{
			name:       "request body too large keeps the empty message",
			err:        errRequestBodyTooLarge(),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   "\n",
		},
		{
			name:       "upstream session expired stays text without codes",
			err:        model.NewWpsAPIError("list entries", 401, model.WpsCategorySessionExpired),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "WPS session expired; refresh the configured credentials\n",
		},
		{
			name:       "upstream failure stays text",
			err:        model.NewWpsAPIError("list entries", 500, model.WpsCategoryUpstream),
			wantStatus: http.StatusBadGateway,
			wantBody:   "upstream WPS request failed\n",
		},
		{
			name:       "unclassified error",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal server error\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := errorRouter(t, tc.err)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, newTestRequest("PROPFIND", "/dav/file.txt"))
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
		})
	}
}

// TestControlRequestErrorCloseFlag pins the short-body contract: framing
// failures close the connection, JSON parse failures do not.
func TestControlRequestErrorCloseFlag(t *testing.T) {
	router := errorRouter(t, errBadRequestClose("request body is shorter than Content-Length"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}

	router = errorRouter(t, errBadRequest("request body must be a JSON object"))
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/metadata"))
	if got := recorder.Header().Get("Connection"); got != "" {
		t.Errorf("JSON errors must not close: %q", got)
	}
}

// TestSendJSONEnforcesResponseLimit pins the 507 path: an oversized control
// response becomes a KindInsufficientStorage error before any header is
// written.
func TestSendJSONEnforcesResponseLimit(t *testing.T) {
	limits := ControlLimits{MaxControlBody: 1024, MaxResponseBody: 64}
	recorder := httptest.NewRecorder()
	err := sendJSON(recorder, newTestRequest("GET", "/api/v1/status"), http.StatusOK,
		map[string]string{"filler": strings.Repeat("x", 200)}, limits, nil)
	if err == nil {
		t.Fatal("expected a limit error")
	}
	storageErr, ok := model.AsStorageError(err)
	if !ok || storageErr.Kind != model.KindInsufficientStorage || storageErr.Message != "response exceeds the configured size limit" {
		t.Fatalf("error = %v", err)
	}
	// httptest recorders default to Code 200; nothing may have been
	// written before the limit check failed.
	if recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
		t.Errorf("response started before the limit check: body %q headers %v",
			recorder.Body.String(), recorder.Header())
	}

	// The mapped error surfaces as 507 with Python's message.
	router := errorRouter(t, err)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", "/api/v1/status"))
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"error":"response exceeds the configured size limit"}` {
		t.Errorf("body = %q", body)
	}

	// Under the limit the payload flows through untouched.
	recorder = httptest.NewRecorder()
	if err := sendJSON(recorder, newTestRequest("GET", "/api/v1/status"), http.StatusOK,
		map[string]string{"ok": "yes"}, limits, nil); err != nil {
		t.Fatalf("small payload rejected: %v", err)
	}
	if body := recorder.Body.String(); body != `{"ok":"yes"}` {
		t.Errorf("body = %q", body)
	}
}

// TestSendJSONDefaultsLimits mirrors the AdapterApplication defaults when a
// caller passes zero limits.
func TestSendJSONDefaultsLimits(t *testing.T) {
	defaults := DefaultControlLimits()
	if defaults.MaxControlBody != 1024*1024 {
		t.Errorf("MaxControlBody = %d", defaults.MaxControlBody)
	}
	if defaults.MaxResponseBody != 16*1024*1024 {
		t.Errorf("MaxResponseBody = %d", defaults.MaxResponseBody)
	}
	recorder := httptest.NewRecorder()
	if err := sendJSON(recorder, newTestRequest("GET", "/api/v1/status"), http.StatusOK,
		map[string]string{"k": strings.Repeat("x", 1024*1024)}, ControlLimits{}, nil); err != nil {
		t.Fatalf("payload within the default limit rejected: %v", err)
	}
}
