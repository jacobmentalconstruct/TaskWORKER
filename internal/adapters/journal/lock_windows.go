package journal

import (
	"os"
	"syscall"
)

// Share mode zero prevents another owner from opening this persistent lock file.
// The kernel releases the handle on process death; file existence is irrelevant.
func lockDirectory(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// Windows does not provide the POSIX directory fsync guarantee through os.File.
// Header/file Sync is required; directory-entry power-loss durability is a gap.
func syncDirectory(string) error { return nil }
