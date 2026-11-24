package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
)

// hashFileTransactionMetadata classifies a durable journal generation without
// allocating an entire JSON image. Reject symlinks and non-regular files
// throughout the path. A zero limit preserves the previous unbounded-size
// policy for restore/purge, while the read itself is bounded by fstat size.
func hashFileTransactionMetadata(root, name string, limit int64) (string, error) {
	file, err := openArchiveRegular(root, name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", err
	}
	if before.Size() < 0 || before.Size() == math.MaxInt64 || (limit > 0 && before.Size() > limit) {
		return "", errors.New("invalid file transaction metadata size")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, before.Size()+1))
	if err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	current, err := os.Lstat(filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, current) ||
		n != before.Size() || n != after.Size() || n != current.Size() ||
		!before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(current.ModTime()) {
		return "", errors.New("file transaction metadata changed during hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
