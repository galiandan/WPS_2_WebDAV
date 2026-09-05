//go:build !linux && !darwin && !windows

package budget

import "errors"

// diskFreeBytes has no implementation for this platform; spool reservations
// that need real free-space accounting fail closed.
func diskFreeBytes(path string) (int64, error) {
	return 0, errors.New("spool free space is unavailable on this platform")
}
