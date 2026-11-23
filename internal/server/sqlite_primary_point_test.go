package server

import(
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLPrimaryPointGenerationInvalidates(t *testing.T){
	for _,scenario:=range []string{"same-connection","external-connection","reconnect","target-memory","import-memory","schema-change","closed-db"}{
		t.Run(scenario,func(t *testing.T){
			s,meta,root:=sqlFirstStorageFixture(t)
			id:=strings.Repeat("1",24)
			importID:=strings.Repeat("a",24)
			if _,ok:=s.get(id);!ok{t.Fatal("initial point read")}
			stamp:=s.primaryPointStamp
			if stamp.token==""{t.Fatal("generation not established")}
			if _,ok:=s.get(id);!ok||s.primaryPointStamp!=stamp{t.Fatal("stable generation not reused")}
			switch scenario{
			case "same-connection":
				if _,err:=meta.db.Exec("UPDATE author_imports SET job_state='queued' WHERE import_id=?",importID);err!=nil{t.Fatal(err)}
			case "external-connection":
				other,err:=openSQLiteMetadata(filepath.Join(root,"metadata.db"))
				if err!=nil{t.Fatal(err)}
				if _,err:=other.db.Exec("UPDATE author_imports SET job_state='queued' WHERE import_id=?",importID);err!=nil{other.close();t.Fatal(err)}
				if err:=other.close();err!=nil{t.Fatal(err)}
			case "reconnect":
				meta.db.SetMaxIdleConns(0)
				meta.db.SetMaxIdleConns(1)
				if _,ok:=s.get(id);!ok{t.Fatal("valid reconnect rejected")}
				if s.primaryPointStamp.token==stamp.token{t.Fatal("reused connection-local stamp")}
				return
			case "target-memory":s.jobs[id].Title="uncommitted"
			case "import-memory":
				s.imports[importID].State="canceled"
				if !s.importCanceled(importID)||s.primaryFatal==nil{t.Fatal("import memory drift accepted")}
				return
			case "schema-change":
				if _,err:=meta.db.Exec("CREATE TABLE test_generation_ddl (id INTEGER)");err!=nil{t.Fatal(err)}
				if _,ok:=s.get(id);!ok{t.Fatal("valid schema change rejected")}
				if s.primaryPointStamp.schemaVersion==stamp.schemaVersion{t.Fatal("schema change not detected")}
				return
			case "closed-db":if err:=meta.close();err!=nil{t.Fatal(err)}
			}
			if _,ok:=s.get(id);ok||s.primaryFatal==nil{t.Fatal("point read accepted divergent generation")}
		})
	}
}

func TestSQLPrimaryPointReadsCommittedACLAndMissingIDs(t *testing.T){
	s,_,_:=sqlFirstStorageFixture(t)
	id:=strings.Repeat("1",24)
	job,ok:=s.get(id)
	if !ok{t.Fatal("missing archive")}
	stamp:=s.primaryPointStamp
	job.SharedWith=[]string{"new-member"}
	job.SharedForAll=false
	if err:=s.upsert(job);err!=nil{t.Fatal(err)}
	got,ok:=s.get(id)
	if !ok||got.SharedForAll||len(got.SharedWith)!=1||got.SharedWith[0]!="new-member"{t.Fatal("stale ACL",got)}
	if s.primaryPointStamp==stamp{t.Fatal("commit did not invalidate generation")}
	if _,ok:=s.get(strings.Repeat("9",24));ok||s.primaryFatal!=nil{t.Fatal("missing ID unexpectedly quarantined")}
	if !s.importCanceled(strings.Repeat("9",24))||s.primaryFatal!=nil{t.Fatal("missing import unexpectedly quarantined")}
	// A new unexpected SQL-only row invalidates even an already-warm read.
	newJob:=got
	newJob.ID=strings.Repeat("e",24)
	newJob.PostID="unreconciled-post"
	if err:=s.primary.apply(context.Background(),[]sqliteArchiveChange{{After:newJob}},nil);err!=nil{t.Fatal(err)}
	if _,ok:=s.get(id);ok||s.primaryFatal==nil{t.Fatal("unreconciled SQL row not detected")}
}
