package model

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestStorageErrorCarriesKindAndMessage(t *testing.T) {
	cases := map[ErrorKind]string{
		KindInvalidPath:          "path must be absolute",
		KindEntryNotFound:        "entry does not exist",
		KindNotFolder:            "parent is a regular file",
		KindAlreadyExists:        "destination already exists",
		KindInsufficientStorage:  "upload spool directory is unavailable",
		KindServiceBusy:          "transfer limit reached",
		KindAmbiguousPath:        "duplicate names in one folder",
		KindUnsupportedOperation: "operation not confirmed by a safe capture",
	}
	for kind, message := range cases {
		err := NewStorageError(kind, message)
		if err.Kind != kind {
			t.Errorf("Kind = %q, want %q", err.Kind, kind)
		}
		if err.Error() != message {
			t.Errorf("Error() = %q, want %q", err.Error(), message)
		}
	}
}

type wrappedError struct{ inner error }

func (w wrappedError) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrappedError) Unwrap() error { return w.inner }

func TestStorageErrorClassificationSurvivesWrapping(t *testing.T) {
	inner := NewStorageError(KindEntryNotFound, "gone")
	cases := map[string]error{
		"unwrapped":          inner,
		"wrapped once":       fmt.Errorf("resolve /a/b: %w", inner),
		"wrapped twice":      fmt.Errorf("GET folder: %w", fmt.Errorf("resolve /a/b: %w", inner)),
		"custom wrap struct": wrappedError{inner},
	}
	for name, err := range cases {
		got, ok := AsStorageError(err)
		if !ok {
			t.Errorf("%s: AsStorageError() = ok=false, want the wrapped error", name)
			continue
		}
		if got.Kind != KindEntryNotFound {
			t.Errorf("%s: Kind = %q, want %q", name, got.Kind, KindEntryNotFound)
		}
	}
	if _, ok := AsStorageError(errors.New("plain")); ok {
		t.Errorf("AsStorageError(plain error) = ok=true, want false")
	}

	// A WPS upstream error must never classify as a storage error.
	if _, ok := AsStorageError(NewWpsAPIError("list", 502, WpsCategoryUpstream)); ok {
		t.Errorf("AsStorageError(WpsAPIError) = ok=true, want false")
	}
}

func TestWpsAPIErrorMessageMatchesPythonFormat(t *testing.T) {
	cases := []struct {
		operation string
		status    int
		want      string
	}{
		{"list_files", 0, "WPS operation failed: list_files"},
		{"list_files", 502, "WPS operation failed: list_files (HTTP 502)"},
		{"object upload", 409, "WPS operation failed: object upload (HTTP 409)"},
	}
	for _, tc := range cases {
		err := NewWpsAPIError(tc.operation, tc.status, WpsCategoryUpstream)
		if err.Error() != tc.want {
			t.Errorf("Error() = %q, want %q", err.Error(), tc.want)
		}
	}
}

func TestWpsAPIErrorClassificationSurvivesWrapping(t *testing.T) {
	inner := NewWpsAPIError("download", 401, WpsCategorySessionExpired)
	cases := map[string]error{
		"unwrapped":     inner,
		"wrapped once":  fmt.Errorf("open stream: %w", inner),
		"wrapped twice": fmt.Errorf("GET /dav/a: %w", fmt.Errorf("open stream: %w", inner)),
	}
	for name, err := range cases {
		got, ok := AsWpsAPIError(err)
		if !ok {
			t.Errorf("%s: AsWpsAPIError() = ok=false, want the wrapped error", name)
			continue
		}
		if got.Operation != "download" || got.Status != 401 || got.Category != WpsCategorySessionExpired {
			t.Errorf("%s: got %+v, want operation/status/category preserved", name, got)
		}
	}
	if _, ok := AsWpsAPIError(errors.New("plain")); ok {
		t.Errorf("AsWpsAPIError(plain error) = ok=true, want false")
	}
	if _, ok := AsWpsAPIError(NewStorageError(KindServiceBusy, "busy")); ok {
		t.Errorf("AsWpsAPIError(StorageError) = ok=true, want false")
	}
}

func TestWpsAPIErrorHoldsOnlySafeFields(t *testing.T) {
	err := NewWpsAPIError("op", 500, WpsCategoryUpstream)
	value := reflect.ValueOf(*err)
	fieldNames := make([]string, 0, value.NumField())
	for i := 0; i < value.NumField(); i++ {
		fieldNames = append(fieldNames, value.Type().Field(i).Name)
	}
	want := []string{"Operation", "Status", "Category"}
	if !reflect.DeepEqual(fieldNames, want) {
		t.Errorf("WpsAPIError fields = %v, want %v (no body or URL fields may be added)",
			strings.Join(fieldNames, ","), strings.Join(want, ","))
	}
}
