package server

import(
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The stamp is NOT cached authorization. It only proves that a complete
// SQL projection was checked since the last observable database change.
// Every requested row and its grants are still read in a fresh transaction.
// SQLite data_version is connection-local; the TEMP token prevents reuse
// across pool reconnects. total_changes detects writes on the same
// connection, and schema_version detects DDL. Never persist this stamp.
type sqlitePrimaryGeneration struct{
	token string
	dataVersion,schemaVersion,changes int64
	jobs,imports int
}

func sqlitePrimaryReadStamp(ctx context.Context,conn *sql.Conn)(sqlitePrimaryGeneration,error){
	var stamp sqlitePrimaryGeneration
	const query="SELECT token,total_changes() FROM temp.bd_primary_read_generation WHERE singleton=1"
	err:=conn.QueryRowContext(ctx,query).Scan(&stamp.token,&stamp.changes)
	if err!=nil{
		// A newly opened physical connection has no TEMP table. The token
		// changes even if SQLite's counters happen to match an old handle.
		if _,e:=conn.ExecContext(ctx,"CREATE TEMP TABLE IF NOT EXISTS bd_primary_read_generation(singleton INTEGER PRIMARY KEY CHECK(singleton=1),token TEXT NOT NULL)");e!=nil{return stamp,e}
		if _,e:=conn.ExecContext(ctx,"INSERT OR IGNORE INTO temp.bd_primary_read_generation VALUES(1,hex(randomblob(16)))");e!=nil{return stamp,e}
		if err:=conn.QueryRowContext(ctx,query).Scan(&stamp.token,&stamp.changes);err!=nil{return stamp,err}
	}
	if len(stamp.token)!=32{return stamp,errors.New("invalid SQL point connection token")}
	if err:=conn.QueryRowContext(ctx,"PRAGMA main.data_version").Scan(&stamp.dataVersion);err!=nil{return stamp,err}
	if err:=conn.QueryRowContext(ctx,"PRAGMA main.schema_version").Scan(&stamp.schemaVersion);err!=nil{return stamp,err}
	return stamp,nil
}

func(s *Storage) sqlitePrimaryPointReadLocked(read func(context.Context,*sql.Tx)error)error{
	if s.primaryFatal!=nil{return s.primaryFatal}
	if s.primary==nil{return errors.New("SQL-primary point reader not configured")}
	if s.shadow!=nil{return s.sqlitePrimaryQuarantineLocked(errors.New("SQL-primary point read conflicts with shadow writer"))}
	ctx:=context.Background()
	conn,err:=s.primary.db.Conn(ctx)
	if err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	defer conn.Close()
	stamp,err:=sqlitePrimaryReadStamp(ctx,conn)
	if err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	stamp.jobs,stamp.imports=len(s.jobs),len(s.imports)
	tx,err:=conn.BeginTx(ctx,&sql.TxOptions{ReadOnly:true})
	if err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	defer tx.Rollback()
	if stamp!=s.primaryPointStamp{
		if err:=s.sqlitePrimaryValidateTxLocked(ctx,tx);err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	}
	if err:=read(ctx,tx);err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	if err:=tx.Commit();err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	// Detect a concurrent external commit between the stamp and the
	// snapshot (or after the snapshot). No result is exposed on mismatch.
	after,err:=sqlitePrimaryReadStamp(ctx,conn)
	if err!=nil{return s.sqlitePrimaryQuarantineLocked(err)}
	after.jobs,after.imports=len(s.jobs),len(s.imports)
	if after!=stamp{return s.sqlitePrimaryQuarantineLocked(errors.New("SQL generation changed during point read"))}
	s.primaryPointStamp=stamp
	return nil
}

func(s *Storage) sqlitePrimaryPointJobLocked(id string)(Job,bool,error){
	if !safeArchivePart(id){return Job{},false,nil}
	var result Job
	found:=false
	err:=s.sqlitePrimaryPointReadLocked(func(ctx context.Context,tx *sql.Tx)error{
		current:=s.jobs[id]
		if current==nil{
			var exists int
			if err:=tx.QueryRowContext(ctx,"SELECT EXISTS(SELECT 1 FROM owner_archives WHERE archive_id=?)",id).Scan(&exists);err!=nil{return err}
			if exists!=0{return fmt.Errorf("unreconciled SQL archive %s",id)}
			return nil
		}
		job,err:=sqliteReadJobTx(ctx,tx,current.OwnerID,id,true)
		if err!=nil{return err}
		if !sqliteShadowEqual(*current,job){return fmt.Errorf("SQL-primary point archive differs: %s",id)}
		result,found=job,true
		return nil
	})
	if err!=nil{return Job{},false,err}
	return result,found,nil
}

func(s *Storage) sqlitePrimaryPointImportLocked(id string)(AuthorImport,bool,error){
	if !safeArchivePart(id){return AuthorImport{},false,nil}
	var result AuthorImport
	found:=false
	err:=s.sqlitePrimaryPointReadLocked(func(ctx context.Context,tx *sql.Tx)error{
		current:=s.imports[id]
		if current==nil{
			var exists int
			if err:=tx.QueryRowContext(ctx,"SELECT EXISTS(SELECT 1 FROM author_imports WHERE import_id=?)",id).Scan(&exists);err!=nil{return err}
			if exists!=0{return fmt.Errorf("unreconciled SQL import %s",id)}
			return nil
		}
		imp,err:=sqliteReadImport(ctx,tx,current.OwnerID,id)
		if err!=nil{return err}
		if !sqliteShadowEqual(*current,imp){return fmt.Errorf("SQL-primary point import differs: %s",id)}
		result,found=imp,true
		return nil
	})
	if err!=nil{return AuthorImport{},false,err}
	return result,found,nil
}
