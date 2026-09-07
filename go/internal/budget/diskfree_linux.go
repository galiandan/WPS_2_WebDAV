//go:build linux

package budget

import "syscall"

// diskFreeBytes reports the bytes available to unprivileged users on the
// filesystem containing path, mirroring shutil.disk_usage(...).free
// (f_bavail * f_frsize).
func diskFreeBytes(path string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * fs.Frsize, nil
}
