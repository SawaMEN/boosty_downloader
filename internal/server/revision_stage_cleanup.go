package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Run only at startup under the data-directory lock, after recovery of
// archive/history journals. A published revision is never a target.
func cleanupAbandonedRevisionStages(root string) error {
	for _, name := range []string{sqliteHistoryJournalName, sqliteHistoryDeleteJournalName, revisionRestoreJournalName, purgeJournalName} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return errors.New("revision cleanup blocked by pending journal")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	history := filepath.Join(root, "revisions")
	if info, err := purgeSafePath(root, history); err != nil || info == nil {
		return err
	}
	return walkRevisionStageParents(root, history, 0)
}

func walkRevisionStageParents(root, parent string, depth int) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(parent, name)
		if depth < 3 {
			if !safeArchivePart(name) || !entry.IsDir() {
				return errors.New("unsafe revision parent")
			}
			if info, err := purgeSafePath(root, path); err != nil || info == nil {
				return errors.New("unsafe revision parent path")
			}
			if err := walkRevisionStageParents(root, path, depth+1); err != nil {
				return err
			}
			continue
		}
		const prefix = ".incomplete-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if !validRevisionID(strings.TrimPrefix(name, prefix)) || !entry.IsDir() {
			return fmt.Errorf("unsafe orphaned revision stage: %s", name)
		}
		if info, err := purgeSafePath(root, path); err != nil || info == nil {
			return errors.New("unsafe revision stage path")
		}
		// RemoveAll does not follow symlinks within the private stage.
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return syncMetadataDirectory(parent)
	}
	return nil
}
