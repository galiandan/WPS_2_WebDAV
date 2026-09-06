package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// The B700 completion gate replays the REST read-only records captured from
// the Python service (contract_tests/results/REST-*.json) against the Go
// dispatcher. The fake storage models the upstream listing exactly like the
// contract harness does; the records are Python's observed wire output, so
// a mismatch means the Go semantics drifted.

func contractResultsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Clean(filepath.Join("../../../contract_tests", "results"))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("contract results are missing: %v", err)
	}
	return dir
}

func loadRecord(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractResultsDir(t), name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// contractListing mirrors the contract harness listing (DEFAULT_LISTING plus
// the nulls.txt entry with fsize "not-a-number" and no mtime/fsha/parentid).
func contractListing() []model.RemoteEntry {
	return []model.RemoteEntry{
		{
			ID: "bench-file-1", Name: "bench-one.txt", Kind: model.KindFile,
			ParentID: model.Ptr("0"), Size: model.Ptr(int64(11)),
			ModifiedAt: model.Ptr("1788268272"), Etag: model.Ptr("bench-etag-bench-file-1"),
		},
		{
			ID: "bench-file-2", Name: "bench-two.txt", Kind: model.KindFile,
			ParentID: model.Ptr("0"), Size: model.Ptr(int64(11)),
			ModifiedAt: model.Ptr("1788268272"), Etag: model.Ptr("bench-etag-bench-file-2"),
		},
		{
			ID: "bench-dir-1", Name: "bench-folder", Kind: model.KindFolder,
			ParentID: model.Ptr("0"), Size: model.Ptr(int64(11)),
			ModifiedAt: model.Ptr("1788268272"), Etag: model.Ptr("bench-etag-bench-dir-1"),
		},
		{ID: "bench-null", Name: "nulls.txt", Kind: model.KindFile},
	}
}

// requestJSON issues a GET through the router and returns the parsed body.
func requestJSON(t *testing.T, router *Router, target string) (int, map[string]any, string, http.Header) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newTestRequest("GET", target))
	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	return recorder.Code, payload, recorder.Body.String(), recorder.Header()
}

