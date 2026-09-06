// The writer adapter delegates every ported method to wps.Client (covered
// end to end in the wps package). These tests prove the delegation reaches
// the real client call instead of a local stub.

package storage

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/galiandan/WPS_2_WebDAV/go/internal/credentials"
	"github.com/galiandan/WPS_2_WebDAV/go/internal/wps"
)

// Compile-time proof that the adapter satisfies the Writer surface.
var _ Writer = NewWriter(nil)

func TestWpsWriterDelegatesUploadToTheClient(t *testing.T) {
	// A nil client cannot answer, but the name guard fires first and its
	// exact message proves the call reached wps.Client.Upload.
	writer := NewWriter(nil)
	want := "name must be one remote file name"
	if _, err := writer.Upload(UploadRequest{Name: "a/b"}); err == nil || err.Error() != want {
		t.Fatalf("upload error = %v, want %q", err, want)
	}
}

// scriptedControlOpener scripts control-plane responses in order so the
// storage tests can drive the real client's full upload flow.
type scriptedControlOpener struct {
	responses []*http.Response
	requests  []*http.Request
}

func (o *scriptedControlOpener) Do(request *http.Request) (*http.Response, error) {
	o.requests = append(o.requests, request)
	if len(o.responses) == 0 {
		return nil, errors.New("no scripted control response left")
	}
	next := o.responses[0]
	o.responses = o.responses[1:]
	return next, nil
}

// scriptedObjectTransport answers the signed object PUT with an ETag.
type scriptedObjectTransport struct{}

func (scriptedObjectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Etag": []string{`"writer-etag"`}},
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func TestWpsWriterUploadForwardsTheRequestFields(t *testing.T) {
	config := wps.DefaultConfig("group-1")
	config.CredentialSource = &credentials.StaticCredentialSource{
		Credentials: credentials.Credentials{Cookie: "Cookie-secret", CSRFToken: "csrf-secret"},
	}
	config.SpoolLimiter = uploadSpoolLimiterStub{}
	opener := &scriptedControlOpener{responses: []*http.Response{
		jsonResponse(`{"result":"ok"}`),
		jsonResponse(`{"url":"https://hwc-bj.ag.kdocs.cn/u","response":{"expect_code":[200]},"store":"writer-store"}`),
		jsonResponse(`{"result":"ok","id":9,"fname":"file","ftype":"file","fsize":5,"parentid":3}`),
	}}
	client, err := wps.NewClient(config,
		wps.WithOpener(opener),
		wps.WithSignedTransport(scriptedObjectTransport{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewWriter(client)
	size := int64(5)
	entry, err := writer.Upload(UploadRequest{
		ParentID:    "3",
		Name:        "file",
		Source:      strings.NewReader("12345"),
		Size:        &size,
		ContentType: "text/plain",
		CSRFToken:   "csrf-direct",
		Overwrite:   true,
	})
	if err != nil {
		t.Fatalf("upload error = %v", err)
	}
	if entry.ID != "9" || entry.Name != "file" {
		t.Fatalf("entry = %+v", entry)
	}
	// The forwarded ContentType, CSRFToken, and Overwrite flag must surface
	// in the create_update body the client sent.
	createBody := readBody(t, opener.requests[1])
	for _, fragment := range []string{`"contenttype":"text/plain"`, `"csrfmiddlewaretoken":"csrf-direct"`, `"md5":"`} {
		if !strings.Contains(createBody, fragment) {
			t.Fatalf("create_update body %q lacks %q", createBody, fragment)
		}
	}
}

func jsonResponse(payload string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(payload)),
		ContentLength: int64(len(payload)),
	}
}

func readBody(t *testing.T, request *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// uploadSpoolLimiterStub is a no-op limiter; a five-byte spool never
// reserves anyway, so the stub only satisfies the non-nil guard.
type uploadSpoolLimiterStub struct{}

func (uploadSpoolLimiterStub) ReserveSpool(total int64, current int64) (int64, error) {
	return current, nil
}

func (uploadSpoolLimiterStub) ReleaseSpool(int64) {}
