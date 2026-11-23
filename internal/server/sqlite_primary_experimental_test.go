package server

import(
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLPrimaryExperimentalPublicOptInAndRollback(t *testing.T){
	cfg,backup,snapshot:=sqlActivationFixture(t)
	t.Setenv("BD_SQLITE_PRIMARY","1")
	if app,err:=New(cfg);err==nil{app.Close();t.Fatal("environment alone selected SQLite")}
	if err:=ActivateSQLitePrimaryExperimental(cfg.DataDir,backup,snapshot);err!=nil{t.Fatal(err)}
	t.Setenv("BD_SQLITE_PRIMARY","0")
	if app,err:=New(cfg);err==nil{app.Close();t.Fatal("SQLite selector alone enabled experimental mode")}
	t.Setenv("BD_SQLITE_PRIMARY","1")
	app,err:=New(cfg)
	if err!=nil{t.Fatal(err)}
	if app.store.primary==nil||app.store.shadow!=nil{t.Fatal("wrong backend")}
	job,ok:=app.store.get(strings.Repeat("d",24))
	if !ok{t.Fatal("missing job")}
	job.Title="public SQL write"
	if err:=app.store.upsert(job);err!=nil{t.Fatal(err)}
	app.Close()
	app,err=New(cfg)
	if err!=nil{t.Fatal("public SQL restart",err)}
	job,ok=app.store.get(job.ID)
	if !ok||job.Title!="public SQL write"{t.Fatal("SQL write lost")}
	app.Close()
	full:=filepath.Join(t.TempDir(),"new-full")
	if err:=PrepareFullDataSnapshot(cfg.DataDir,full);err!=nil{t.Fatal(err)}
	if err:=RollbackSQLiteToJSON(cfg.DataDir,full);err!=nil{t.Fatal(err)}
	if app,err:=New(cfg);err==nil{app.Close();t.Fatal("stale SQL opt-in silently fell back to JSON")}
	t.Setenv("BD_SQLITE_PRIMARY","0")
	app,err=New(cfg)
	if err!=nil{t.Fatal("public JSON restart",err)}
	defer app.Close()
	job,ok=app.store.get(job.ID)
	if !ok||job.Title!="public SQL write"||app.store.primary!=nil{t.Fatal("rollback lost SQL write")}
}

func TestSQLPrimaryExperimentalRejectsInvalidEnvAndMissingAccounts(t *testing.T){
	cfg,backup,snapshot:=sqlActivationFixture(t)
	t.Setenv("BD_SQLITE_PRIMARY","true")
	if app,err:=New(cfg);err==nil{app.Close();t.Fatal("ambiguous environment accepted")}
	if err:=ActivateSQLitePrimaryExperimental(cfg.DataDir,backup,snapshot);err!=nil{t.Fatal(err)}
	t.Setenv("BD_SQLITE_PRIMARY","1")
	path:=filepath.Join(cfg.DataDir,"accounts.json")
	for _,scenario:=range []string{"missing","empty"}{
		t.Run(scenario,func(t *testing.T){
			original,err:=os.ReadFile(path)
			if err!=nil{t.Fatal(err)}
			defer func(){if err:=writeAtomic(path,original);err!=nil{t.Error(err)}}()
			if scenario=="missing"{if err:=os.Remove(path);err!=nil{t.Fatal(err)}}else if err:=writeAtomic(path,[]byte("{}"));err!=nil{t.Fatal(err)}
			if app,err:=New(cfg);err==nil{app.Close();t.Fatal("SQL startup bootstrapped replacement accounts")}
			if scenario=="missing"{
				if _,err:=os.Stat(path);!os.IsNotExist(err){t.Fatal("account file recreated",err)}
			}else{
				raw,err:=os.ReadFile(path)
				if err!=nil||!bytes.Equal(raw,[]byte("{}")){t.Fatal("account file rewritten",err)}
			}
		})
	}
}
