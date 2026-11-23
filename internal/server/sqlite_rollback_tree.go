package server

import(
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The full SHA-256 name avoids accidental reuse of a different database.
// No ordinary JSON/shadow startup opens this retired database. It remains
// available for offline inspection and is not a new authoritative store.
func sqliteRollbackRetiredName(j sqliteRollbackJournal)string{
	return "metadata.retired-"+j.SQLHash+".db"
}

func sqliteRollbackRequireClosedDB(root string)error{
	for _,name:=range []string{"metadata.db-wal","metadata.db-shm","metadata.db-journal"}{
		info,err:=os.Lstat(filepath.Join(root,name))
		if os.IsNotExist(err){continue}
		if err!=nil{return err}
		if !info.Mode().IsRegular()||info.Mode().Perm()&0077!=0||
			name=="metadata.db-journal"||(name=="metadata.db-wal"&&info.Size()!=0){
			return fmt.Errorf("rollback refuses unsafe or uncheckpointed SQLite sidecar: %s",name)
		}
		// A stopped SQLite can leave an empty WAL and a private SHM
		// cache. Neither contains committed pages when the WAL is empty.
	}
	return nil
}

func sqliteRollbackSQLName(root string,j sqliteRollbackJournal)(string,error){
	if j.Version==1{return "metadata.db",nil}
	if err:=sqliteRollbackRequireClosedDB(root);err!=nil{return "",err}
	retired:=sqliteRollbackRetiredName(j)
	activeInfo,activeErr:=os.Lstat(filepath.Join(root,"metadata.db"))
	retiredInfo,retiredErr:=os.Lstat(filepath.Join(root,retired))
	for _,err:=range []error{activeErr,retiredErr}{if err!=nil&&!os.IsNotExist(err){return "",err}}
	if (activeInfo==nil)==(retiredInfo==nil){return "",errors.New("rollback requires exactly one pinned SQL database")}
	if activeInfo!=nil{return "metadata.db",nil}
	for _,name:=range []string{"metadata.db-wal","metadata.db-shm"}{
		if _,err:=os.Lstat(filepath.Join(root,name));err==nil{return "",errors.New("retired SQL still has active sidecars")}else if !os.IsNotExist(err){return "",err}
	}
	return retired,nil
}

// Revalidate ALL live entries, including accounts, comments, tokens and
// non-indexed data. Only the journal and its three exact metadata images
// may differ from the pinned snapshot; v2 also permits the SQL rename.
func sqliteRollbackVerifyLiveTree(root,snapshot string,j sqliteRollbackJournal,sqlName string)error{
	raw,err:=backupReadPrivateFile(snapshot,fullSnapshotManifestName)
	if err!=nil||metadataDigest(raw)!=j.SnapshotHash{return errors.New("rollback full manifest changed")}
	var manifest fullDataSnapshotManifest
	if err:=json.Unmarshal(raw,&manifest);err!=nil{return err}
	info,err:=os.Lstat(root)
	if err!=nil||!info.IsDir()||uint32(info.Mode().Perm())!=manifest.RootMode{return errors.New("rollback data root mode changed")}
	expected:=make(map[string]fullDataSnapshotEntry,len(manifest.Entries))
	for _,entry:=range manifest.Entries{
		if entry.Path==sqliteRollbackJournalName{return errors.New("rollback snapshot contains pending journal")}
		if entry.Path=="metadata.db"{entry.Path=sqlName}
		if _,ok:=expected[entry.Path];ok{return errors.New("rollback SQL retirement destination conflicts with snapshot")}
		expected[entry.Path]=entry
	}
	images:=map[string]metadataImage{"jobs.json":j.Jobs,"imports.json":j.Imports,sqliteBackendSelectionName:j.Selector}
	err=filepath.WalkDir(root,func(path string,entry os.DirEntry,walkErr error)error{
		if walkErr!=nil{return walkErr}
		if path==root{return nil}
		rel,err:=filepath.Rel(root,path)
		if err!=nil{return err}
		name:=filepath.ToSlash(rel)
		info,err:=os.Lstat(path)
		if err!=nil{return err}
		if name==sqliteRollbackJournalName{
			if !info.Mode().IsRegular()||info.Mode().Perm()!=0600{return errors.New("unsafe rollback journal in live tree")}
			return nil
		}
		item,ok:=expected[name]
		if !ok{return fmt.Errorf("rollback live entry not in snapshot: %s",name)}
		delete(expected,name)
		if img,ok:=images[name];ok{
			if !info.Mode().IsRegular()||info.Mode().Perm()&0077!=0{return errors.New("unsafe rollback metadata mode")}
			if err:=validateMetadataRecoveryTarget(root,name,img);err!=nil{return err}
			return nil
		}
		if uint32(info.Mode().Perm())!=item.Mode{return fmt.Errorf("rollback live permissions changed: %s",name)}
		switch item.Type{
		case "dir":
			if !info.IsDir(){return fmt.Errorf("rollback directory replaced: %s",name)}
		case "file":
			if !info.Mode().IsRegular()||info.Size()!=item.Size{return fmt.Errorf("rollback file size or type changed: %s",name)}
			hash,size,err:=sqliteReindexHashFile(path)
			if err!=nil||hash!=item.SHA256||size!=item.Size{return fmt.Errorf("rollback live SHA-256 changed: %s",name)}
		default:return errors.New("unknown rollback snapshot type")
		}
		return nil
	})
	if err!=nil{return err}
	// Only after the durable JSON selector may recovery remove sidecars
	// from the pinned full snapshot. Present sidecars must match exactly.
	if j.Version==2{
		mode,err:=readSQLiteBackendSelection(root)
		if err!=nil{return err}
		if mode=="json"{
			delete(expected,"metadata.db-wal")
			delete(expected,"metadata.db-shm")
		}
	}
	if len(expected)!=0{return errors.New("rollback live entries disappeared")}
	return nil
}
