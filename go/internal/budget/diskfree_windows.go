//go:build windows

package budget

import (
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")

// diskFreeBytes reports the bytes available to the calling user on the
// drive containing path, mirroring shutil.disk_usage(...).free on Windows
// (GetDiskFreeSpaceExW's user-available figure).
func diskFreeBytes(path string) (int64, error) {
	var available, total, totalFree uint64
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	ret, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&available)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, callErr
	}
	return int64(available), nil
}