func TestRESTReadContractGoldens(t *testing.T) {
	invalidPathErr := model.NewStorageError(model.KindInvalidPath, "remote paths must start with '/'")
	traversalErr := model.NewStorageError(model.KindInvalidPath, "remote path contains an empty or traversal component")

	t.Run("REST-STATUS-001", func(t *testing.T) {
		record := loadRecord(t, "REST-STATUS-001")
		payload := record["payload"].(map[string]any)
		checkedAt := int(payload["last_checked_at"].(float64))
		checker := &fakeStatusChecker{status: model.WpsStatus{
			Status:        payload["status"].(string),
			Wps:           payload["wps"].(string),
			Workspace:     payload["workspace"].(string),
			AccountType:   payload["account_type"].(string),
			LastCheckedAt: model.Ptr(checkedAt),
		}}
		router := readRouter(t, newReadDispatcher(t, &fakeReadStorage{}, NewStatusController(&fakeStatusRoots{rootID: "0"}, checker)))
		code, got, _, header := requestJSON(t, router, "/api/v1/status")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if header.Get("Content-Type") != record["content_type"] {
			t.Errorf("Content-Type = %q, want %q", header.Get("Content-Type"), record["content_type"])
		}
		if !reflect.DeepEqual(got, payload) {
			t.Errorf("payload = %v, want %v", got, payload)
		}
	})

	t.Run("REST-LIST-001", func(t *testing.T) {
		record := loadRecord(t, "REST-LIST-001")
		storage := &fakeReadStorage{metadataEntry: contractListing()[2], entries: contractListing()}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		code, got, _, header := requestJSON(t, router, "/api/v1/entries?path=%2F")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if header.Get("Content-Type") != record["content_type"] {
			t.Errorf("Content-Type = %q, want %q", header.Get("Content-Type"), record["content_type"])
		}
		if !reflect.DeepEqual(got, record["payload"]) {
			t.Errorf("payload = %v, want %v", got, record["payload"])
		}
	})

	t.Run("REST-LIST-002 alias", func(t *testing.T) {
		newRouter := func() *Router {
			return readRouter(t, newReadDispatcher(t, &fakeReadStorage{metadataEntry: contractListing()[2], entries: contractListing()}, nil))
		}
		_, _, entriesBody, _ := requestJSON(t, newRouter(), "/api/v1/entries?path=%2F")
		code, _, listBody, _ := requestJSON(t, newRouter(), "/api/v1/list?path=%2F")
		if code != 200 || entriesBody != listBody {
			t.Fatalf("list alias body = %q, entries body = %q (status %d)", listBody, entriesBody, code)
		}
	})

	rawCases := []struct {
		record  string
		storage *fakeReadStorage
		target  string
	}{
		{"REST-LIST-003", &fakeReadStorage{metadataEntry: contractListing()[0]}, "/api/v1/entries?path=%2Fbench-one.txt"},
		{"REST-LIST-004", &fakeReadStorage{metadataErr: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing")}, "/api/v1/entries?path=%2Fmissing"},
		{"REST-LIST-006", &fakeReadStorage{}, "/api/v1/entries?path="},
		{"REST-LIST-007", &fakeReadStorage{}, "/api/v1/entries?path=%2F&path=%2F"},
		{"REST-LIST-008", &fakeReadStorage{metadataErr: invalidPathErr}, "/api/v1/entries?path=abc"},
		{"REST-LIST-010", &fakeReadStorage{}, "/api/v1/nope"},
	}
	// The traversal record carries three cases with the same rule; the
	// Go storage rule itself is pinned in the storage package (B501), the
	// replay checks the REST framing around it.
	for _, tc := range rawCases {
		t.Run(tc.record, func(t *testing.T) {
			record := loadRecord(t, tc.record)
			router := readRouter(t, newReadDispatcher(t, tc.storage, nil))
			code, _, body, _ := requestJSON(t, router, tc.target)
			if code != int(record["status"].(float64)) {
				t.Fatalf("status = %d, want %v", code, record["status"])
			}
			if body != record["body"] {
				t.Errorf("body = %q, want %q", body, record["body"])
			}
		})
	}
	t.Run("REST-LIST-009 all traversal shapes", func(t *testing.T) {
		record := loadRecord(t, "REST-LIST-009")
		for label, observed := range record {
			target := map[string]string{
				"dotdot_first":  "/api/v1/entries?path=%2F..%2Fetc",
				"dotdot_middle": "/api/v1/entries?path=%2Fbench-folder%2F..%2Fbench-one.txt",
				"dot_component": "/api/v1/entries?path=%2F.%2Fbench-one.txt",
			}[label]
			observed := observed.(map[string]any)
			router := readRouter(t, newReadDispatcher(t, &fakeReadStorage{metadataErr: traversalErr}, nil))
			code, _, body, _ := requestJSON(t, router, target)
			if code != int(observed["status"].(float64)) || body != observed["body"] {
				t.Errorf("%s: status = %d body = %q, want %v %q", label, code, body, observed["status"], observed["body"])
			}
		}
	})

	t.Run("REST-LIST-005 default path", func(t *testing.T) {
		record := loadRecord(t, "REST-LIST-005")
		storage := &fakeReadStorage{metadataEntry: contractListing()[2], entries: contractListing()}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		code, got, _, _ := requestJSON(t, router, "/api/v1/entries")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if got["path"] != record["path"] {
			t.Errorf("path = %v, want %v", got["path"], record["path"])
		}
	})

	t.Run("REST-META-001", func(t *testing.T) {
		record := loadRecord(t, "REST-META-001")
		storage := &fakeReadStorage{metadataEntry: contractListing()[0]}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		code, got, _, _ := requestJSON(t, router, "/api/v1/metadata?path=%2Fbench-one.txt")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if !reflect.DeepEqual(got, record["payload"]) {
			t.Errorf("payload = %v, want %v", got, record["payload"])
		}
		missing := &fakeReadStorage{metadataErr: model.NewStorageError(model.KindEntryNotFound, "entry not found: missing.txt")}
		missingRouter := readRouter(t, newReadDispatcher(t, missing, nil))
		missingCode, _, _, _ := requestJSON(t, missingRouter, "/api/v1/metadata?path=%2Fmissing.txt")
		if missingCode != http.StatusNotFound {
			t.Errorf("missing status = %d, want 404", missingCode)
		}
	})

	t.Run("REST-ERROR-001", func(t *testing.T) {
		record := loadRecord(t, "REST-ERROR-001")
		storage := &fakeReadStorage{metadataErr: model.NewWpsAPIError("list entries", 500, model.WpsCategoryUpstream)}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		code, got, _, header := requestJSON(t, router, "/api/v1/entries?path=%2F")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if !reflect.DeepEqual(got, record["payload"]) {
			t.Errorf("payload = %v, want %v", got, record["payload"])
		}
		if header.Get("Retry-After") != "" {
			t.Errorf("Retry-After = %q, want absent", header.Get("Retry-After"))
		}
	})

	t.Run("REST-ERROR-002", func(t *testing.T) {
		record := loadRecord(t, "REST-ERROR-002")
		storage := &fakeReadStorage{metadataErr: model.NewWpsAPIError("list entries", 401, model.WpsCategorySessionExpired)}
		router := readRouter(t, newReadDispatcher(t, storage, nil))
		code, got, _, header := requestJSON(t, router, "/api/v1/entries?path=%2F")
		if code != int(record["status"].(float64)) {
			t.Fatalf("status = %d, want %v", code, record["status"])
		}
		if !reflect.DeepEqual(got, record["payload"]) {
			t.Errorf("payload = %v, want %v", got, record["payload"])
		}
		if header.Get("Retry-After") != record["retry_after"] {
			t.Errorf("Retry-After = %q, want %q", header.Get("Retry-After"), record["retry_after"])
		}
	})
}
