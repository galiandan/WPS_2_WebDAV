package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/storage"
)

type batchStorageFake struct {
	stubMutations
	entries map[string]model.RemoteEntry
	errors  map[string]error
	calls   []string
	cancel  context.CancelFunc
}

func (f *batchStorageFake) Metadata(path string) (model.RemoteEntry, error) {
	if e, ok := f.entries[path]; ok {
		return e, nil
	}
	return model.RemoteEntry{}, model.NewStorageError(model.KindEntryNotFound, "entry not found")
}
func (f *batchStorageFake) ListPath(string) ([]model.RemoteEntry, error) { return nil, nil }
func (f *batchStorageFake) ListChildren(string, model.RemoteEntry) ([]model.RemoteEntry, error) {
	return nil, nil
}
func (f *batchStorageFake) DeletePath(path string) error {
	f.calls = append(f.calls, "delete:"+path)
	if f.cancel != nil {
		f.cancel()
	}
	return f.errors[path]
}
func (f *batchStorageFake) MovePath(source, destination string) (model.RemoteEntry, error) {
	f.calls = append(f.calls, "move:"+source+":"+destination)
	return f.entries[source], f.errors[source]
}
func (f *batchStorageFake) CopyPath(ctx context.Context, source, destination string, options storage.CopyOptions) (model.RemoteEntry, error) {
	f.calls = append(f.calls, "copy:"+source+":"+destination+":"+options.Depth)
	if options.Overwrite {
		panic("batch must not overwrite")
	}
	return f.entries[source], f.errors[source]
}

func batchDispatcher(t *testing.T, fake *batchStorageFake) *RESTDispatcher {
	d := newReadDispatcher(t, fake, nil)
	d.mutations = fake
	return d
}

func batchRequest(t *testing.T, d *RESTDispatcher, body string) (*httptest.ResponseRecorder, batchResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/batch", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	w := httptest.NewRecorder()
	readRouter(t, d).ServeHTTP(w, r)
	var response batchResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return w, response
}

func TestBatchReportsEachResultAndRedactsUpstream(t *testing.T) {
	fake := &batchStorageFake{errors: map[string]error{
		"/bad": model.NewWpsAPIError("internal signed URL", 503, model.WpsCategoryHTTP),
	}}
	w, result := batchRequest(t, batchDispatcher(t, fake), `{"operation":"delete","paths":["/good","/bad","/later"]}`)
	if w.Code != 200 || result.Succeeded != 2 || result.Failed != 1 || len(result.Results) != 3 {
		t.Fatalf("status=%d response=%s", w.Code, w.Body.String())
	}
	if result.Results[1].Status != 502 || result.Results[1].Error != "upstream WPS request failed" || strings.Contains(w.Body.String(), "signed") {
		t.Fatalf("error leaked or misclassified: %s", w.Body.String())
	}
	if !reflect.DeepEqual(fake.calls, []string{"delete:/good", "delete:/bad", "delete:/later"}) {
		t.Fatal(fake.calls)
	}
}

func TestBatchValidationBeforeMutations(t *testing.T) {
	for _, body := range []string{
		`{"operation":"delete","paths":[]}`,
		`{"operation":"delete","paths":["/"]}`,
		`{"operation":"delete","paths":["/a","/a/"]}`,
		`{"operation":"delete","paths":["/a","/a/b"]}`,
		`{"operation":"delete","paths":["/safe","/a/../b"]}`,
		`{"operation":"delete","paths":["/safe",7]}`,
		`{"operation":"delete","paths":["/safe"],"overwrite":true}`,
		`{"operation":"copy","paths":["/a/file","/b/file"],"destination":"/target"}`,
		`{"operation":"move","paths":["/a"],"destination":"/a/sub"}`,
	} {
		t.Run(body, func(t *testing.T) {
			fake := &batchStorageFake{entries: map[string]model.RemoteEntry{"/target": folderEntry("dest"), "/a/sub": folderEntry("sub")}}
			w, _ := batchRequest(t, batchDispatcher(t, fake), body)
			if w.Code != 400 || len(fake.calls) != 0 {
				t.Fatalf("status=%d calls=%v body=%s", w.Code, fake.calls, w.Body.String())
			}
		})
	}
}

func TestBatchCopyAndMoveRejectConflictsAndPreserveLiteralPaths(t *testing.T) {
	for _, operation := range []string{"copy", "move"} {
		t.Run(operation, func(t *testing.T) {
			fake := &batchStorageFake{entries: map[string]model.RemoteEntry{
				"/target": folderEntry("dest"), "/target/existing": fileEntry("collision", "existing"),
			}}
			w, result := batchRequest(t, batchDispatcher(t, fake), `{"operation":"`+operation+`","paths":["/existing","/100%25.txt","/folder"],"destination":"/target"}`)
			if w.Code != 200 || result.Failed != 1 || result.Succeeded != 2 || result.Results[0].Status != 409 {
				t.Fatalf("%s", w.Body.String())
			}
			want := []string{operation + ":/100%25.txt:/target/100%25.txt", operation + ":/folder:/target/folder"}
			if operation == "copy" {
				for i := range want {
					want[i] += ":infinity"
				}
			}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Fatalf("calls=%v want=%v", fake.calls, want)
			}
		})
	}
}

func TestBatchHonorsDescendantAndDestinationLocks(t *testing.T) {
	fake := &batchStorageFake{entries: map[string]model.RemoteEntry{"/target": folderEntry("dest")}}
	d := batchDispatcher(t, fake)
	if _, err := d.locks.Acquire("/folder/child", "0", "", 3600, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.locks.Acquire("/target/file", "0", "", 3600, ""); err != nil {
		t.Fatal(err)
	}
	w, result := batchRequest(t, d, `{"operation":"move","paths":["/folder","/file","/other"],"destination":"/target"}`)
	if w.Code != 200 || result.Succeeded != 1 || result.Failed != 2 || result.Results[0].Status != 423 || result.Results[1].Status != 423 {
		t.Fatal(w.Body.String())
	}
	if !reflect.DeepEqual(fake.calls, []string{"move:/other:/target/other"}) {
		t.Fatal(fake.calls)
	}
}

func TestBatchStopsStartingItemsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &batchStorageFake{cancel: cancel}
	d := batchDispatcher(t, fake)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/batch", strings.NewReader(`{"operation":"delete","paths":["/a","/b"]}`)).WithContext(ctx)
	r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	readRouter(t, d).ServeHTTP(httptest.NewRecorder(), r)
	if !reflect.DeepEqual(fake.calls, []string{"delete:/a"}) {
		t.Fatal(fake.calls)
	}
}

func TestBatchAndArchiveRequireAuthenticationAndSameOrigin(t *testing.T) {
	fake := &batchStorageFake{}
	d := batchDispatcher(t, fake)
	chain, err := NewChain(ChainConfig{Router: readRouter(t, d), Health: func(http.ResponseWriter, *http.Request) {}, Auth: BasicAuthConfig{Username: "user", Password: "pass"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"batch", "archive"} {
		for _, authenticated := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/"+endpoint, strings.NewReader(`{"operation":"delete","paths":["/a"]}`))
			want := http.StatusUnauthorized
			if authenticated {
				r.SetBasicAuth("user", "pass")
				r.Header.Set("Origin", "https://foreign.example")
				want = http.StatusForbidden
			}
			w := httptest.NewRecorder()
			chain.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("%s auth=%t code=%d body=%s", endpoint, authenticated, w.Code, w.Body.String())
			}
		}
	}
	if len(fake.calls) != 0 {
		t.Fatal(fake.calls)
	}
}
