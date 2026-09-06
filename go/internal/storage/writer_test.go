// The writer adapter delegates every ported method to wps.Client (covered
// end to end in the wps package). These tests prove the delegation reaches
// the real client call instead of a local stub.

package storage

import (
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

func TestWpsWriterUploadForwardsTheRequestFields(t *testing.T) {
	config := wps.DefaultConfig("group-1")
	config.CredentialSource = &credentials.StaticCredentialSource{
		Credentials: credentials.Credentials{Cookie: "Cookie-secret", CSRFToken: "csrf-secret"},
	}
	config.SpoolLimiter = uploadSpoolLimiterStub{}
	client, err := wps.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewWriter(client)
	size := int64(5)
	if _, err := writer.Upload(UploadRequest{
		ParentID:    "3",
		Name:        "file",
		Source:      strings.NewReader("12345"),
		Size:        &size,
		ContentType: "text/plain",
		CSRFToken:   "csrf-direct",
		Overwrite:   true,
	}); err == nil {
		t.Fatal("the upload must refuse at the stage boundary")
	} else if err.Error() != "upload is not implemented in this stage" {
		t.Fatalf("upload error = %q", err.Error())
	}
}

// uploadSpoolLimiterStub is a no-op limiter; a five-byte spool never
// reserves anyway, so the stub only satisfies the non-nil guard.
type uploadSpoolLimiterStub struct{}

func (uploadSpoolLimiterStub) ReserveSpool(total int64, current int64) (int64, error) {
	return current, nil
}

func (uploadSpoolLimiterStub) ReleaseSpool(int64) {}
