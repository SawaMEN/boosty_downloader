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

func TestSQLHistoryJournalCrashWorker(t *testing.T){
	root:=os.Getenv("BOOSTY_TEST_HISTORY_ROOT")
	if root==""{return}
	s,err:=newStorage(root,os.Getenv("BOOSTY_TEST_HISTORY_KEY"))
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	job,ok:=s.get(strings.Repeat("1",24))
	if !ok{t.Fatal("missing job")}
	s.sqliteHistoryCheckpoint=func(phase string)error{
		if phase==os.Getenv("BOOSTY_TEST_HISTORY_PHASE"){return syscall.Kill(os.Getpid(),syscall.SIGKILL)}
		return nil
	}
	_,err=s.saveArchiveRevision(job)
	t.Fatalf("crash worker escaped checkpoint: %v",err)
}

func TestSQLHistoryJournalRecoversSIGKILL(t *testing.T){
	for _,phase:=range []string{"prepared","published","indexed"}{
		t.Run(phase,func(t *testing.T){
			root,id,history,_:=sqliteSnapshotAuditFixture(t)
			key:=testKey(t)
			cmd:=exec.Command(os.Args[0],"-test.run=^TestSQLHistoryJournalCrashWorker$")
			cmd.Env=append(os.Environ(),"BOOSTY_TEST_HISTORY_ROOT="+root,"BOOSTY_TEST_HISTORY_KEY="+key,"BOOSTY_TEST_HISTORY_PHASE="+phase)
			out,err:=cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err,&exit){t.Fatalf("worker did not crash: %v %s",err,out)}
			ws,ok:=exit.Sys().(syscall.WaitStatus)
			if !ok||!ws.Signaled()||ws.Signal()!=syscall.SIGKILL{t.Fatalf("unexpected worker failure: %v %s",err,out)}
			if err:=sqliteReindexNoWAL(root);err==nil{t.Fatal("offline operation ignored history journal")}
			s,err:=newStorage(root,key)
			if err!=nil{t.Fatal("startup recovery:",err)}
			job,ok:=s.get(id)
			if !ok{t.Fatal("job disappeared")}
			list,err:=s.listArchiveRevisions(job)
			if err!=nil||len(list)!=2{t.Fatal("history not recovered exactly once",list,err)}
			if _,err:=os.Stat(filepath.Join(s.revisionsDir(job),history,"manifest.json"));err!=nil{t.Fatal("old history lost",err)}
			meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
			if err!=nil{t.Fatal(err)}
			var busy,logFrames,checkpointed int
			err=meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy,&logFrames,&checkpointed)
			closeErr:=meta.close()
			if err!=nil||closeErr!=nil||busy!=0{t.Fatal("checkpoint",err,closeErr,busy)}
			if err:=verifyJSONSQLiteAgreement(root);err!=nil{t.Fatal("metadata or ACL changed",err)}
			if err:=verifySQLiteSnapshotInventory(root);err!=nil{t.Fatal("history index diverged",err)}
			if _,err:=newStorage(root,key);err!=nil{t.Fatal("second recovery failed",err)}
		})
	}
}

func TestSQLHistoryJournalRejectsModifiedPreparedSnapshot(t *testing.T){
	root,id,_,_:=sqliteSnapshotAuditFixture(t)
	key:=testKey(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	s.primary=meta
	job,ok:=s.get(id)
	if !ok{t.Fatal("missing job")}
	s.sqliteHistoryCheckpoint=func(phase string)error{return syscall.ENOSPC}
	if _,err:=s.saveArchiveRevision(job);err==nil{t.Fatal("expected interrupted publication")}
	if err:=meta.close();err!=nil{t.Fatal(err)}
	entries,err:=os.ReadDir(s.revisionsDir(job))
	if err!=nil{t.Fatal(err)}
	for _,entry:=range entries{
		if strings.HasPrefix(entry.Name(),".incomplete-"){
			// Replace HTML rather than altering a shared hardlink in place.
			if err:=writeAtomic(filepath.Join(s.revisionsDir(job),entry.Name(),"index.html"),[]byte("tampered HTML"));err!=nil{t.Fatal(err)}
		}
	}
	if _,err:=newStorage(root,key);err==nil{t.Fatal("tampered staged bytes were trusted")}
	if _,err:=os.Lstat(filepath.Join(root,sqliteHistoryJournalName));err!=nil{t.Fatal("unresolved intent removed",err)}
}
