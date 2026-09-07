package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemoteEntryMarshalJSONExposesOnlyPublicFields(t *testing.T) {
	entry := RemoteEntry{
		ID:         "id-1",
		Name:       "report.pdf",
		Kind:       KindFile,
		ParentID:   Ptr("parent-1"),
		Size:       Ptr(int64(2048)),
		ModifiedAt: Ptr("2026-09-01T10:00:00Z"),
		Etag:       Ptr("etag-1"),
		LinkID:     Ptr("link-77"),
		Raw: map[string]any{
			"signed_url": "https://internal.example/download?token=secret",
			"link_id":    "link-77",
		},
	}
	got, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal() returned %v", err)
	}
	want := `{"id":"id-1","name":"report.pdf","kind":"file","parent_id":"parent-1",` +
		`"size":2048,"modified_at":"2026-09-01T10:00:00Z","etag":"etag-1"}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
	for _, secret := range []string{"link-77", "signed_url", "internal.example", "raw"} {
		if strings.Contains(string(got), secret) {
			t.Errorf("Marshal() leaked internal value %q: %s", secret, got)
		}
	}
}

func TestRemoteEntryMarshalJSONKeepsNullsForAbsentFields(t *testing.T) {
	entry := RemoteEntry{ID: "root", Name: "root", Kind: KindFolder}
	got, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal() returned %v", err)
	}
	want := `{"id":"root","name":"root","kind":"folder","parent_id":null,` +
		`"size":null,"modified_at":null,"etag":null}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestRemoteEntryPublicProjectionExcludesInternals(t *testing.T) {
	entry := RemoteEntry{
		ID:     "id-2",
		Name:   "docs",
		Kind:   KindFolder,
		LinkID: Ptr("link-88"),
		Raw:    map[string]any{"x": "y"},
	}
	public := entry.Public()
	if public.ID != "id-2" || public.Name != "docs" || public.Kind != KindFolder {
		t.Errorf("Public() = %+v, want id/name/kind copied", public)
	}
	if public.ParentID != nil || public.Size != nil || public.ModifiedAt != nil || public.Etag != nil {
		t.Errorf("Public() = %+v, want absent optional fields as nil", public)
	}
}

func TestPtrReturnsPointerToValue(t *testing.T) {
	value := "v"
	if got := Ptr(value); got == &value || *got != "v" {
		t.Errorf("Ptr() = %v, want a distinct pointer holding v", got)
	}
}

func TestWpsStatusJSONKeyOrder(t *testing.T) {
	lastChecked := 1725500000
	status := WpsStatus{
		Status:        "ok",
		Wps:           "connected",
		Workspace:     "ready",
		AccountType:   "enterprise",
		LastCheckedAt: &lastChecked,
	}
	got, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal() returned %v", err)
	}
	want := `{"status":"ok","wps":"connected","workspace":"ready",` +
		`"account_type":"enterprise","last_checked_at":1725500000,"retry_after":0}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}

	status.LastCheckedAt = nil
	got, err = json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal() returned %v", err)
	}
	want = `{"status":"ok","wps":"connected","workspace":"ready",` +
		`"account_type":"enterprise","last_checked_at":null,"retry_after":0}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestWpsStatusWithRetryAfterClampsAndCopies(t *testing.T) {
	original := WpsStatus{Status: "upstream_unavailable", Wps: "session_expired", Workspace: "pending"}
	cases := []struct {
		input int
		want  int
	}{
		{-5, 0},
		{0, 0},
		{60, 60},
	}
	for _, tc := range cases {
		got := original.WithRetryAfter(tc.input)
		if got.RetryAfter != tc.want {
			t.Errorf("WithRetryAfter(%d).RetryAfter = %d, want %d", tc.input, got.RetryAfter, tc.want)
		}
		if got.Status != original.Status || got.Wps != original.Wps || got.Workspace != original.Workspace {
			t.Errorf("WithRetryAfter(%d) = %+v, want other fields copied", tc.input, got)
		}
		if original.RetryAfter != 0 {
			t.Errorf("WithRetryAfter(%d) mutated the receiver", tc.input)
		}
	}
}

func TestDefaultUploadOptionsMatchCapturedShape(t *testing.T) {
	got := DefaultUploadOptions()
	if got.SuccessActionStatus != 200 {
		t.Errorf("SuccessActionStatus = %d, want 200", got.SuccessActionStatus)
	}
	if !got.WithRapid {
		t.Errorf("WithRapid = false, want true")
	}
	if got.ReqByInternal || got.IsUpNewVer {
		t.Errorf("ReqByInternal/IsUpNewVer = %+v, want false", got)
	}
	if got.ClientStores != "" || got.StartsWithFilename != "" {
		t.Errorf("string fields = %q/%q, want empty", got.ClientStores, got.StartsWithFilename)
	}
	if got.FileID != 0 {
		t.Errorf("FileID = %d, want 0", got.FileID)
	}
	if len(got.ParentPath) != 0 || len(got.TriedStore) != 0 {
		t.Errorf("slices = %v/%v, want empty", got.ParentPath, got.TriedStore)
	}
}
