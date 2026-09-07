//go:build unix

package securefile

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// supported reports whether the platform implements the POSIX discipline.
func supported() bool { return true }

// ownedByService accepts files owned by root or the service's own user,
// like the Python uid checks.
func ownedByService(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stat.Uid == 0 || stat.Uid == uint32(os.Getuid())
}

// openSecure opens read-only with O_NOFOLLOW and O_CLOEXEC so a symlink
// swapped in after the pre-open check fails instead of being followed.
func openSecure(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errCode(CodeOpenMissing)
		}
		return nil, errCode(CodeOpenFailed)
	}
	return file, nil
}

// validateParent mirrors the Python parent checks: no symlinked component
// anywhere in the parent, and — when it exists — a private directory owned
// by root or the service user. requireExisting rejects a missing parent
// (credential files); state files allow it while login is pending.
func validateParent(path string, requireExisting bool) error {
	parent := rawDirName(path)
	if parentHasSymlinkComponent(parent) {
		return errCode(CodeParentSymlink)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !requireExisting {
			return nil
		}
		return errCode(CodeParentUnavailable)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByService(info) {
		return errCode(CodeParentUnsafe)
	}
	return nil
}

// parentHasSymlinkComponent reproduces os.path.realpath(parent) !=
// os.path.abspath(parent) by walking every component, including ones above
// a missing tail that filepath.EvalSymlinks cannot resolve.
func parentHasSymlinkComponent(parent string) bool {
	current := ""
	for _, part := range strings.Split(parent, "/") {
		if part == "" {
			continue
		}
		current += "/" + part
		info, err := os.Lstat(current)
		if err != nil {
			// os.path.realpath(strict=False) keeps walking the literal
			// name; the parent stat below reports real availability.
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
