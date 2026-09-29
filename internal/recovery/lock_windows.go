//go:build windows

package recovery

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION.
const errSharingViolation syscall.Errno = 32

// lockDir opens the lock file without sharing: Windows refuses to open it
// again, from any process, until the handle is closed.
func lockDir(dir string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(filepath.Join(dir, lockFileName))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, fmt.Errorf("%w (%s)", ErrLocked, dir)
		}
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	return func() { _ = syscall.CloseHandle(h) }, nil
}
