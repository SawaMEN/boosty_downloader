package server

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqlGCFixture(t *testing.T)(*Storage,Job,string){
	t.Helper()
	root,id,_,media:=sqliteSnapshotAuditFixture(t)
	s,err:=newStorage(root,testKey(t))
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	t.Cleanup(func(){_ = meta.close()})
	s.primary=meta
	job,ok:=s.get(id)
	if !ok{t.Fatal("missing job")}
	return s,job,media
}

func sqlGCCreateObject(t *testing.T,root string,data []byte)(string,string){
	t.Helper()
	digest:=fmt.Sprintf("%x",sha256.Sum256(data))
	path:=filepath.Join(root,"blobs","sha256",digest[:2],digest)
	if err:=os.MkdirAll(filepath.Dir(path),0700);err!=nil{t.Fatal(err)}
	if err:=os.WriteFile(path,data,0600);err!=nil{t.Fatal(err)}
	return digest,path
}

func TestSQLBlobGCPreservesIndexedCopiesAndHardlinks(t *testing.T){
	s,job,media:=sqlGCFixture(t)
	raw,err:=os.ReadFile(filepath.Join(s.archiveDir(job.OwnerID,job.Author,job.PostID),"media",media))
	if err!=nil{t.Fatal(err)}
	// This CAS object is an independent copy: nlink=1, SQL still needs it.
	digest,indexed:=sqlGCCreateObject(t,s.root,raw)
	if _,err:=s.primary.db.Exec("UPDATE media_blobs SET relative_path=? WHERE sha256=?",filepath.ToSlash(filepath.Join("blobs","sha256",digest[:2],digest)),digest);err!=nil{t.Fatal(err)}
	linkedDigest,linked:=sqlGCCreateObject(t,s.root,[]byte("private in-flight hardlink"))
	if err:=os.Link(linked,filepath.Join(s.root,"inflight"));err!=nil{t.Fatal(err)}
	orphanDigest,orphan:=sqlGCCreateObject(t,s.root,[]byte("unreferenced blob"))
	if err:=s.discardBlobsIfUnreferenced([]string{digest,linkedDigest,orphanDigest,orphanDigest});err!=nil{t.Fatal(err)}
	for _,path:=range []string{indexed,linked}{if _,err:=os.Stat(path);err!=nil{t.Fatal("protected CAS lost",path,err)}}
	if _,err:=os.Stat(orphan);!os.IsNotExist(err){t.Fatal("orphan retained",err)}
	if err:=os.Remove(filepath.Join(s.root,"inflight"));err!=nil{t.Fatal(err)}
	if err:=s.collectUnusedBlobs();err!=nil{t.Fatal(err)}
	if _,err:=os.Stat(linked);!os.IsNotExist(err){t.Fatal("last unreferenced CAS link retained",err)}
	if _,err:=os.Stat(indexed);err!=nil{t.Fatal("SQL referenced object removed",err)}
	if err:=s.collectUnusedBlobs();err!=nil{t.Fatal("idempotent sweep failed",err)}
}

func TestSQLBlobGCRejectsPendingJournalAndSQLDrift(t *testing.T){
	for _,scenario:=range []string{"journal","sql-path","sql-acl","bad-digest"}{
		t.Run(scenario,func(t *testing.T){
			s,job,_:=sqlGCFixture(t)
			digest,orphan:=sqlGCCreateObject(t,s.root,[]byte("preserve until verified"))
			switch scenario{
			case "journal":
				if err:=os.WriteFile(filepath.Join(s.root,sqliteHistoryJournalName),[]byte("{}"),0600);err!=nil{t.Fatal(err)}
			case "sql-path":
				if _,err:=s.primary.db.Exec("UPDATE media_blobs SET relative_path='blobs/sha256/invalid'");err!=nil{t.Fatal(err)}
			case "sql-acl":
				if _,err:=s.primary.db.Exec("UPDATE owner_archives SET title='tampered' WHERE archive_id=?",job.ID);err!=nil{t.Fatal(err)}
			case "bad-digest":
				if err:=os.WriteFile(orphan,[]byte("wrong bytes"),0600);err!=nil{t.Fatal(err)}
			}
			if err:=s.discardBlobIfUnreferenced(digest);err==nil{t.Fatal("unsafe GC accepted")}
			if _,err:=os.Stat(orphan);err!=nil{t.Fatal("unsafe GC deleted object",err)}
		})
	}
}

func TestSQLBlobGCRejectsUnsafeShardBeforeDeleting(t *testing.T){
	s,_,_:=sqlGCFixture(t)
	_,orphan:=sqlGCCreateObject(t,s.root,[]byte("ordinary orphan"))
	outside:=t.TempDir()
	victim:=filepath.Join(outside,strings.Repeat("f",64))
	if err:=os.WriteFile(victim,[]byte("private"),0600);err!=nil{t.Fatal(err)}
	if err:=os.Symlink(outside,filepath.Join(s.root,"blobs","sha256","ff"));err!=nil{t.Fatal(err)}
	if err:=s.collectUnusedBlobs();err==nil{t.Fatal("symlink shard accepted")}
	for _,path:=range []string{orphan,victim}{if _,err:=os.Stat(path);err!=nil{t.Fatal("unexpected deletion",path,err)}}
}

func TestSQLBlobGCAfterPermanentPurge(t *testing.T){
	root,key:=sqlPurgeFixture(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	_,shared:=sqlGCCreateObject(t,root,[]byte("new media bytes"))
	_,orphan:=sqlGCCreateObject(t,root,[]byte("last owner removed"))
	if n,err:=s.purgeOwned("first",strings.Repeat("1",24),"");err!=nil||n!=1{t.Fatal("purge",n,err)}
	if _,err:=os.Stat(shared);err!=nil{t.Fatal("second owner's CAS removed",err)}
	if _,err:=os.Stat(orphan);!os.IsNotExist(err){t.Fatal("orphan was not collected",err)}
}
