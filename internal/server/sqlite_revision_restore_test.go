package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Make two genuinely different current/history revisions using the actual
// SQL-first archive publisher, with a shared ACL on the current generation.
func sqlRestoreFixture(t *testing.T)(string,string,string){
	t.Helper()
	root,id,hist,_:=sqliteSnapshotAuditFixture(t)
	key:=testKey(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	s.primary=meta
	job,ok:=s.get(id)
	if !ok{t.Fatal("missing SQL job")}
	job.State="running"
	job.Error=""
	job.AutoSync=true
	job.SharedForAll=true
	job.SharedWith=[]string{"second"}
	if err:=s.upsert(job);err!=nil{t.Fatal(err)}
	stage:=archiveUpdateWriteStage(t,s,job)
	committed,err:=s.publishArchiveUpdate(job,stage)
	if err!=nil||!committed{t.Fatal("fixture publication",committed,err)}
	if err:=meta.close();err!=nil{t.Fatal(err)}
	return root,key,hist
}

func TestSQLRevisionRestoreCrashWorker(t *testing.T){
	root:=os.Getenv("BOOSTY_TEST_SQL_RESTORE_ROOT")
	if root==""{return}
	s,err:=newStorage(root,os.Getenv("BOOSTY_TEST_SQL_RESTORE_KEY"))
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	s.restoreCheckpoint=func(phase string)error{
		if phase==os.Getenv("BOOSTY_TEST_SQL_RESTORE_PHASE"){return syscall.Kill(os.Getpid(),syscall.SIGKILL)}
		return nil
	}
	_,err=s.restoreArchiveRevision("first",strings.Repeat("1",24),strings.Repeat("a",24))
	t.Fatalf("restore worker escaped checkpoint: %v",err)
}

func TestSQLRevisionRestoreRecoversSIGKILL(t *testing.T){
	for _,phase:=range []string{"journal","old-moved","new-moved","primary-prepared","primary-sql-committed","primary-json-committed","metadata"}{
		t.Run(phase,func(t *testing.T){
			root,key,hist:=sqlRestoreFixture(t)
			cmd:=exec.Command(os.Args[0],"-test.run=^TestSQLRevisionRestoreCrashWorker$")
			cmd.Env=append(os.Environ(),"BOOSTY_TEST_SQL_RESTORE_ROOT="+root,"BOOSTY_TEST_SQL_RESTORE_KEY="+key,"BOOSTY_TEST_SQL_RESTORE_PHASE="+phase)
			out,err:=cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err,&exit){t.Fatalf("worker did not crash: %v %s",err,out)}
			ws,ok:=exit.Sys().(syscall.WaitStatus)
			if !ok||!ws.Signaled()||ws.Signal()!=syscall.SIGKILL{t.Fatalf("unexpected worker failure: %v %s",err,out)}
			s,err:=newStorage(root,key)
			if err!=nil{t.Fatal("startup recovery:",err)}
			job,ok:=s.get(strings.Repeat("1",24))
			if !ok{t.Fatal("job lost")}
			committed:=phase=="primary-sql-committed"||phase=="primary-json-committed"||phase=="metadata"
			live:=s.archiveDir(job.OwnerID,job.Author,job.PostID)
			if committed{
				if job.Title!="Archived fixture"||job.SharedForAll||len(job.SharedWith)!=0||job.AutoSync{t.Fatal("restored metadata or ACL wrong",job)}
				if _,err:=os.Stat(filepath.Join(live,"media","clip.mp4"));err!=nil{t.Fatal("historical media missing",err)}
			}else{
				if job.Title!="New"||!job.SharedForAll||len(job.SharedWith)!=1||!job.AutoSync{t.Fatal("uncommitted restore changed ACL or metadata",job)}
				if _,err:=os.Stat(filepath.Join(live,"media","photo.jpg"));err!=nil{t.Fatal("old media missing",err)}
			}
			history,err:=s.listArchiveRevisions(job)
			if err!=nil||len(history)!=2{t.Fatal("previous finished version was not preserved",history,err)}
			if _,err:=os.Stat(filepath.Join(s.revisionsDir(job),hist,"manifest.json"));err!=nil{t.Fatal("selected history lost",err)}
			if err:=verifyJSONSQLiteAgreement(root);err!=nil{t.Fatal("SQL/JSON diverged",err)}
			meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
			if err!=nil{t.Fatal(err)}
			var busy,frames,done int
			err=meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy,&frames,&done)
			closeErr:=meta.close()
			if err!=nil||closeErr!=nil||busy!=0{t.Fatal("checkpoint",err,closeErr,busy)}
			if err:=verifySQLiteSnapshotInventory(root);err!=nil{t.Fatal("SQL/media inventory diverged",err)}
			for _,name:=range []string{revisionRestoreJournalName,sqlitePrimaryTransactionName,sqliteHistoryJournalName,metadataJournalName}{
				if _,err:=os.Lstat(filepath.Join(root,name));!os.IsNotExist(err){t.Fatal("journal leaked",name,err)}
			}
		})
	}
}

func TestSQLRevisionRestoreRejectsChangedHistoricalBytes(t *testing.T){
	root,key,hist:=sqlRestoreFixture(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	job,ok:=s.get(strings.Repeat("1",24))
	if !ok{t.Fatal("missing SQL job")}
	if err:=writeAtomic(filepath.Join(s.revisionsDir(job),hist,"media","clip.mp4"),[]byte("tampered"));err!=nil{t.Fatal(err)}
	if _,err:=s.restoreArchiveRevision(job.OwnerID,job.ID,hist);err==nil{t.Fatal("tampered history accepted")}
	if _,err:=os.Stat(filepath.Join(s.archiveDir(job.OwnerID,job.Author,job.PostID),"media","photo.jpg"));err!=nil{t.Fatal("current archive damaged",err)}
	if _,err:=os.Lstat(filepath.Join(root,revisionRestoreJournalName));!os.IsNotExist(err){t.Fatal("invalid restore intent published")}
}
