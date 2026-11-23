package server

import(
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sqlVideoFixture(t *testing.T)(string,string,Job,string){
	t.Helper()
	app,job,_,_,folder:=videoPublicationFixture(t)
	root:=app.store.root
	key:=base64.StdEncoding.EncodeToString(app.store.key)
	app.Close()
	// The legacy video fixture permits zero timestamps. SQL does not.
	now:=time.Now().UTC()
	jobs,err:=sqliteShadowReadJobs(filepath.Join(root,"jobs.json"))
	if err!=nil{t.Fatal(err)}
	job=jobs[job.ID]
	job.CreatedAt,job.UpdatedAt=now,now
	jobs[job.ID]=job
	if err:=writeJSON(filepath.Join(root,"jobs.json"),jobs);err!=nil{t.Fatal(err)}
	if err:=writeJSON(filepath.Join(root,"imports.json"),map[string]AuthorImport{});err!=nil{t.Fatal(err)}
	cmd:=exec.Command("python3","../../tools/sqlite_migrate.py","--data-dir",root,"--output",filepath.Join(root,"metadata.db"))
	if out,err:=cmd.CombinedOutput();err!=nil{t.Fatalf("SQL video migration: %v %s",err,out)}
	return root,key,job,folder
}

func TestSQLVideoPublicationCrashWorker(t *testing.T){
	root:=os.Getenv("BOOSTY_TEST_SQL_VIDEO_ROOT")
	if root==""{return}
	s,err:=newStorage(root,os.Getenv("BOOSTY_TEST_SQL_VIDEO_KEY"))
	if err!=nil{t.Fatal(err)}
	meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
	if err!=nil{t.Fatal(err)}
	defer meta.close()
	s.primary=meta
	job,ok:=s.get(strings.Repeat("d",24))
	if !ok{t.Fatal("missing SQL video job")}
	folder:=s.archiveDir(job.OwnerID,job.Author,job.PostID)
	_,man,err:=readArchiveManifestNoFollow(folder)
	if err!=nil{t.Fatal(err)}
	staged:=os.Getenv("BOOSTY_TEST_SQL_VIDEO_STAGE")
	size,digest,err:=fileDigest(staged)
	if err!=nil{t.Fatal(err)}
	s.videoCheckpoint=func(phase string)error{
		if phase==os.Getenv("BOOSTY_TEST_SQL_VIDEO_PHASE"){return syscall.Kill(os.Getpid(),syscall.SIGKILL)}
		return nil
	}
	a:=&App{store:s,archiveGates:s.gates}
	_,err=a.publishVideoRendition(job,man.CapturedAt,man.Assets[0],"movie.mp4.h264.mp4",staged,size,digest)
	t.Fatalf("worker escaped checkpoint: %v",err)
}

func TestSQLVideoPublicationRecoversSIGKILL(t *testing.T){
	for _,phase:=range []string{"journal","media","manifest","html","sql-prepared","sql-committed","committed"}{
		t.Run(phase,func(t *testing.T){
			root,key,job,folder:=sqlVideoFixture(t)
			staged,_,_:=newVideoPublicationStage(t,root)
			cmd:=exec.Command(os.Args[0],"-test.run=^TestSQLVideoPublicationCrashWorker$")
			cmd.Env=append(os.Environ(),"BOOSTY_TEST_SQL_VIDEO_ROOT="+root,"BOOSTY_TEST_SQL_VIDEO_KEY="+key,
				"BOOSTY_TEST_SQL_VIDEO_STAGE="+staged,"BOOSTY_TEST_SQL_VIDEO_PHASE="+phase)
			out,err:=cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err,&exit){t.Fatalf("worker did not crash: %v %s",err,out)}
			ws,ok:=exit.Sys().(syscall.WaitStatus)
			if !ok||!ws.Signaled()||ws.Signal()!=syscall.SIGKILL{t.Fatalf("unexpected worker failure: %v %s",err,out)}
			s,err:=newStorage(root,key)
			if err!=nil{t.Fatal("startup recovery:",err)}
			recovered,ok:=s.get(job.ID)
			if !ok||!sqliteShadowEqual(job,recovered){t.Fatal("video changed job metadata")}
			committed:=phase=="sql-committed"||phase=="committed"
			_,man,err:=readArchiveManifestNoFollow(folder)
			if err!=nil||(len(man.Assets)==2)!=committed{t.Fatal("wrong manifest generation",err)}
			if raw,err:=os.ReadFile(filepath.Join(folder,"media","movie.mp4"));err!=nil||string(raw)!="original source media"{t.Fatal("original video lost",err)}
			raw,err:=os.ReadFile(filepath.Join(folder,"media","movie.mp4.h264.mp4"))
			if committed&&(err!=nil||string(raw)!="encoded media bytes")||!committed&&!os.IsNotExist(err){t.Fatal("wrong rendition",err)}
			meta,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
			if err!=nil{t.Fatal(err)}
			var busy,frames,done int
			err=meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy,&frames,&done)
			closeErr:=meta.close()
			if err!=nil||closeErr!=nil||busy!=0{t.Fatal("checkpoint",err,closeErr,busy)}
			if err:=verifySQLiteSnapshotInventory(root);err!=nil{t.Fatal("SQL/media inventory diverged",err)}
			if err:=verifyJSONSQLiteAgreement(root);err!=nil{t.Fatal("SQL/JSON diverged",err)}
		})
	}
}

func TestVideoPublicationRollbackCleansPinnedReplayTemp(t *testing.T){
	for _,hadOld:=range []bool{false,true}{
		t.Run(map[bool]string{false:"new",true:"replacement"}[hadOld],func(t *testing.T){
			app,_,_,_,folder:=videoPublicationFixture(t)
			root:=app.store.root
			key:=base64.StdEncoding.EncodeToString(app.store.key)
			if hadOld{if err:=os.WriteFile(filepath.Join(folder,"media","movie.mp4.h264.mp4"),[]byte("previous rendition"),0600);err!=nil{t.Fatal(err)}}
			staged,_,_:=newVideoPublicationStage(t,root)
			videoJournalSIGKILL(t,root,key,staged,"journal")
			j,found,err:=videoPublicationReadJournal(root)
			if err!=nil||!found{t.Fatal("journal missing",err)}
			temp:=filepath.Join(folder,"media",".video-replay-"+j.Nonce)
			if err:=os.Link(filepath.Join(root,videoPublicationStagePrefix+j.Nonce,"media.new"),temp);err!=nil{t.Fatal(err)}
			if _,err:=newStorage(root,key);err!=nil{t.Fatal("replay temp recovery",err)}
			if _,err:=os.Lstat(temp);!os.IsNotExist(err){t.Fatal("replay temp leaked",err)}
		})
	}
}
