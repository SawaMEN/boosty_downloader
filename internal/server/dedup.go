package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// deduplicateAsset stores identical media once, but leaves a regular file at
// each post's original path. This preserves HTTP Range, the local HTML player,
// ZIP exports and per-user ACL. No symlinks or external storage paths are used.
// Files must have passed SHA-256 verification before this is called.
// Hard links are limited to the same filesystem; unsupported filesystems keep
// their normal archive files instead of blocking the download.
func (s *Storage) deduplicateAsset(path, digest string, size int64) error {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || size < 0 {
		return errors.New("неверная контрольная сумма медиа")
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("неверный тип или размер медиа")
	}
	root := filepath.Join(s.root, "blobs", "sha256", digest[:2])
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	object := filepath.Join(root, digest)
	stored, err := os.Lstat(object)
	if os.IsNotExist(err) {
		if err := os.Link(path, object); err != nil {
			if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EOPNOTSUPP) {
				return nil // Filesystem does not support safe hardlinks.
			}
			return err
		}
		// A store object and its post hardlink are read-only. Downloads replace
		// a destination atomically instead of modifying the shared inode.
		return os.Chmod(object, 0400)
	}
	if err != nil {
		return err
	}
	if !stored.Mode().IsRegular() || stored.Size() != size {
		return errors.New("хранилище одинаковых файлов повреждено")
	}
	// Every candidate is checked against its digest, not only its size.
	valid, err := verifyAsset(object, ArchiveAsset{SHA256: digest, Size: size})
	if err != nil || !valid {
		return errors.New("хранилище одинаковых файлов не прошло SHA-256")
	}
	if os.SameFile(info, stored) {
		return nil
	}
	secret := make([]byte, 8)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".dedup-"+hex.EncodeToString(secret))
	if err := os.Link(object, temporary); err != nil {
		if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("не удалось заменить дубликат атомарно: %w", err)
	}
	return nil
}

// discardBlobIfUnreferenced is cheap enough to run after a media replacement.
// It avoids retaining a large orphan until the next user deletes an archive.
func (s *Storage) discardBlobIfUnreferenced(digest string) error {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return nil
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	object := filepath.Join(s.root, "blobs", "sha256", digest[:2], digest)
	info, err := os.Lstat(object)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("неожиданный объект в хранилище")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if ok && st.Nlink <= 1 {
		return os.Remove(object)
	}
	return nil
}

// collectUnusedBlobs reclaims the shared-object link after the last post
// referencing it has been deleted. It does not delete any post media itself.
func (s *Storage) collectUnusedBlobs() error {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	root := filepath.Join(s.root, "blobs", "sha256")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("неожиданный файл в хранилище медиа")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil // Unknown FS metadata: preserve object rather than delete.
		}
		if st.Nlink <= 1 {
			return os.Remove(path)
		}
		return nil
	})
}
