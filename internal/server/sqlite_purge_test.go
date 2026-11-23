package server

import(
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func sqlPurgeFixture(t *testing.T)(string,string){
	t.Helper()
	root,key,_:=sqlRestoreFixture(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	s.primary=meta
	// Both owners archive the same source post independently. Build the
	// second owner's real archive with the same media bytes and SQL blob.
	job,ok:=s.get(strings.Repeat("2",24))
	if !ok{t.Fatal("missing second owner")}
	job.State="running"
	job.Error=""
	if err:=s.upsert(job);err!=nil{t.Fatal(err)}
	stage:=archiveUpdateWriteStage(t,s,job)
	if committed,err:=s.publishArchiveUpdate(job,stage);err!=nil||!committed{t.Fatal("second owner fixture",err)}
	if n,err:=s.removeOwned("first",strings.Repeat("1",24),"");err!=nil||n!=1{t.Fatal("trash fixture",n,err)}
	if err:=meta.close();err!=nil{t.Fatal(err)}
	return root,key
}

func TestSQLPurgeCrashWorker(t *testing.T){
	root:=os.Getenv("BOOSTY_TEST_SQL_PURGE_ROOT")
	if root==""{return}
	s,err:=newStorage(root,os.Getenv("BOOSTY_TEST_SQL_PURGE_KEY"))
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	s.purgeCheckpoint=func(phase string)error{
		if phase==os.Getenv("BOOSTY_TEST_SQL_PURGE_PHASE"){return syscall.Kill(os.Getpid(),syscall.SIGKILL)}
		return nil
	}
	_,err=s.purgeOwned("first",strings.Repeat("1",24),"")
	t.Fatalf("purge worker escaped checkpoint: %v",err)
}

func TestSQLPurgeRecoversSIGKILL(t *testing.T){
	for _,phase:=range []string{"journal","moved","primary-prepared","primary-sql-committed","primary-json-committed","metadata"}{
		t.Run(phase,func(t *testing.T){
			root,key:=sqlPurgeFixture(t)
			cmd:=exec.Command(os.Args[0],"-test.run=^TestSQLPurgeCrashWorker$")
			cmd.Env=append(os.Environ(),"BOOSTY_TEST_SQL_PURGE_ROOT="+root,"BOOSTY_TEST_SQL_PURGE_KEY="+key,"BOOSTY_TEST_SQL_PURGE_PHASE="+phase)
			out,err:=cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err,&exit){t.Fatalf("worker did not crash: %v %s",err,out)}
			ws,ok:=exit.Sys().(syscall.WaitStatus)
			if !ok||!ws.Signaled()||ws.Signal()!=syscall.SIGKILL{t.Fatalf("unexpected worker failure: %v %s",err,out)}
			s,err:=newStorage(root,key)
			if err!=nil{t.Fatal("startup recovery:",err)}
			committed:=phase=="primary-sql-committed"||phase=="primary-json-committed"||phase=="metadata"
			job,exists:=s.jobs[strings.Repeat("1",24)]
			if exists==committed||(!committed&&job.DeletedAt==nil){t.Fatal("wrong trash generation")}
			for _,path:=range []string{filepath.Join(root,"library","first","creator","post-1"),filepath.Join(root,"revisions","first","creator","post-1")}{
				_,err:=os.Stat(path)
				if committed&&!os.IsNotExist(err)||!committed&&err!=nil{t.Fatal("wrong archive folder",path,err)}
			}
			second,ok:=s.get(strings.Repeat("2",24))
			if !ok{t.Fatal("second owner disappeared")}
			if raw,err:=os.ReadFile(filepath.Join(s.archiveDir(second.OwnerID,second.Author,second.PostID),"media","photo.jpg"));err!=nil||string(raw)!="new media bytes"{t.Fatal("shared media lost",err)}
			if err:=verifyJSONSQLiteAgreement(root);err!=nil{t.Fatal("SQL/JSON diverged",err)}
			meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
			if err!=nil{t.Fatal(err)}
			var busy,frames,done int
			err=meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy,&frames,&done)
			closeErr:=meta.close()
			if err!=nil||closeErr!=nil||busy!=0{t.Fatal("checkpoint",err,closeErr,busy)}
			if err:=verifySQLiteSnapshotInventory(root);err!=nil{t.Fatal("SQL/media inventory diverged",err)}
			if _,err:=os.Stat(filepath.Join(root,purgeJournalName));!os.IsNotExist(err){t.Fatal("journal leaked",err)}
			items,err:=os.ReadDir(filepath.Join(root,"trash"))
			if err!=nil||len(items)!=0{t.Fatal("trash stages leaked",items,err)}
		})
	}
}

func TestSQLPurgeRejectsUnindexedHistory(t *testing.T){
	root,key:=sqlPurgeFixture(t)
	s,err:=newStorage(root,key)
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	path:=filepath.Join(root,"revisions","first","creator","post-1",strings.Repeat("b",24))
	if err:=os.Mkdir(path,0700);err!=nil{t.Fatal(err)}
	if n,err:=s.purgeOwned("first",strings.Repeat("1",24),"");err==nil||n!=0{t.Fatal("purge accepted unexpected history",n,err)}
	if _,err:=os.Stat(path);err!=nil{t.Fatal("unexpected history destroyed",err)}
}
