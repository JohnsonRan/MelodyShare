//go:build !windows

package storage

import "golang.org/x/sys/unix"

// FreeSpace returns the bytes available to this process on the volume that
// holds the files directory.
func (l *Local) FreeSpace() (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(l.filesDir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
