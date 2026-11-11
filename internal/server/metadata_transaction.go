package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const metadataJournalName = ".metadata-transaction.json"
const metadataJournalVersion = 1

// metadataTransaction uses a fixed two-file allowlist: no journal-provided
// path is ever used to access the filesystem. JSON remains authoritative.
// A prepared journal rolls back; a committed journal rolls forward.
type metadataTransaction struct {
	Version int           `json:"version"`
	Phase   string        `json:"phase"`
	Jobs    metadataImage `json:"jobs"`
	Imports metadataImage `json:"imports"`
}

type metadataImage struct {
	Existed bool   `json:"existed"`
	Before  []byte `json:"before"`
	After   []byte `json:"after"`
	OldHash string `json:"oldHash"`
	NewHash string `json:"newHash"`
}

func metadataDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func checkedMetadataImage(root, name string, after []byte) (metadataImage, error) {
	if !json.Valid(after) {
		return metadataImage{}, errors.New("invalid new metadata JSON")
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return metadataImage{}, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return metadataImage{}, fmt.Errorf("%s is not a regular metadata file", name)
	}
	img := metadataImage{After: after, NewHash: metadataDigest(after), Existed: err == nil}
	if img.Existed {
		img.Before, err = os.ReadFile(path)
		if err != nil {
			return metadataImage{}, err
		}
		if !json.Valid(img.Before) {
			return metadataImage{}, fmt.Errorf("existing %s has invalid JSON", name)
		}
	}
	img.OldHash = metadataDigest(img.Before)
	return img, nil
}

func validateMetadataImage(img metadataImage) error {
	if !json.Valid(img.After) || img.NewHash != metadataDigest(img.After) ||
		img.OldHash != metadataDigest(img.Before) {
		return errors.New("damaged metadata transaction checksum")
	}
	if img.Existed && !json.Valid(img.Before) {
		return errors.New("damaged metadata transaction source JSON")
	}
	if !img.Existed && len(img.Before) != 0 {
		return errors.New("metadata transaction source does not match existence flag")
	}
	return nil
}

func syncMetadataDirectory(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeSyncedMetadata(root, name string, data []byte) error {
	path := filepath.Join(root, name)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe metadata target: %s", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	return syncMetadataDirectory(root)
}

func removeSyncedMetadata(root, name string) error {
	path := filepath.Join(root, name)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe metadata removal: %s", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncMetadataDirectory(root)
}

func replayMetadataImage(root, name string, img metadataImage, commit bool) error {
	data, exists := img.Before, img.Existed
	if commit {
		data, exists = img.After, true
	}
	if !exists {
		return removeSyncedMetadata(root, name)
	}
	return writeSyncedMetadata(root, name, data)
}

// recoverMetadataPair runs before any Storage load or worker start. A corrupt
// or symlink journal is a hard startup error rather than a guess about ACLs.
func recoverMetadataPair(root string) error {
	path := filepath.Join(root, metadataJournalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsafe metadata transaction journal")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var txn metadataTransaction
	if err := json.Unmarshal(raw, &txn); err != nil {
		return fmt.Errorf("unreadable metadata transaction: %w", err)
	}
	if txn.Version != metadataJournalVersion ||
		(txn.Phase != "prepared" && txn.Phase != "committed") {
		return errors.New("unknown metadata transaction version or phase")
	}
	if err := validateMetadataImage(txn.Jobs); err != nil {
		return err
	}
	if err := validateMetadataImage(txn.Imports); err != nil {
		return err
	}
	commit := txn.Phase == "committed"
	// Idempotent: on a second crash, the same image is replayed again.
	if err := replayMetadataImage(root, "jobs.json", txn.Jobs, commit); err != nil {
		return err
	}
	if err := replayMetadataImage(root, "imports.json", txn.Imports, commit); err != nil {
		return err
	}
	return removeSyncedMetadata(root, metadataJournalName)
}

func encodeMetadataTransaction(txn metadataTransaction) ([]byte, error) {
	return json.Marshal(txn)
}

func rollbackMetadataPair(root string, cause error) error {
	if err := recoverMetadataPair(root); err != nil {
		return fmt.Errorf("%w; metadata recovery failed (restart required): %v", cause, err)
	}
	return cause
}

// writeMetadataPair is a crash-safe two-file JSON commit boundary. It is
// called only with Storage.mu held. The caller rolls back in-memory state on
// an error. SQL remains detached until the separate production cutover.
func writeMetadataPair(root string, jobs, imports []byte) error {
	if err := recoverMetadataPair(root); err != nil {
		return err
	}
	jobImage, err := checkedMetadataImage(root, "jobs.json", jobs)
	if err != nil {
		return err
	}
	importImage, err := checkedMetadataImage(root, "imports.json", imports)
	if err != nil {
		return err
	}
	txn := metadataTransaction{
		Version: metadataJournalVersion,
		Phase:   "prepared",
		Jobs:    jobImage,
		Imports: importImage,
	}
	data, err := encodeMetadataTransaction(txn)
	if err != nil {
		return err
	}
	if err := writeSyncedMetadata(root, metadataJournalName, data); err != nil {
		// The rename may have succeeded even if directory fsync failed;
		// any durable prepared journal must be rolled back.
		return rollbackMetadataPair(root, err)
	}
	if err := writeSyncedMetadata(root, "jobs.json", jobs); err != nil {
		return rollbackMetadataPair(root, err)
	}
	if err := writeSyncedMetadata(root, "imports.json", imports); err != nil {
		return rollbackMetadataPair(root, err)
	}
	txn.Phase = "committed"
	data, err = encodeMetadataTransaction(txn)
	if err != nil {
		return rollbackMetadataPair(root, err)
	}
	if err := writeSyncedMetadata(root, metadataJournalName, data); err != nil {
		// Determine the on-disk phase, not the intent: the committed rename
		// may already have happened. Never roll back a committed journal.
		state, readErr := os.ReadFile(filepath.Join(root, metadataJournalName))
		var actual metadataTransaction
		if readErr == nil && json.Unmarshal(state, &actual) == nil &&
			actual.Phase == "committed" &&
			actual.Version == metadataJournalVersion {
			if recoverErr := recoverMetadataPair(root); recoverErr == nil {
				return nil
			} else {
				return fmt.Errorf("metadata committed but finalization failed: %w", recoverErr)
			}
		}
		return rollbackMetadataPair(root, err)
	}
	// Once committed, an unlink/fsync failure is not a failed transaction:
	// recovery will replay the committed image before the next write/start.
	_ = removeSyncedMetadata(root, metadataJournalName)
	return nil
}

func (s *Storage) saveJobsImportsLocked() error {
	jobs, err := json.MarshalIndent(s.jobs, "", "  ")
	if err != nil {
		return err
	}
	imports, err := json.MarshalIndent(s.imports, "", "  ")
	if err != nil {
		return err
	}
	if err := writeMetadataPair(s.root, jobs, imports); err != nil {
		return err
	}
	s.notifySQLiteShadowLocked()
	return nil
}
