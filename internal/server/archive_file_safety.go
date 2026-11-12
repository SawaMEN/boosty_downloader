package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openArchiveRegular opens one archive file without following symlinks in
// ANY path component. os.Lstat followed by os.Open/ServeFile is vulnerable
// to replacement of the file or a parent directory between the two calls.
// This project runs on Linux; openat(O_NOFOLLOW) pins each checked directory.
func openArchiveRegular(base, relative string) (*os.File, error) {
	if relative == "" || strings.Contains(relative, "\\") {
		return nil, errors.New("invalid archive file path")
	}
	relParts := strings.Split(relative, "/")
	if len(relParts) > 2 {
		return nil, errors.New("archive file path too deep")
	}
	for _, part := range relParts {
		if !safeArchivePart(part) {
			return nil, errors.New("unsafe archive file component")
		}
	}
	full, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(relative)))
	if err != nil {
		return nil, err
	}
	components := strings.Split(strings.TrimPrefix(filepath.ToSlash(full), "/"), "/")
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for i, part := range components {
		if part == "" || part == "." || part == ".." {
			syscall.Close(fd)
			return nil, errors.New("invalid archive directory component")
		}
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		if i < len(components)-1 {
			flags |= syscall.O_DIRECTORY
		} else {
			// Do not block if a malicious replacement changes a media file
			// into a FIFO. Non-regular types are rejected after fstat.
			flags |= syscall.O_NONBLOCK
		}
		next, openErr := syscall.Openat(fd, part, flags, 0)
		syscall.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), full)
	if file == nil {
		syscall.Close(fd)
		return nil, errors.New("could not open archive file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("archive entry is not a regular file")
	}
	return file, nil
}

const maxArchiveManifestBytes = 16 << 20

// readArchiveManifestNoFollow ensures an attacker cannot swap a symlink into
// manifest.json to read a token, accounts file, or another owner's metadata.
func readArchiveManifestNoFollow(base string) ([]byte, ArchiveManifest, error) {
	var manifest ArchiveManifest
	file, err := openArchiveRegular(base, "manifest.json")
	if err != nil {
		return nil, manifest, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxArchiveManifestBytes+1))
	if err != nil {
		return nil, manifest, err
	}
	if len(raw) > maxArchiveManifestBytes {
		return nil, manifest, fmt.Errorf("archive manifest exceeds %d bytes", maxArchiveManifestBytes)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, manifest, err
	}
	return raw, manifest, nil
}

func serveArchiveRegular(w http.ResponseWriter, r *http.Request, base, relative string) {
	file, err := openArchiveRegular(base, relative)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; media-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filepath.Base(relative), info.ModTime(), file)
}
