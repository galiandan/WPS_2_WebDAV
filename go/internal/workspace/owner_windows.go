//go:build windows

package workspace

import "os"

// OwnedByService skips the POSIX ownership check on the Windows development
// platform: the production service is Linux, and Windows has no direct
// equivalent of the uid rules. B302 formalises this policy in securefile.
func OwnedByService(info os.FileInfo) bool {
	return true
}
