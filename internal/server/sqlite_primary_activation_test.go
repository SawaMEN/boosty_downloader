package server

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func sqlActivationFixture(t *testing.T)(Config,string,string){
	t.Helper()
	root,key,_,_:=sqlVideoFixture(t)
	backup:=filepath.Join(t.TempDir(),"metadata-backup")
	snapshot:=filepath.Join(t.TempDir(),"full-backup")
	if err:=PinJSONBackendSelection(root);err!=nil{t.Fatal(err)}
	if err:=PrepareSQLiteBackup(root,backup);err!=nil{t.Fatal(err)}
	if err:=PrepareFullDataSnapshot(root,snapshot);err!=nil{t.Fatal(err)}
	return Config{DataDir:root,SecretKey:key,AdminUser:"admin",AdminPassword:"strong-password-123"},backup,snapshot
}

func TestSQLPrimaryActivationCrashWorker(t *testing.T){
	root:=os.Getenv("BOOSTY_TEST_SQL_ACTIVATE_ROOT")
	if root==""{return}
	err:=activateSQLPrimaryOffline(root,os.Getenv("BOOSTY_TEST_SQL_ACTIVATE_BACKUP"),os.Getenv("BOOSTY_TEST_SQL_ACTIVATE_FULL"),func(phase string)error{
		if phase==os.Getenv("BOOSTY_TEST_SQL_ACTIVATE_PHASE"){return syscall.Kill(os.Getpid(),syscall.SIGKILL)}
		return nil
	})
	t.Fatalf("activation worker escaped checkpoint: %v",err)
}

func TestSQLPrimaryActivationAtomicSelector(t *testing.T){
	for _,phase:=range []string{"validated","selected"}{
		t.Run(phase,func(t *testing.T){
			cfg,backup,snapshot:=sqlActivationFixture(t)
			root:=cfg.DataDir
			before:=map[string][]byte{}
			for _,name:=range []string{"jobs.json","imports.json","metadata.db","accounts.json","library/admin/creator/post-a/media/movie.mp4"}{
				raw,err:=os.ReadFile(filepath.Join(root,name))
				if err!=nil{t.Fatal(err)}
				before[name]=raw
			}
			cmd:=exec.Command(os.Args[0],"-test.run=^TestSQLPrimaryActivationCrashWorker$")
			cmd.Env=append(os.Environ(),"BOOSTY_TEST_SQL_ACTIVATE_ROOT="+root,"BOOSTY_TEST_SQL_ACTIVATE_BACKUP="+backup,
				"BOOSTY_TEST_SQL_ACTIVATE_FULL="+snapshot,"BOOSTY_TEST_SQL_ACTIVATE_PHASE="+phase)
			out,err:=cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err,&exit){t.Fatalf("worker did not crash: %v %s",err,out)}
			ws,ok:=exit.Sys().(syscall.WaitStatus)
			if !ok||!ws.Signaled()||ws.Signal()!=syscall.SIGKILL{t.Fatalf("unexpected worker failure: %v %s",err,out)}
			expected:="json"
			if phase=="selected"{expected="sqlite"}
			if mode,err:=readSQLiteBackendSelection(root);err!=nil||mode!=expected{t.Fatal("wrong backend commit",mode,err)}
			for name,raw:=range before{
				after,err:=os.ReadFile(filepath.Join(root,name))
				if err!=nil||!bytes.Equal(raw,after){t.Fatal("activation modified data",name,err)}
			}
			if expected=="json"{
				if err:=activateSQLPrimaryOffline(root,backup,snapshot,nil);err!=nil{t.Fatal("retry activation",err)}
			}
			// The old public constructor is still closed; an internal SQL
			// app can start with the new selector and serve real media.
			if a,err:=New(cfg);err==nil{a.Close();t.Fatal("legacy server accepted SQL selection")}
			app,err:=newAppMode(cfg,true)
			if err!=nil{t.Fatal("SQL app after activation",err)}
			if rec:=sqlAppRequest(app,"GET","/healthz","","");rec.Code!=200{t.Fatal("SQL health",rec.Code)}
			app.Close()
			if err:=VerifySQLiteMetadataBackup(backup);err!=nil{t.Fatal("backup changed",err)}
			if err:=VerifyFullDataSnapshot(snapshot);err!=nil{t.Fatal("full recovery point changed",err)}
		})
	}
}

func TestSQLPrimaryActivationRejectsUnsafeRecoveryPoints(t *testing.T){
	for _,scenario:=range []string{"missing-full","stale-full","stale-metadata","pending-journal","locked","legacy-auth"}{
		t.Run(scenario,func(t *testing.T){
			cfg,backup,snapshot:=sqlActivationFixture(t)
			root:=cfg.DataDir
			switch scenario{
			case "missing-full":snapshot=""
			case "stale-full":
				if err:=os.WriteFile(filepath.Join(root,"new-private-data"),[]byte("not backed up"),0600);err!=nil{t.Fatal(err)}
			case "stale-metadata":
				path:=filepath.Join(root,"jobs.json")
				raw,err:=os.ReadFile(path)
				if err!=nil{t.Fatal(err)}
				if err:=writeAtomic(path,append(raw,'\n'));err!=nil{t.Fatal(err)}
			case "pending-journal":
				if err:=os.WriteFile(filepath.Join(root,sqliteCutoverIntentName),[]byte("{}"),0600);err!=nil{t.Fatal(err)}
			case "locked":
				lock,err:=lockDataDirectory(root)
				if err!=nil{t.Fatal(err)}
				defer lock.Close()
			case "legacy-auth":
				if err:=os.WriteFile(filepath.Join(root,"boosty-auth.enc"),[]byte("legacy"),0600);err!=nil{t.Fatal(err)}
			}
			path:=filepath.Join(root,sqliteBackendSelectionName)
			before,err:=os.ReadFile(path)
			if err!=nil{t.Fatal(err)}
			if err:=activateSQLPrimaryOffline(root,backup,snapshot,nil);err==nil{t.Fatal("unsafe activation accepted")}
			after,err:=os.ReadFile(path)
			if err!=nil||!bytes.Equal(before,after){t.Fatal("failed activation changed selector",err)}
		})
	}
}

func TestSQLPrimaryActivationBlocksShadowMaintenance(t *testing.T){
	cfg,backup,snapshot:=sqlActivationFixture(t)
	root:=cfg.DataDir
	if err:=activateSQLPrimaryOffline(root,backup,snapshot,nil);err!=nil{t.Fatal(err)}
	if err:=activateSQLPrimaryOffline(root,backup,snapshot,nil);err==nil{t.Fatal("active backend reactivated")}
	if err:=RestoreSQLiteShadowFromBackup(root,backup);err==nil{t.Fatal("shadow restore replaced selected primary")}
	if err:=ReindexSQLiteRevisionInventory(root,backup);err==nil{t.Fatal("shadow reindex modified selected primary")}
	if mode,err:=readSQLiteBackendSelection(root);err!=nil||mode!="sqlite"{t.Fatal("selector changed",mode,err)}
}
