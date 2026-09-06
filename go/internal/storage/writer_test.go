// The writer adapter delegates the ported methods to wps.Client (covered end
// to end in the wps package) and refuses the write methods whose migration
// stages have not landed yet with a fixed unsupported error.

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
	if err := writer.Move("x", "a", "b"); err == nil || err.Error() != want {
		t.Fatalf("move error = %v, want %q", err, want)
	}
}
