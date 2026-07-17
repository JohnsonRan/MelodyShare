//go:build windows

package storage

import "golang.org/x/sys/windows"

// FreeSpace returns the bytes available to this process on the volume that
// holds the files directory.
func (l *Local) FreeSpace() (int64, error) {
	p, err := windows.UTF16PtrFromString(l.filesDir)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return int64(free), nil
}
