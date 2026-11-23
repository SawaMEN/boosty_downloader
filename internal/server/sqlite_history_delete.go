package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const sqliteHistoryDeleteJournalName = ".sqlite-history-delete.json"

// Deletion is roll-forward only after the pinned durable intent exists.
// The directory moves to an inaccessible name before SQL loses its reference.
// Physical cleanup happens only after the SQL commit, including after SIGKILL.
func sqliteHistoryDeletePaths(root string,j sqliteHistoryJournal)(string,string,error){
	_,live,err:=sqliteHistoryPaths(root,j)
	if err!=nil{return "","",err}
	return live,filepath.Join(filepath.Dir(live),".incomplete-delete-"+j.Revision),nil
}

func sqliteHistoryDeleteGuard(root string, recovering bool)error{
	for _,name:=range []string{sqliteHistoryDeleteJournalName,sqliteHistoryJournalName,sqlitePrimaryTransactionName,
		metadataJournalName,sqliteRollbackJournalName,sqliteCutoverIntentName,purgeJournalName,
		revisionRestoreJournalName,archiveUpdateJournalName,videoPublicationJournalName}{
		if recovering&&name==sqliteHistoryDeleteJournalName{continue}
		if _,err:=os.Lstat(filepath.Join(root,name));err==nil{return fmt.Errorf("history deletion blocked by %s",name)}else if !os.IsNotExist(err){return err}
	}
	return nil
}

// Validate both SQL metadata/ACL and immutable indexed historical bytes.
// Do not silently repair an unrelated or changed revision while deleting it.
func sqliteHistoryDeleteIndex(ctx context.Context,tx *sql.Tx,root string,j sqliteHistoryJournal,source string)(bool,error){
	jobsRaw,err:=videoPublicationReadFile(root,"jobs.json",64<<20)
	if err!=nil||metadataDigest(jobsRaw)!=j.JobsHash{return false,errors.New("history deletion jobs generation changed")}
	importsRaw,err:=videoPublicationReadFile(root,"imports.json",64<<20)
	if err!=nil||metadataDigest(importsRaw)!=j.ImportsHash{return false,errors.New("history deletion imports generation changed")}
	jobs,imports,err:=sqlitePrimaryDecodeImages(jobsRaw,importsRaw)
	if err!=nil{return false,err}
	job,ok:=jobs[j.JobID]
	if !ok||job.OwnerID!=j.Owner||job.Author!=j.Author||job.PostID!=j.Post||job.DeletedAt!=nil{return false,errors.New("history deletion owner or job changed")}
	actualJobs,err:=sqliteShadowSQLJobs(ctx,tx)
	if err!=nil||!sqliteShadowEqual(jobs,actualJobs){return false,errors.New("history deletion SQL jobs or ACL diverged")}
	actualImports,err:=sqliteShadowSQLImports(ctx,tx)
	if err!=nil||!sqliteShadowEqual(imports,actualImports){return false,errors.New("history deletion SQL imports diverged")}
	old,err:=sqliteReindexOldRevisions(ctx,tx,jobs)
	if err!=nil{return false,err}
	indexed,ok:=old[j.Revision]
	if !ok{return false,nil}
	if indexed.archiveID!=job.ID||source==""{return false,errors.New("history deletion index owner or directory differs")}
	if digest,err:=sqliteHistoryFilesHash(source);err!=nil||digest!=j.FilesHash{return false,errors.New("history deletion snapshot bytes changed")}
	rev,err:=sqliteReindexReadRevision(root,job,j.Revision,[]string{"revisions",j.Owner,j.Author,j.Post,filepath.Base(source)},false)
	if err!=nil{return false,err}
	rev.path=filepath.ToSlash(filepath.Join("revisions",j.Owner,j.Author,j.Post,j.Revision))
	assets,err:=sqliteReindexOldAssets(ctx,tx)
	if err!=nil{return false,err}
	equal,err:=sqliteReindexUnchanged(ctx,tx,indexed,rev,assets[j.Revision])
	if err!=nil{return false,err}
	if !equal{return false,errors.New("history deletion does not match immutable SQL index")}
	return true,nil
}

