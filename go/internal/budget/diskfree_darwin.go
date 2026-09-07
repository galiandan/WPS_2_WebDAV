//go:build darwin

package budget

import "syscall"

// diskFreeBytes reports the bytes available to unprivileged users on the
// filesystem containing path. Darwin's statfs has no f_frsize; os.statvfs
// (used by shutil.disk_usage) synthesizes f_frsize from f_bsize, so Bsize
// is the equivalent multiplier here.
func diskFreeBytes(path string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
