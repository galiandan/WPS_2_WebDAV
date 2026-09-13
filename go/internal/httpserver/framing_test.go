package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/model"
)

// The web UI creates folders with a bodyless POST, and the Cloudflare
// Tunnel re-frames such requests as chunked on its HTTP/2-to-HTTP/1.1 hop.
// The boundary must route them and the REST dispatcher must drain the
// decoded empty body through discardBody — this pair of requests is the
// regression for the tunnel folder-creation 400.
func TestRestFolderCreationAcceptsChunkedFraming(t *testing.T) {
	mutations := &recordingMutations{entry: model.RemoteEntry{ID: "f9", Name: "chunked-dir", Kind: model.KindFolder, ParentID: model.Ptr("root"), Size: model.Ptr(int64(0))}}
	router, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/": rootFolder()}, mutations, nil)

	request := writeRequest("POST", "/api/v1/folders?path=/chunked-dir", nil, "")
	request.TransferEncoding = []string{"chunked"}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if len(mutations.folders) != 1 || mutations.folders[0] != "/chunked-dir" {
		t.Fatalf("folders = %v", mutations.folders)
	}

	// The same framing must survive on the DAV side: cc-switch's bodyless
	// PROPFIND (connection test) arrives chunked through the tunnel too.
	router2, _ := newWriteRouter(t, map[string]model.RemoteEntry{"/": rootFolder()}, mutations, nil)
	request = writeRequest("PROPFIND", "/dav/", map[string]string{"Depth": "0"}, "")
	request.TransferEncoding = []string{"chunked"}
	recorder = httptest.NewRecorder()
	router2.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("chunked PROPFIND status = %d, body %q", recorder.Code, recorder.Body.String())
	}
}
