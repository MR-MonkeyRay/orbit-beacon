package inbox_test

import (
 "context"
 "encoding/json"
 "math"
 "os"
 "path/filepath"
 "testing"
 "time"

 "orbitbeacon/internal/server/inbox"
 "orbitbeacon/internal/server/storage"
)

func TestSequenceExhaustionDoesNotInsertOrRewind(t *testing.T) {
 ctx:=context.Background();dir:=t.TempDir();if err:=os.Chmod(dir,0700);err!=nil{t.Fatal(err)}
 raw,err:=storage.Open(ctx,filepath.Join(dir,"inbox.db"));if err!=nil{t.Fatal(err)};defer raw.Close()
 if _,err:=raw.Exec(`UPDATE inbox_sequence SET value=? WHERE id=1`,int64(math.MaxInt64));err!=nil{t.Fatal(err)}
 tx,err:=beginMainWrite(t,raw,ctx);if err!=nil{t.Fatal(err)};defer tx.Rollback()
 err=inbox.Insert(ctx,tx,inbox.Event{ID:"00000000-0000-4000-8000-000000000001",Kind:"alert",SourceID:"source",Transition:"firing",Severity:"warning",CreatedAt:time.Now(),Details:json.RawMessage(`{}`)})
 if err==nil{t.Fatal("exhausted sequence accepted")}
 if err:=tx.Rollback();err!=nil{t.Fatal(err)}
 var value int64;var count int
 if err:=raw.QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&value);err!=nil||value!=math.MaxInt64{t.Fatalf("value=%d err=%v",value,err)}
 if err:=raw.QueryRow(`SELECT count(*) FROM inbox_notifications`).Scan(&count);err!=nil||count!=0{t.Fatalf("count=%d err=%v",count,err)}
}
