package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqlAppFixture(t *testing.T)(Config,Job,string){
	t.Helper()
	root,key,job,folder:=sqlVideoFixture(t)
	if err:=writeSyncedMetadata(root,sqliteBackendSelectionName,[]byte(`{"version":1,"active":"sqlite"}`));err!=nil{t.Fatal(err)}
	return Config{DataDir:root,SecretKey:key,AdminUser:"admin",AdminPassword:"strong-password-123"},job,folder
}

func sqlAppRequest(a *App,method,path,token,body string)*httptest.ResponseRecorder{
	req:=httptest.NewRequest(method,path,strings.NewReader(body))
	if token!=""{req.AddCookie(&http.Cookie{Name:sessionCookie,Value:token})}
	req.Header.Set("X-BD-Request","1")
	req.Header.Set("Content-Type","application/json")
	rec:=httptest.NewRecorder()
	a.Handler().ServeHTTP(rec,req)
	return rec
}

func TestSQLPrimaryAppLifecycle(t *testing.T){
	cfg,job,_:=sqlAppFixture(t)
	a,err:=newAppMode(cfg,true)
	if err!=nil{t.Fatal(err)}
	defer a.Close()
	if a.store.primary==nil||a.store.shadow!=nil{t.Fatal("wrong active backend")}
	if rec:=sqlAppRequest(a,"GET","/healthz","","");rec.Code!=200{t.Fatal(rec.Code,rec.Body.String())}
	if second,err:=newAppMode(cfg,true);err==nil{second.Close();t.Fatal("second process acquired data lock")}
	token,err:=a.accounts.session("admin")
	if err!=nil{t.Fatal(err)}
	rec:=sqlAppRequest(a,"GET","/api/jobs",token,"")
	if rec.Code!=200||!strings.Contains(rec.Body.String(),job.ID){t.Fatal("SQL catalogue",rec.Code,rec.Body.String())}
	rec=sqlAppRequest(a,"GET","/library/"+job.ID+"/media/movie.mp4",token,"")
	if rec.Code!=200||rec.Body.String()!="original source media"{t.Fatal("SQL media read",rec.Code,rec.Body.String())}
	rec=sqlAppRequest(a,"DELETE","/api/jobs/"+job.ID,token,"")
	if rec.Code!=200{t.Fatal("SQL trash",rec.Code,rec.Body.String())}
	if rec=sqlAppRequest(a,"GET","/library/"+job.ID+"/media/movie.mp4",token,"");rec.Code!=404{t.Fatal("trashed archive served",rec.Code)}
	meta:=a.store.primary
	a.Close()
	a.Close()
	if err:=meta.db.Ping();err==nil{t.Fatal("SQL handle survived shutdown")}
	lock,err:=lockDataDirectory(cfg.DataDir)
	if err!=nil{t.Fatal("shutdown retained directory lock",err)}
	lock.Close()
	a,err=newAppMode(cfg,true)
	if err!=nil{t.Fatal("restart",err)}
	defer a.Close()
	if len(a.store.trashed("admin"))!=1{t.Fatal("SQL trash lost across restart")}
	token,err=a.accounts.session("admin")
	if err!=nil{t.Fatal(err)}
	rec=sqlAppRequest(a,"POST","/api/trash/"+job.ID+"/restore",token,"")
	if rec.Code!=200{t.Fatal("SQL restore trash",rec.Code,rec.Body.String())}
	if rec=sqlAppRequest(a,"GET","/library/"+job.ID+"/media/movie.mp4",token,"");rec.Code!=200{t.Fatal("restored SQL archive not served",rec.Code)}
	jobs,err:=sqliteShadowReadJobs(filepath.Join(cfg.DataDir,"jobs.json"))
	if err!=nil{t.Fatal(err)}
	imports,err:=sqliteShadowReadImports(filepath.Join(cfg.DataDir,"imports.json"))
	if err!=nil{t.Fatal(err)}
	if err:=a.store.primary.verifyJSONParity(context.Background(),jobs,imports);err!=nil{t.Fatal(err)}
}

func TestSQLPrimaryAppHealthFailsClosed(t *testing.T){
	cfg,job,_:=sqlAppFixture(t)
	a,err:=newAppMode(cfg,true)
	if err!=nil{t.Fatal(err)}
	defer a.Close()
	token,err:=a.accounts.session("admin")
	if err!=nil{t.Fatal(err)}
	if _,err:=a.store.primary.db.Exec("UPDATE owner_archives SET shared_for_all=1 WHERE archive_id=?",job.ID);err!=nil{t.Fatal(err)}
	if rec:=sqlAppRequest(a,"GET","/healthz","","");rec.Code!=503{t.Fatal("corruption did not fail health",rec.Code)}
	if rec:=sqlAppRequest(a,"GET","/library/"+job.ID+"/media/movie.mp4",token,"");rec.Code!=404{t.Fatal("quarantined SQL media served",rec.Code)}
}

func TestSQLPrimaryAppFailedInitReleasesLock(t *testing.T){
	cfg,_,_:=sqlAppFixture(t)
	path:=filepath.Join(cfg.DataDir,"accounts.json")
	original,err:=os.ReadFile(path)
	if err!=nil{t.Fatal(err)}
	if err:=writeAtomic(path,[]byte("invalid accounts"));err!=nil{t.Fatal(err)}
	if a,err:=newAppMode(cfg,true);err==nil{a.Close();t.Fatal("invalid accounts accepted")}
	lock,err:=lockDataDirectory(cfg.DataDir)
	if err!=nil{t.Fatal("failed app retained lock",err)}
	lock.Close()
	if err:=writeAtomic(path,original);err!=nil{t.Fatal(err)}
	a,err:=newAppMode(cfg,true)
	if err!=nil{t.Fatal("restart after repaired accounts",err)}
	a.Close()
}

func TestSQLPrimaryAppRejectsConflictingModes(t *testing.T){
	cfg,_,_:=sqlAppFixture(t)
	for _,flag:=range []string{"BD_SQLITE_SHADOW","BD_SQLITE_VERIFY","BD_SQLITE_MEDIA_SHADOW","BD_SQLITE_CATALOG_READS"}{
		t.Run(flag,func(t *testing.T){
			t.Setenv(flag,"1")
			before,err:=os.ReadFile(filepath.Join(cfg.DataDir,"jobs.json"))
			if err!=nil{t.Fatal(err)}
			if a,err:=newAppMode(cfg,true);err==nil{a.Close();t.Fatal("conflicting mode accepted")}
			after,err:=os.ReadFile(filepath.Join(cfg.DataDir,"jobs.json"))
			if err!=nil||!bytes.Equal(before,after){t.Fatal("rejected mode rewrote mirror",err)}
		})
	}
}
