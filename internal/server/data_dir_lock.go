package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDataDirectory obtains an advisory, process-scoped exclusive lock on the
// directory inode itself. It creates no lockfile and can be used by read-only
// offline commands. Every writer operated by this project must hold the same
// lock for its entire lifetime. It does not stop external, noncooperating tools.
func lockDataDirectory(root string) (*os.File, error) {
	if root == "" {
		return nil, errors.New("data directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("data directory must be a real directory")
	}
	dir, err := os.Open(absolute)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(before, opened) {
		dir.Close()
		return nil, errors.New("data directory changed while opening")
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		dir.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("data directory is locked by another process: %w", err)
		}
		return nil, fmt.Errorf("data directory lock unavailable: %w", err)
	}
	after, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(opened, after) {
		dir.Close()
		return nil, errors.New("data directory changed while locking")
	}
	return dir, nil
}
