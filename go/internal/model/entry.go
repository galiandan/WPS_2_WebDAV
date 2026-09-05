// Package model holds the pure data structures and error taxonomy shared by
// the WPS client, storage, and HTTP layers. It must stay dependency-free.
package model

import "encoding/json"

// EntryKind mirrors the Python provider's EntryKind literal.
type EntryKind string

const (
	KindFile    EntryKind = "file"
	KindFolder  EntryKind = "folder"
	KindUnknown EntryKind = "unknown"
)

// Ptr returns a pointer to v for optional entry fields.
func Ptr[T any](v T) *T {
	return &v
}

// RemoteEntry is the normalized metadata shared by the WPS and protocol layers.
//
// LinkID and Raw are internal WPS details: they drive upload and cache
// behavior inside the WPS layer and must never reach REST or WebDAV
// responses. MarshalJSON drops both, so even accidental serialization cannot
// leak them; protocol layers should use the Public projection explicitly.
type RemoteEntry struct {
	ID         string
	Name       string
	Kind       EntryKind
	ParentID   *string
	Size       *int64
	ModifiedAt *string
	Etag       *string
	// LinkID is the internal WPS file link identifier (the upload cid).
	LinkID *string
	// Raw is the untouched upstream payload kept for later WPS operations.
	Raw map[string]any
}

// PublicEntry is the REST-facing projection of a RemoteEntry: exactly the
// fields the Python REST layer serializes, in its JSON key order.
type PublicEntry struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       EntryKind `json:"kind"`
	ParentID   *string   `json:"parent_id"`
	Size       *int64    `json:"size"`
	ModifiedAt *string   `json:"modified_at"`
	Etag       *string   `json:"etag"`
}

// Public returns the projection without link ID or raw payload.
func (e RemoteEntry) Public() PublicEntry {
	return PublicEntry{
		ID:         e.ID,
		Name:       e.Name,
		Kind:       e.Kind,
		ParentID:   e.ParentID,
		Size:       e.Size,
		ModifiedAt: e.ModifiedAt,
		Etag:       e.Etag,
	}
}

// MarshalJSON always emits the public shape so internal fields cannot leak.
func (e RemoteEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Public())
}

// ListPage is one page of a folder listing with continuation metadata.
type ListPage struct {
	Entries    []RemoteEntry
	NextOffset *int
	NextFilter *string
	Result     *string
}

// WpsStatus is a deliberately redacted result of the WPS session preflight.
// Its JSON field order matches the Python as_dict() key order.
type WpsStatus struct {
	Status        string `json:"status"`
	Wps           string `json:"wps"`
	Workspace     string `json:"workspace"`
	AccountType   string `json:"account_type"`
	LastCheckedAt *int   `json:"last_checked_at"`
	RetryAfter    int    `json:"retry_after"`
}

// WithRetryAfter returns a copy with retry_after clamped to at least zero.
func (s WpsStatus) WithRetryAfter(value int) WpsStatus {
	if value < 0 {
		value = 0
	}
	s.RetryAfter = value
	return s
}

// UploadOptions carries the captured-shape defaults for the normal upload
// fallback; optional control fields stay provisional until a second
// successful replay confirms them.
type UploadOptions struct {
	ParentPath          []string
	ReqByInternal       bool
	ClientStores        string
	StartsWithFilename  string
	SuccessActionStatus int
	FileID              int64
	WithRapid           bool
	TriedStore          []string
	IsUpNewVer          bool
}

// DefaultUploadOptions returns the captured defaults of UploadOptions().
func DefaultUploadOptions() UploadOptions {
	return UploadOptions{
		SuccessActionStatus: 200,
		WithRapid:           true,
	}
}
