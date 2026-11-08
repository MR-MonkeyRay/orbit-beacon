package storage

import (
 "errors"
 "os"
 "path/filepath"
 "testing"
 "time"

 "github.com/google/uuid"
 "orbitbeacon/internal/config"
)

func TestManualRecoveryDoesNotEnrollUnregisteredInactive(t *testing.T){
 ctx:=t.Context();dir:=t.TempDir();if err:=os.Chmod(dir,0700);err!=nil{t.Fatal(err)}
 r,err:=openControl(ctx,dir,true);if err!=nil{t.Fatal(err)}
 r.cfg=config.DatabaseConfig{Primary:config.DatabaseEndpoint{Driver:"sqlite"},Standby:&config.DatabaseEndpoint{Driver:"sqlite"}}
 m:=&memberRecord{Resource:uuid.NewString(),Primary:uuid.NewString(),Standby:uuid.NewString(),Epoch:uuid.NewString(),Active:"primary",State:"initializing",Generation:1}
 r.record=controlRecord{Cluster:uuid.NewString(),Binding:configBinding(r.cfg),BootstrapPending:true,Domains:map[Domain]*memberRecord{MainDomain:m}}
 if err=r.persistLocked(ctx);err!=nil{t.Fatal(err)}
 dbs:=&Databases{replication:r};defer dbs.Close()
 if err=r.openRegistered(ctx,dbs,[]Domain{MainDomain});err!=nil{t.Fatal(err)}
 if err=dbs.CompleteBootstrap(ctx);err!=nil{t.Fatal(err)}
 d:=dbs.Main.replica
 inactive:=t.TempDir();if err=os.Chmod(inactive,0700);err!=nil{t.Fatal(err)};r.dataDir=inactive
 if err=d.update(ctx,func(m *memberRecord){m.FailureSince=time.Now().UTC().Add(-25*time.Hour)});err!=nil{t.Fatal(err)}
 if err=d.checkManual(ctx);err!=nil{t.Fatal(err)}
 if !m.Manual||m.StandbyRegistered{t.Fatal("fixture did not retain an unregistered target under its real deadline lock")}
 candidate,err:=d.openMember(ctx,"standby");if candidate!=nil{candidate.Close()}
 if !errors.Is(err,ErrUnavailable){t.Fatal("manual recovery allowed automatic inactive enrollment",err)}
 if _,err=os.Stat(filepath.Join(inactive,"orbitbeacon.db"));!os.IsNotExist(err){t.Fatal("manual validation created an inactive database",err)}
 if !m.Manual||m.StandbyRegistered||!dbs.Main.Ready(){t.Fatal("readonly validation changed authorization or active service")}
}
