//go:build unix

package workspace

import (
	"os"
	"syscall"
)

// OwnedByService accepts files owned by root or the service's own user,
// like the Python mode checks. The securefile package owns the full
// validation from B302 onward; this helper serves workspace and config
// load-time checks.
func OwnedByService(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stat.Uid == 0 || stat.Uid == uint32(os.Getuid())
}
