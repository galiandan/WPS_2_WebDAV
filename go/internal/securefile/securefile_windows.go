//go:build windows

package securefile

import "os"

// supported reports whether the platform implements the POSIX secure-file
// discipline. Windows exists only to build development fixtures: reads fail
// closed with an explicit error instead of pretending the POSIX mode and
// owner checks passed.
func supported() bool { return false }

func ownedByService(info os.FileInfo) bool { return false }

func openSecure(path string) (*os.File, error) {
	return nil, errCode(CodeUnsupportedPlatform)
}

func validateParent(path string, requireExisting bool) error {
	return errCode(CodeUnsupportedPlatform)
}