func(s *Storage)deleteSQLHistory(job Job,id string)error{
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.primary==nil{return errors.New("SQL history deletion requires primary adapter")}
	if err:=s.sqlitePrimaryGuardMutationLocked();err!=nil{return err}
	if err:=sqliteHistoryDeleteGuard(s.root,false);err!=nil{return err}
	current:=s.jobs[job.ID]
	if current==nil||current.OwnerID!=job.OwnerID||current.Author!=job.Author||current.PostID!=job.PostID||current.DeletedAt!=nil||
		!validRevisionID(id)||!safeArchivePart(job.OwnerID)||!safeArchivePart(job.Author)||!safeArchivePart(job.PostID){
		return errors.New("unsafe history deletion identity")
	}
	live:=filepath.Join(s.revisionsDir(job),id)
	dev,inode,err:=revisionRestoreIdentity(s.root,live)
	if err!=nil{return err}
	manifest,_,err:=readArchiveManifestNoFollow(live)
	if err!=nil{return err}
	digest,err:=sqliteHistoryFilesHash(live)
	if err!=nil{return err}
	jobs,err:=videoPublicationReadFile(s.root,"jobs.json",64<<20)
	if err!=nil{return err}
	imports,err:=videoPublicationReadFile(s.root,"imports.json",64<<20)
	if err!=nil{return err}
	j:=sqliteHistoryJournal{Version:1,JobID:job.ID,Owner:job.OwnerID,Author:job.Author,Post:job.PostID,Revision:id,
		JobsHash:metadataDigest(jobs),ImportsHash:metadataDigest(imports),ManifestHash:metadataDigest(manifest),FilesHash:digest,Device:dev,Inode:inode}
	expected,tomb,err:=sqliteHistoryDeletePaths(s.root,j)
	if err!=nil||expected!=live{return errors.New("invalid SQL history deletion paths")}
	if info,err:=purgeSafePath(s.root,tomb);err!=nil||info!=nil{return errors.New("history deletion staging destination occupied")}
	ctx:=context.Background()
	tx,err:=s.primary.db.BeginTx(ctx,&sql.TxOptions{ReadOnly:true})
	if err!=nil{return err}
	indexed,checkErr:=sqliteHistoryDeleteIndex(ctx,tx,s.root,j,live)
	rollbackErr:=tx.Rollback()
	if checkErr!=nil{return checkErr}
	if rollbackErr!=nil{return rollbackErr}
	if !indexed{return errors.New("cannot delete unindexed historical revision")}
	raw,err:=json.Marshal(j)
	if err!=nil{return err}
	if err:=writeSyncedMetadata(s.root,sqliteHistoryDeleteJournalName,raw);err!=nil{
		if _,readErr:=os.Lstat(filepath.Join(s.root,sqliteHistoryDeleteJournalName));!os.IsNotExist(readErr){return s.sqlitePrimaryQuarantineLocked(err)}
		return err
	}
	if s.sqliteHistoryDeleteCheckpoint!=nil{
		if err:=s.sqliteHistoryDeleteCheckpoint("prepared");err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	}
	if err:=recoverSQLHistoryDeleteLocked(s.root,s.sqliteHistoryDeleteCheckpoint);err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	return nil
}

func recoverSQLHistoryDeleteLocked(root string,checkpoint func(string)error)error{
	info,err:=os.Lstat(filepath.Join(root,sqliteHistoryDeleteJournalName))
	if os.IsNotExist(err){return nil}
	if err!=nil{return err}
	if !info.Mode().IsRegular()||info.Mode().Perm()&0077!=0||info.Size()<1||info.Size()>16<<10{return errors.New("unsafe SQL history deletion journal")}
	raw,err:=videoPublicationReadFile(root,sqliteHistoryDeleteJournalName,16<<10)
	if err!=nil{return err}
	var j sqliteHistoryJournal
	decoder:=json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err:=decoder.Decode(&j);err!=nil{return err}
	var trailing any
	if err:=decoder.Decode(&trailing);err!=io.EOF{return errors.New("trailing history deletion journal data")}
	live,tomb,err:=sqliteHistoryDeletePaths(root,j)
	if err!=nil{return err}
	if err:=sqliteHistoryDeleteGuard(root,true);err!=nil{return err}
	liveExists,err:=revisionRestoreDirectory(root,live,j.ManifestHash,j.Author,j.Post,j.Device,j.Inode,true)
	if err!=nil{return err}
	// A tombstone can be partly removed only after its SQL row is gone.
	tombExists,err:=revisionRestoreDirectory(root,tomb,j.ManifestHash,j.Author,j.Post,j.Device,j.Inode,false)
	if err!=nil{return err}
	if liveExists&&tombExists{return errors.New("history deletion has two candidate directories")}
	source:=""
	if liveExists{source=live}else if tombExists{source=tomb}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{return err}
	defer meta.close()
	ctx:=context.Background()
	tx,err:=meta.db.BeginTx(ctx,nil)
	if err!=nil{return err}
	defer tx.Rollback()
	indexed,err:=sqliteHistoryDeleteIndex(ctx,tx,root,j,source)
	if err!=nil{return err}
	if indexed {
		if liveExists{
			if err:=durableRevisionRestoreRename(root,live,tomb);err!=nil{return err}
			tombExists=true
		}
		if !tombExists{return errors.New("indexed history directory missing")}
		if err:=syncMetadataDirectory(filepath.Dir(tomb));err!=nil{return err}
		if checkpoint!=nil{if err:=checkpoint("moved");err!=nil{return err}}
		result,err:=tx.ExecContext(ctx,"DELETE FROM archive_revisions WHERE revision_id=? AND archive_id=? AND is_current=0",j.Revision,j.JobID)
		if err!=nil{return err}
		n,err:=result.RowsAffected()
		if err!=nil||n!=1{return errors.New("history SQL deletion did not affect exactly one revision")}
		if _,err:=tx.ExecContext(ctx,`DELETE FROM media_blobs WHERE NOT EXISTS
			(SELECT 1 FROM revision_assets a WHERE a.sha256=media_blobs.sha256)`);err!=nil{return err}
	}else if liveExists{
		return errors.New("history index vanished before directory was quarantined")
	}
	if err:=tx.Commit();err!=nil{return err}
	if checkpoint!=nil{if err:=checkpoint("indexed");err!=nil{return err}}
	if tombExists{
		if _,err:=revisionRestoreDirectory(root,tomb,j.ManifestHash,j.Author,j.Post,j.Device,j.Inode,false);err!=nil{return err}
		if err:=durableRevisionRestoreRemove(root,tomb);err!=nil{return err}
	}
	if err:=syncMetadataDirectory(filepath.Dir(tomb));err!=nil{return err}
	if checkpoint!=nil{if err:=checkpoint("removed");err!=nil{return err}}
	final,err:=videoPublicationReadFile(root,sqliteHistoryDeleteJournalName,16<<10)
	if err!=nil||!bytes.Equal(raw,final){return errors.New("history deletion journal changed")}
	return removeSyncedMetadata(root,sqliteHistoryDeleteJournalName)
}
