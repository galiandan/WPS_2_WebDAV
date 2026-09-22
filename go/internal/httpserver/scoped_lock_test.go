package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

type memberLockRead struct {
	*fakeReadStorage
	prefix string
	err    error
}

func (s memberLockRead) LockPath(path string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	canonical, err := canonicalRemotePath(path)
	if err != nil {
		return "", err
	}
	if canonical == "/" {
		return s.prefix, nil
	}
	return s.prefix + canonical, nil
}

func TestScopedRESTAndDAVSharePhysicalLockNamespace(t *testing.T) {
	read := memberLockRead{fakeReadStorage: &fakeReadStorage{}, prefix: "/Private/member"}
	dispatcher := newReadDispatcher(t, read, nil)
	lock, err := dispatcher.locks.Acquire("/Private/member/note.txt", "0", "", 30, "")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("PUT", "/api/v1/upload?path=/note.txt", nil)
	recorder := httptest.NewRecorder()
	allowed, err := dispatcher.checkLocks(recorder, request, "/note.txt")
	if err != nil || allowed || recorder.Code != http.StatusLocked || strings.Contains(recorder.Body.String(), "Private") {
		t.Fatalf("REST alias bypassed/exposed lock: %t %v %d %s", allowed, err, recorder.Code, recorder.Body.String())
	}
	dav := &DAVDispatcher{storage: read, locks: dispatcher.locks}
	if allowed, err := dav.checkLocks(httptest.NewRecorder(), request, "/note.txt"); err != nil || allowed {
		t.Fatalf("DAV alias bypassed lock: %t %v", allowed, err)
	}
	request.Header.Set("Lock-Token", "<"+lock.Token+">")
	if allowed, err := dispatcher.checkLocks(httptest.NewRecorder(), request, "/note.txt"); err != nil || !allowed {
		t.Fatalf("member with token blocked: %t %v", allowed, err)
	}
	if allowed, err := dispatcher.allowsTree("/", nil); err != nil || allowed {
		t.Fatalf("batch missed locked descendant: %t %v", allowed, err)
	}
	if allowed, err := dispatcher.allowsTree("/", map[string]struct{}{lock.Token: {}}); err != nil || !allowed {
		t.Fatalf("batch rejected lock owner: %t %v", allowed, err)
	}
	if path, err := dispatcher.lockPath("/%2F"); err != nil || path != "/Private/member/%2F" {
		t.Fatalf("lock path decoded twice: %s %v", path, err)
	}
	upload := &fakeUploadStorage{}
	dispatcher.uploads = upload
	writeRequest := restUploadRequest("PUT", "/api/v1/upload?path=/note.txt", []byte("new"))
	writeResponse := httptest.NewRecorder()
	readRouter(t, dispatcher).ServeHTTP(writeResponse, writeRequest)
	if writeResponse.Code != http.StatusLocked || upload.name != "" {
		t.Fatalf("REST upload route skipped mapped lock: %d", writeResponse.Code)
	}
}

func TestScopedRESTLockMappingFailurePreventsMutation(t *testing.T) {
	denied := model.NewStorageError(model.KindPermissionDenied, "configured root has changed")
	read := memberLockRead{fakeReadStorage: &fakeReadStorage{}, err: denied}
	dispatcher := newReadDispatcher(t, read, nil)
	if _, err := dispatcher.lockPath("/file"); !errors.Is(err, denied) {
		t.Fatalf("mapping failure lost: %v", err)
	}
	if allowed, err := dispatcher.allowsTree("/file", nil); allowed || !errors.Is(err, denied) {
		t.Fatal("mapping error allowed batch")
	}
	if allowed, err := dispatcher.allowsLock("/file", nil); allowed || !errors.Is(err, denied) {
		t.Fatal("mapping error allowed parent")
	}
	for _, rest := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		mapError(recorder, httptest.NewRequest("DELETE", "/test", nil), denied, rest)
		if recorder.Code != 403 || strings.Contains(recorder.Body.String(), "configured") {
			t.Fatalf("permission error not redacted: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}
