package storage

import "errors"

// ErrUploadTargetChanged means an update-only upload no longer names the
// previously read file. No writer call has been made when this is returned.
var ErrUploadTargetChanged = errors.New("the upload target changed")
