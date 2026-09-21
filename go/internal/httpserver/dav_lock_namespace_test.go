package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

type mappedLockStorage struct {
	*mapEntryStorage
	prefix string
}

func (s *mappedLockStorage) LockPath(path string) (string, error) {
	return canonicalRemotePath(strings.TrimSuffix(s.prefix, "/") + path)
}

func namespaceDispatcher(t *testing.T, read DAVStorage, locks *DavLockStore, mutations *recordingMutations) *DAVDispatcher {
	t.Helper()
	d, err := NewDAVDispatcher(read, DefaultControlLimits(), DAVLimits{}, DownloadLimits{}, stubDownloadStorage{}, stubUploadStorage{}, mutations, locks, 0, "/dav")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDAVLocksShareRESTNamespaceAndKeepDAVHrefs(t *testing.T) {
	locks := newTestLockStore(t)
	read := &mappedLockStorage{mapEntryStorage: &mapEntryStorage{byPath: map[string]model.RemoteEntry{"/file": fileEntryAt("file")}}, prefix: "/Space/web"}
	mutations := &recordingMutations{}
	d := namespaceDispatcher(t, read, locks, mutations)
	w := httptest.NewRecorder()
	if err := d.ServeDAV(w, writeRequest("LOCK", "/dav/file", nil, lockInfoBody("owner")), "/file"); err != nil {
		t.Fatal(err)
	}
	token := lockTokenOf(w)
	if w.Code != 200 || token == "" || !strings.Contains(w.Body.String(), "<D:href>/dav/file</D:href>") || strings.Contains(w.Body.String(), "/Space/") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if locks.Allows("/Space/web/file", nil) || !locks.Allows("/file", nil) {
		t.Fatal("lock was not registered in the browser namespace")
	}

	restFake := &batchStorageFake{}
	rest := batchDispatcher(t, restFake)
	rest.locks = locks
	w, result := batchRequest(t, rest, `{"operation":"delete","paths":["/Space/web/file"]}`)
	if w.Code != 200 || result.Failed != 1 || result.Results[0].Status != 423 || len(restFake.calls) != 0 {
		t.Fatalf("batch bypassed DAV lock: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	readRouter(t, rest).ServeHTTP(w, writeRequest("DELETE", "/api/v1/entries?path=/Space/web/file", nil, ""))
	if w.Code != 423 || len(restFake.calls) != 0 {
		t.Fatalf("REST delete bypassed DAV lock: %d", w.Code)
	}

	w = httptest.NewRecorder()
	if err := d.ServeDAV(w, writeRequest("LOCK", "/dav/file", map[string]string{"If": "(<" + token + ">)"}, ""), "/file"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || lockTokenOf(w) != token {
		t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
	}

	// A reconfigured DAV root is a different resource despite the same URL.
	read.prefix = "/Other/web"
	w = httptest.NewRecorder()
	if err := d.ServeDAV(w, writeRequest("UNLOCK", "/dav/file", map[string]string{"Lock-Token": "<" + token + ">"}, ""), "/file"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 409 || locks.Allows("/Space/web/file", nil) {
		t.Fatal("new mapping released a lock belonging to the old mapping")
	}
	read.prefix = "/Space/web"
	w = httptest.NewRecorder()
	if err := d.ServeDAV(w, writeRequest("UNLOCK", "/dav/file", map[string]string{"Lock-Token": "<" + token + ">"}, ""), "/file"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 204 || !locks.Allows("/Space/web/file", nil) {
		t.Fatalf("unlock status=%d", w.Code)
	}
}

func TestEveryDAVMutationChecksMappedLocks(t *testing.T) {
	for _, method := range []string{"PUT", "MKCOL", "DELETE", "MOVE", "COPY"} {
		for _, lockDestination := range []bool{false, true} {
			if lockDestination && method != "MOVE" && method != "COPY" {
				continue
			}
			t.Run(method+map[bool]string{false: " source", true: " destination"}[lockDestination], func(t *testing.T) {
				locks := newTestLockStore(t)
				lockPath := "/Space/web/file"
				if lockDestination {
					lockPath = "/Space/web/target"
				}
				if _, err := locks.Acquire(lockPath, "0", "", 3600, ""); err != nil {
					t.Fatal(err)
				}
				mutations := &recordingMutations{}
				read := &mappedLockStorage{mapEntryStorage: &mapEntryStorage{}, prefix: "/Space/web"}
				d := namespaceDispatcher(t, read, locks, mutations)
				w := httptest.NewRecorder()
				r := writeRequest(method, "/dav/file", map[string]string{"Destination": "/dav/target"}, "")
				if err := d.ServeDAV(w, r, "/file"); err != nil {
					t.Fatal(err)
				}
				if w.Code != http.StatusLocked {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if len(mutations.copies)+len(mutations.moves)+len(mutations.deletes)+len(mutations.folders) != 0 {
					t.Fatal("locked mutation ran")
				}
			})
		}
	}
}

func TestDAVRequestPinsSelectedRootBeforeCheckingLocks(t *testing.T) {
	locks := newTestLockStore(t)
	if _, err := locks.Acquire("/Space/web/file", "0", "", 3600, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	view, err := storage.NewDAVView(&storage.MultiSpace{}, func() (string, error) {
		calls++
		if calls > 1 {
			return "/Other/web", nil
		}
		return "/Space/web", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := namespaceDispatcher(t, view, locks, &recordingMutations{})
	w := httptest.NewRecorder()
	if err := d.ServeDAV(w, writeRequest("DELETE", "/dav/file", nil, ""), "/file"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 423 || calls != 1 {
		t.Fatalf("mapping changed during request: status=%d calls=%d", w.Code, calls)
	}
}
