package model

import (
	"errors"
	"fmt"
)

// ErrorKind identifies a storage error category. The HTTP layer must map
// these to status codes by Kind — never by matching error text.
type ErrorKind string

const (
	KindInvalidPath          ErrorKind = "invalid_path"
	KindEntryNotFound        ErrorKind = "not_found"
	KindNotFolder            ErrorKind = "not_folder"
	KindAlreadyExists        ErrorKind = "already_exists"
	KindInsufficientStorage  ErrorKind = "insufficient_storage"
	KindServiceBusy          ErrorKind = "service_busy"
	KindAmbiguousPath        ErrorKind = "ambiguous_path"
	KindUnsupportedOperation ErrorKind = "unsupported_operation"
)

// StorageError is a domain error the HTTP layer can translate into a status
// code. It survives fmt.Errorf("%w") wrapping via errors.As.
type StorageError struct {
	Kind    ErrorKind
	Message string
}

// NewStorageError builds a classifiable storage error.
func NewStorageError(kind ErrorKind, message string) *StorageError {
	return &StorageError{Kind: kind, Message: message}
}

func (e *StorageError) Error() string {
	return e.Message
}

// AsStorageError extracts a *StorageError from a wrapped error chain.
func AsStorageError(err error) (*StorageError, bool) {
	var storageErr *StorageError
	if errors.As(err, &storageErr) {
		return storageErr, true
	}
	return nil, false
}

// WPS error categories, mirroring client.py's WpsApiError usage.
const (
	WpsCategoryUpstream        = "upstream"
	WpsCategoryDisabled        = "disabled"
	WpsCategoryHTTP            = "http"
	WpsCategoryInvalidResponse = "invalid_response"
	WpsCategorySessionExpired  = "session_expired"
	WpsCategoryUnavailable     = "unavailable"
)

// WpsAPIError reports an upstream API or transport failure. It deliberately
// carries only the operation name, HTTP status, and category: response
// bodies and URLs never travel inside this error.
type WpsAPIError struct {
	Operation string
	Status    int // 0 means the failure had no HTTP status.
	Category  string
}

// NewWpsAPIError builds a WPS error; pass status 0 when there is no HTTP
// status and WpsCategoryUpstream for the default category.
func NewWpsAPIError(operation string, status int, category string) *WpsAPIError {
	return &WpsAPIError{Operation: operation, Status: status, Category: category}
}

func (e *WpsAPIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("WPS operation failed: %s", e.Operation)
	}
	return fmt.Sprintf("WPS operation failed: %s (HTTP %d)", e.Operation, e.Status)
}

// AsWpsAPIError extracts a *WpsAPIError from a wrapped error chain.
func AsWpsAPIError(err error) (*WpsAPIError, bool) {
	var apiErr *WpsAPIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
