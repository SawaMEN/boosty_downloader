package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// SQL primary GC deliberately retains every digest present in media_blobs,
// including rows with no current hardlinks or with NULL relative_path.
// A row may describe an independently copied media file. The only removable
// CAS object is an unindexed, canonical, verified regular inode with nlink=1.
// This operation does not alter SQL: a crash after unlink is idempotent.
func (s *Storage) sqliteCollectUnusedBlobsLocked(digests []string) error {
	if s.primary == nil {return errors.New("SQL CAS GC requires primary adapter")}
	if err:=s.sqlitePrimaryGuardMutationLocked();err!=nil{return err}
	if err:=sqliteHistoryDeleteGuard(s.root,false);err!=nil{return err}

	// All callers hold Storage.mu; deduplication serializes physical CAS
	// operations under blobMu, never acquiring Storage.mu while holding it.
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	ctx:=context.Background()
	tx,err:=s.primary.db.BeginTx(ctx,&sql.TxOptions{ReadOnly:true})
	if err!=nil{return err}
	defer tx.Rollback()
	protected,err:=sqliteCASProtected(ctx,tx)
	if err!=nil{return err}
	candidates,err:=sqliteCASCandidates(s.root,digests)
	if err!=nil{return err}
	type removal struct{
		path,digest string
		info os.FileInfo
	}
	plan:=make([]removal,0,len(candidates))
	for _,digest:=range candidates {
		if protected[digest]{continue}
		path,err:=sqliteAuditSafePath(s.root,[]string{"blobs","sha256",digest[:2],digest})
		if os.IsNotExist(err){continue}
		if err!=nil{return err}
		info,err:=os.Lstat(path)
		if os.IsNotExist(err){continue}
		if err!=nil{return err}
		if !info.Mode().IsRegular(){return errors.New("unsafe CAS GC object")}
		stat,ok:=info.Sys().(*syscall.Stat_t)
		if !ok||stat.Nlink!=1{continue}
		got,size,err:=sqliteReindexHashFile(path)
		if err!=nil{return err}
		if got!=digest||size!=info.Size(){return fmt.Errorf("CAS GC refused corrupted object %s",digest)}
		plan=append(plan,removal{path,digest,info})
	}
	// Validate the complete plan before the first unlink. No SQL row can
	// change under the metadata lock. Recheck the pinned inode and link
	// count immediately before each removal; never follow a symlink.
	for _,item:=range plan{
		path,err:=sqliteAuditSafePath(s.root,[]string{"blobs","sha256",item.digest[:2],item.digest})
		if err!=nil{return err}
		now,err:=os.Lstat(path)
		if err!=nil{return err}
		if path!=item.path||!now.Mode().IsRegular()||!os.SameFile(now,item.info)||
			now.Size()!=item.info.Size()||!now.ModTime().Equal(item.info.ModTime()){
			return errors.New("CAS GC object changed before unlink")
		}
		st,ok:=now.Sys().(*syscall.Stat_t)
		if !ok||st.Nlink!=1{continue}
		if err:=os.Remove(path);err!=nil{return err}
		if err:=syncMetadataDirectory(filepath.Dir(path));err!=nil{return err}
	}
	return tx.Commit()
}

func sqliteCASProtected(ctx context.Context,tx *sql.Tx)(map[string]bool,error){
	rows,err:=tx.QueryContext(ctx,"SELECT sha256,byte_length,relative_path FROM media_blobs")
	if err!=nil{return nil,err}
	defer rows.Close()
	protected:=map[string]bool{}
	for rows.Next(){
		var digest string
		var size int64
		var relative sql.NullString
		if err:=rows.Scan(&digest,&size,&relative);err!=nil{return nil,err}
		if !sqliteReindexDigestValid(digest)||size<0{return nil,errors.New("invalid SQL CAS identity")}
		if relative.Valid&&relative.String!=filepath.ToSlash(filepath.Join("blobs","sha256",digest[:2],digest)){
			return nil,errors.New("noncanonical SQL CAS path")
		}
		protected[digest]=true
	}
	if err:=rows.Err();err!=nil{return nil,err}
	if err:=rows.Close();err!=nil{return nil,err}
	// Detect dangling references even if a noncooperating writer previously
	// disabled foreign keys. Do not interpret a missing parent as orphaned.
	var dangling int
	if err:=tx.QueryRowContext(ctx,`SELECT COUNT(*) FROM revision_assets a
		LEFT JOIN media_blobs b ON a.sha256=b.sha256 WHERE b.sha256 IS NULL`).Scan(&dangling);err!=nil{return nil,err}
	if dangling!=0{return nil,errors.New("SQL CAS references a missing blob row")}
	return protected,nil
}

// nil selects a complete sweep. A non-nil slice selects only named digests.
// The full sweep refuses unexpected directory entries rather than guessing.
func sqliteCASCandidates(root string,digests []string)([]string,error){
	base:=filepath.Join(root,"blobs","sha256")
	info,err:=purgeSafePath(root,base)
	if err!=nil{return nil,err}
	if info==nil{return nil,nil}
	seen:=map[string]bool{}
	if digests!=nil{
		for _,digest:=range digests{
			if sqliteReindexDigestValid(digest){seen[digest]=true}
		}
	}else{
		shards,err:=os.ReadDir(base)
		if err!=nil{return nil,err}
		for _,shard:=range shards{
			name:=shard.Name()
			if !shard.IsDir()||len(name)!=2||!sqliteReindexDigestValid(name+strings.Repeat("0",62)){
				return nil,errors.New("unexpected CAS shard")
			}
			path:=filepath.Join(base,name)
			if _,err:=purgeSafePath(root,path);err!=nil{return nil,err}
			files,err:=os.ReadDir(path)
			if err!=nil{return nil,err}
			for _,entry:=range files{
				digest:=entry.Name()
				if !entry.Type().IsRegular()||!sqliteReindexDigestValid(digest)||digest[:2]!=name{return nil,errors.New("unexpected CAS file")}
				seen[digest]=true
			}
		}
	}
	out:=make([]string,0,len(seen))
	for digest:=range seen{out=append(out,digest)}
	sort.Strings(out)
	return out,nil
}
