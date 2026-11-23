package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"golang.org/x/sys/unix"
)

const sqliteRollbackJournalName = ".sqlite-rollback-transaction.json"
const sqliteRollbackJournalMax = 128 << 20

// Offline-only roll-forward journal. SQL stays authoritative until the
// verified JSON pair is published and the durable backend selector is changed.
// A crash never restores an old SQL database over newer grants or jobs.
type sqliteRollbackJournal struct {
	Version      int           `json:"version"`
	DataDir      string        `json:"dataDir"`
	SnapshotDir  string        `json:"snapshotDir"`
	SnapshotHash string        `json:"snapshotManifestSha256"`
	SQLHash      string        `json:"sqliteSha256"`
	Jobs         metadataImage `json:"jobs"`
	Imports      metadataImage `json:"imports"`
	Selector     metadataImage `json:"selector"`
}

func sqliteRollbackReadJournal(root string) (sqliteRollbackJournal, bool, error) {
	info, err := os.Lstat(filepath.Join(root, sqliteRollbackJournalName))
	if os.IsNotExist(err) {
		return sqliteRollbackJournal{}, false, nil
	}
	if err != nil {
		return sqliteRollbackJournal{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > sqliteRollbackJournalMax {
		return sqliteRollbackJournal{}, true, errors.New("unsafe SQLite rollback journal")
	}
	raw, err := videoPublicationReadFile(root, sqliteRollbackJournalName, sqliteRollbackJournalMax)
	if err != nil {
		return sqliteRollbackJournal{}, true, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var j sqliteRollbackJournal
	if err := decoder.Decode(&j); err != nil {
		return j, true, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return j, true, errors.New("SQLite rollback journal has trailing data")
	}
	if (j.Version != 1 && j.Version != 2) || !filepath.IsAbs(j.DataDir) || !filepath.IsAbs(j.SnapshotDir) ||
		!sqliteReindexDigestValid(j.SnapshotHash) || !sqliteReindexDigestValid(j.SQLHash) {
		return j, true, errors.New("invalid SQLite rollback journal identity")
	}
	for _, img := range []metadataImage{j.Jobs, j.Imports, j.Selector} {
		if !img.Existed {
			return j, true, errors.New("SQLite rollback journal lacks a before image")
		}
		if err := validateMetadataImage(img); err != nil {
			return j, true, err
		}
	}
	var oldSelector, newSelector sqliteBackendSelection
	if json.Unmarshal(j.Selector.Before, &oldSelector) != nil ||
		json.Unmarshal(j.Selector.After, &newSelector) != nil ||
		oldSelector.Version != 1 || oldSelector.Active != "sqlite" ||
		newSelector.Version != 1 || newSelector.Active != "json" {
		return j, true, errors.New("SQLite rollback journal selector transition is invalid")
	}
	return j, true, nil
}

func sqliteRollbackCapture(root, name string, after []byte, limit int64) (metadataImage, error) {
	before, err := videoPublicationReadFile(root, name, limit)
	if err != nil {
		return metadataImage{}, err
	}
	img := metadataImage{Existed: true, Before: before, After: after,
		OldHash: metadataDigest(before), NewHash: metadataDigest(after)}
	return img, validateMetadataImage(img)
}

func sqliteRollbackReadSQL(root string) ([]byte, []byte, error) {
	db, err := openSQLiteReadOnly(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	jobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	imports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	jobsRaw, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	importsRaw, err := json.MarshalIndent(imports, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return jobsRaw, importsRaw, nil
}

// RollbackSQLiteToJSON explicitly recovers a stopped SQLite-selected data
// tree. It does not activate SQLite, change SQL rows, restore media or use
// older JSON as an authority. A fresh full snapshot is mandatory.
func RollbackSQLiteToJSON(root, snapshot string) error {
	return rollbackSQLiteToJSON(root, snapshot, nil)
}

func rollbackSQLiteToJSON(root, snapshot string, checkpoint func(string) error) error {
	if root == "" || snapshot == "" {
		return errors.New("data directory and verified full snapshot are required")
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
	if mode, err := readSQLiteBackendSelection(abs); err != nil || mode != "sqlite" {
		return errors.New("rollback requires an explicit SQLite-active backend selector")
	}
	if err := sqliteRollbackOfflineCheck(abs, false); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	snapshot, err = fullCutoverSnapshotPath(abs, snapshot)
	if err != nil {
		return err
	}
	fullHash, err := verifyFullCutoverSnapshotLocked(abs, snapshot, false)
	if err != nil {
		return err
	}
	sqlHash, _, err := backupFileDigest(abs, "metadata.db")
	if err != nil {
		return err
	}
	if err := verifySQLiteSnapshotInventory(abs); err != nil {
		return err
	}
	jobs, imports, err := sqliteRollbackReadSQL(abs)
	if err != nil {
		return err
	}
	if _, _, err := sqlitePrimaryDecodeImages(jobs, imports); err != nil {
		return err
	}
	j := sqliteRollbackJournal{Version: 2, DataDir: physical, SnapshotDir: snapshot,
		SnapshotHash: fullHash, SQLHash: sqlHash}
	if j.Jobs, err = sqliteRollbackCapture(abs, "jobs.json", jobs, 64<<20); err != nil {
		return err
	}
	if j.Imports, err = sqliteRollbackCapture(abs, "imports.json", imports, 64<<20); err != nil {
		return err
	}
	selector, err := json.Marshal(sqliteBackendSelection{Version: 1, Active: "json"})
	if err != nil {
		return err
	}
	if j.Selector, err = sqliteRollbackCapture(abs, sqliteBackendSelectionName, selector, 256); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil || len(raw) > sqliteRollbackJournalMax {
		return errors.New("SQLite rollback journal exceeds its size limit")
	}
	// Recheck the pinned backup and SQL before authorizing any replay.
	if err:=sqliteRollbackRequireClosedDB(abs);err!=nil{return err}
	if _,err:=os.Lstat(filepath.Join(abs,sqliteRollbackRetiredName(j)));err==nil{
		return errors.New("retired SQL destination already exists")
	}else if !os.IsNotExist(err){return err}
	if err := sqliteRollbackValidate(abs, j); err != nil {
		return err
	}
	if err := writeSyncedMetadata(abs, sqliteRollbackJournalName, raw); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := checkpoint("prepared"); err != nil {
			return err
		}
	}
	return recoverSQLiteRollbackLocked(abs, checkpoint)
}

// Guard all other offline writers. Recovery can ignore only its own journal,
// never a concurrent file transaction, cutover rehearsal or pending WAL.
func sqliteRollbackOfflineCheck(root string, recovering bool) error {
	if err := sqliteReindexNoWALExceptRollback(root, recovering); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); err == nil {
		return errors.New("SQLite rollback blocked by pending cutover rehearsal")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sqliteRollbackValidate(root string, j sqliteRollbackJournal) error {
	physical, err := filepath.EvalSymlinks(root)
	if err != nil || physical != j.DataDir {
		return errors.New("SQLite rollback belongs to another data directory")
	}
	snapshot, err := fullCutoverSnapshotPath(root, j.SnapshotDir)
	if err != nil || snapshot != j.SnapshotDir {
		return errors.New("SQLite rollback snapshot moved")
	}
	hash, _, err := backupFileDigest(snapshot, fullSnapshotManifestName)
	if err != nil || hash != j.SnapshotHash {
		return errors.New("SQLite rollback snapshot manifest changed")
	}
	if err := VerifyFullDataSnapshot(snapshot); err != nil {
		return err
	}
	// Each old image must be the exact byte sequence preserved in the
	// mandatory full snapshot. Verify the SQL file against both copies.
	for _, target := range []struct {
		name string
		img  metadataImage
	}{
		{"jobs.json", j.Jobs}, {"imports.json", j.Imports}, {sqliteBackendSelectionName, j.Selector},
	} {
		old, _, err := backupFileDigest(filepath.Join(snapshot, fullSnapshotDataDirName), target.name)
		if err != nil || old != target.img.OldHash {
			return fmt.Errorf("rollback before image not backed up: %s", target.name)
		}
		if err := validateMetadataRecoveryTarget(root, target.name, target.img); err != nil {
			return err
		}
	}
	sqlName,err:=sqliteRollbackSQLName(root,j)
	if err!=nil{return err}
	for _,source:=range []struct{dir,name string}{{root,sqlName},{filepath.Join(snapshot,fullSnapshotDataDirName),"metadata.db"}}{
		actual,_,err:=backupFileDigest(source.dir,source.name)
		if err!=nil||actual!=j.SQLHash{return errors.New("SQLite changed after rollback recovery point")}
	}
	if err:=sqliteRollbackVerifyLiveTree(root,snapshot,j,sqlName);err!=nil{return err}
	mode, err := readSQLiteBackendSelection(root)
	if err != nil {
		return err
	}
	if sqlName!="metadata.db"&&mode!="json"{return errors.New("SQL retired before JSON selection")}
	if mode == "json" {
		for _, target := range []struct {
			name string
			img  metadataImage
		}{
			{"jobs.json", j.Jobs}, {"imports.json", j.Imports},
		} {
			hash, _, err := backupFileDigest(root, target.name)
			if err != nil || hash != target.img.NewHash {
				return errors.New("JSON selector was advanced before both metadata files")
			}
		}
	}
	if err := verifySQLiteSnapshotInventoryFile(root,sqlName); err != nil {
		return err
	}
	jobs, imports, err := sqlitePrimaryDecodeImages(j.Jobs.After, j.Imports.After)
	if err != nil {
		return err
	}
	db, err := openSQLiteReadOnly(filepath.Join(root, sqlName))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := (&sqliteMetadata{db: db}).verifyJSONParity(context.Background(), jobs, imports); err != nil {
		return err
	}
	finalHash, _, err := backupFileDigest(root, sqlName)
	if err != nil || finalHash != j.SQLHash {
		return errors.New("SQL changed during rollback verification")
	}
	return nil
}

// RecoverSQLiteRollback resumes a durable offline rollback; never creates
// an intent or overwrites unrelated metadata.
func RecoverSQLiteRollback(root string) error {
	if root==""{return errors.New("data directory is required")}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(abs)
	if err != nil {
		return err
	}
	defer lock.Close()
	return recoverSQLiteRollbackLocked(abs, nil)
}

func recoverSQLiteRollbackLocked(root string, checkpoint func(string) error) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	j, found, err := sqliteRollbackReadJournal(root)
	if err != nil || !found {
		return err
	}
	if err := sqliteRollbackOfflineCheck(root, true); err != nil {
		return err
	}
	if err := sqliteRollbackValidate(root, j); err != nil {
		return err
	}
	for _, target := range []struct {
		name, phase string
		img         metadataImage
	}{
		{"jobs.json", "jobs", j.Jobs}, {"imports.json", "imports", j.Imports},
	} {
		if err := replayMetadataImage(root, target.name, target.img, true); err != nil {
			return err
		}
		if checkpoint != nil {
			if err := checkpoint(target.phase); err != nil {
				return err
			}
		}
	}
	sqlName,err:=sqliteRollbackSQLName(root,j)
	if err!=nil{return err}
	// Compare the replayed images directly with their pinned hashes and
	// use the immutable audit. Opening a regular read-only WAL connection
	// here can create new SHM/WAL files outside the recovery snapshot.
	for _,target:=range []struct{name string;img metadataImage}{{"jobs.json",j.Jobs},{"imports.json",j.Imports}}{
		hash,_,err:=backupFileDigest(root,target.name)
		if err!=nil||hash!=target.img.NewHash{return errors.New("rollback mirror replay differs from SQL")}
	}
	if err:=sqliteRollbackValidate(root,j);err!=nil{return err}
	if err := replayMetadataImage(root, sqliteBackendSelectionName, j.Selector, true); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := checkpoint("selector"); err != nil {
			return err
		}
	}
	if j.Version==2 {
		if err:=sqliteRollbackRequireClosedDB(root);err!=nil{return err}
		if sqlName=="metadata.db"{
			// The previous full-tree audit pinned both optional sidecars.
			// Remove only checkpointed, private files before renaming SQL.
			for _,name:=range []string{"metadata.db-wal","metadata.db-shm"}{
				if err:=removeSyncedMetadata(root,name);err!=nil{return err}
			}
			if checkpoint!=nil{if err:=checkpoint("sidecars");err!=nil{return err}}
			if err:=unix.Renameat2(unix.AT_FDCWD,filepath.Join(root,sqlName),unix.AT_FDCWD,filepath.Join(root,sqliteRollbackRetiredName(j)),unix.RENAME_NOREPLACE);err!=nil{return err}
		}
		// Also fsync on retry if a prior rename succeeded before fsync.
		if err:=syncMetadataDirectory(root);err!=nil{return err}
		if checkpoint!=nil{if err:=checkpoint("retired");err!=nil{return err}}
	}
	if err:=sqliteRollbackValidate(root,j);err!=nil{return err}
	current,found,err:=sqliteRollbackReadJournal(root)
	if err!=nil||!found||!sqliteShadowEqual(current,j){return errors.New("rollback intent changed during replay")}
	return removeSyncedMetadata(root, sqliteRollbackJournalName)
}
