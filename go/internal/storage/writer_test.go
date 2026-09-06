// The B900 writer adapter is a one-line delegation to wps.Client.CreateFolder
// (covered end to end in the wps package) plus fixed refusals for the write
// methods whose migration stages have not landed yet.

package storage

import "testing"

// Compile-time proof that the adapter satisfies the staged Writer surface.
var _ Writer = NewWriter(nil)

func TestWpsWriterRefusesUnportedMethods(t *testing.T) {
	writer := NewWriter(nil)
	want := "write operation is not implemented in this stage"
	if _, err := writer.Upload(UploadRequest{}); err == nil || err.Error() != want {
		t.Fatalf("upload error = %v, want %q", err, want)
	}
	if err := writer.Delete("x"); err == nil || err.Error() != want {
		t.Fatalf("delete error = %v, want %q", err, want)
	}
	if _, err := writer.Rename("x", "y"); err == nil || err.Error() != want {
		t.Fatalf("rename error = %v, want %q", err, want)
	}
	if err := writer.Move("x", "a", "b"); err == nil || err.Error() != want {
		t.Fatalf("move error = %v, want %q", err, want)
	}
}
