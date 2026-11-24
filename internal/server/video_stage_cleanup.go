package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Video encodes use private data-root temporary directories instead of
// mutating live media. After an abrupt exit those large directories must not
// leak disk space or accidentally become part of a later full snapshot.
// Called during startup before background workers, under the data-dir lock.
func cleanupAbandonedVideoStages(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	cleaned := false
	for _, entry := range entries {
		name := entry.Name()
		const prefix = ".video-stage-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		// os.MkdirTemp appends an unsigned decimal value, which can
		// rarely have fewer than six digits. Preserve the legacy long
		// alphanumeric shape, but only allow decimal short suffixes.
		if len(suffix) == 0 || len(suffix) > 32 {
			return errors.New("unsafe orphaned video staging name")
		}
		for _, r := range suffix {
			digit := r >= '0' && r <= '9'
			if !digit && (len(suffix) < 6 || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')) {
				return errors.New("unsafe orphaned video staging suffix")
			}
		}
		path := filepath.Join(root, name)
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe orphaned video staging entry: %s", name)
		}
		if info, err := purgeSafePath(root, path); err != nil || info == nil {
			return fmt.Errorf("unsafe orphaned video staging path: %s: %w", name, err)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("cannot clean abandoned video output %s: %w", name, err)
		}
		cleaned = true
	}
	if cleaned {
		return syncMetadataDirectory(root)
	}
	return nil
}
