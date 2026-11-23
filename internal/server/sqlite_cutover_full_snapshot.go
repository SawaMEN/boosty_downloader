package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fullCutoverSnapshotPath ensures the snapshot cannot overlap the live
// tree. The prepared intent pins its resolved physical location.
func fullCutoverSnapshotPath(root, snapshot string) (string, error) {
	if snapshot == "" {
		return "", errors.New("full data recovery snapshot is required")
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	physicalRoot, err = filepath.Abs(physicalRoot)
	if err != nil {
		return "", err
	}
	physical, err := filepath.EvalSymlinks(snapshot)
	if err != nil {
		return "", err
	}
	physical, err = filepath.Abs(physical)
	if err != nil {
		return "", err
	}
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))))
	}
	if inside(physicalRoot, physical) || inside(physical, physicalRoot) {
		return "", errors.New("full snapshot and live data directory overlap")
	}
	return physical, nil
}

// Verify both the copied bytes and the live source tree, including secrets,
// comments, HTML, archived revisions and unindexed private files. A complete
// but stale backup is not a safe cutover recovery point. Only the exact
// root-level rehearsal intent may be excluded during its own recovery.
// Caller holds the stopped server's data-directory lock.
func verifyFullCutoverSnapshotLocked(root, snapshot string, recovering bool) (string, error) {
	physical, err := fullCutoverSnapshotPath(root, snapshot)
	if err != nil {
		return "", err
	}
	raw, err := backupReadPrivateFile(physical, fullSnapshotManifestName)
	if err != nil || len(raw) > fullSnapshotManifestMaxBytes {
		return "", errors.New("cannot read bounded full recovery manifest")
	}
	digest := metadataDigest(raw)
	if err := VerifyFullDataSnapshot(physical); err != nil {
		return "", fmt.Errorf("invalid full recovery snapshot: %w", err)
	}
	var manifest fullDataSnapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || uint32(rootInfo.Mode().Perm()) != manifest.RootMode {
		return "", errors.New("live data root mode differs from recovery snapshot")
	}
	expected := make(map[string]fullDataSnapshotEntry, len(manifest.Entries))
	for _, item := range manifest.Entries {
		if item.Path == sqliteCutoverIntentName {
			return "", errors.New("full snapshot includes a pending cutover intent")
		}
		expected[item.Path] = item
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if recovering && key == sqliteCutoverIntentName {
			if !entry.Type().IsRegular() {
				return errors.New("unsafe cutover intent during full snapshot verification")
			}
			return nil
		}
		item, ok := expected[key]
		if !ok {
			return fmt.Errorf("live file or directory missing from full snapshot: %s", key)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if uint32(info.Mode().Perm()) != item.Mode {
			return fmt.Errorf("live permissions differ from full snapshot: %s", key)
		}
		switch item.Type {
		case "dir":
			if !info.IsDir() {
				return fmt.Errorf("live directory replaced: %s", key)
			}
		case "file":
			if !info.Mode().IsRegular() || info.Size() != item.Size {
				return fmt.Errorf("live file type or size differs from full snapshot: %s", key)
			}
			hash, size, err := sqliteReindexHashFile(path)
			if err != nil || hash != item.SHA256 || size != item.Size {
				return fmt.Errorf("live SHA-256 differs from full snapshot: %s", key)
			}
		default:
			return errors.New("unknown full snapshot entry")
		}
		delete(expected, key)
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(expected) != 0 {
		return "", errors.New("live files listed in full snapshot are missing")
	}
	final, err := backupReadPrivateFile(physical, fullSnapshotManifestName)
	if err != nil || metadataDigest(final) != digest {
		return "", errors.New("full recovery manifest changed during verification")
	}
	return digest, nil
}

// VerifySQLiteCutoverFullPreflight extends the metadata-only rehearsal gate
// with an exact offline data-tree comparison. It does not change backend.
func VerifySQLiteCutoverFullPreflight(root, backup, snapshot string) error {
	if root == "" || backup == "" || snapshot == "" {
		return errors.New("live data, metadata backup and full snapshot are required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(abs)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := verifySQLiteCutoverPreflightLocked(abs, backup); err != nil {
		return err
	}
	_, err = verifyFullCutoverSnapshotLocked(abs, snapshot, false)
	return err
}
