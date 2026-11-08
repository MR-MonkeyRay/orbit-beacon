package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLStartupPublicReadFailureKeepsOnlyRegisteredHARecoveryAlive(t *testing.T) {
	for _, ha := range []bool{true, false} {
		name := "standalone"
		if ha {
			name = "registered_ha"
		}
		t.Run(name, func(t *testing.T) {
			f := storagetest.Start(t)
			proxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
			cfg := config.DatabaseConfig{Primary: primary}
			if ha {
				standby := f.Endpoint("standby")
				cfg.Standby = &standby
				cfg.Failover = config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Minute, ManualAfter: time.Hour}
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()
			dbs, err := storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer dbs.Close()
			a := auth.NewService(dbs.Main, auth.Options{})
			if _, err = a.CreateUser(ctx, "constructor-admin", "constructor-fixture-password", false); err != nil {
				t.Fatal(err)
			}
			m := management.NewService(dbs.Main)
			if _, err = m.SetPublicPageEnabled(ctx, false); err != nil {
				t.Fatal(err)
			}
			if ha {
				if err = dbs.CompleteBootstrap(ctx); err != nil {
					t.Fatal(err)
				}
				if err = dbs.StartReplication(ctx); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(30 * time.Second)
				for {
					raw, readErr := dbs.CaptureRestoreMetadata(ctx)
					var record struct {
						Domains map[storage.Domain]struct {
							Ready   bool
							Rebuild *struct{}
						}
					}
					if readErr == nil && json.Unmarshal(raw, &record) == nil && record.Domains[storage.MainDomain].Ready && record.Domains[storage.MainDomain].Rebuild == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("registered startup fixture never completed seed", readErr)
					}
					time.Sleep(20 * time.Millisecond)
				}
				lease, acquireErr := dbs.Main.Acquire(ctx, storage.Writer)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				barrier, beginErr := lease.BeginWrite(lease.Context())
				if beginErr != nil {
					lease.Close()
					t.Fatal(beginErr)
				}
				commitErr := barrier.Commit()
				lease.Close()
				if commitErr != nil {
					t.Fatal(commitErr)
				}
				source, target := f.RawEndpoint(t, "primary", storage.MainDomain), f.RawEndpoint(t, "standby", storage.MainDomain)
				var epoch, head string
				var seq int64
				if err = source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state WHERE id=1").Scan(&epoch, &seq, &head); err != nil {
					t.Fatal(err)
				}
				deadline = time.Now().Add(20 * time.Second)
				for {
					var count int
					readErr := target.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=? AND boot_id<>'' AND barrier_ns>0", epoch, seq, head).Scan(&count)
					if readErr == nil && count == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("constructor recovery candidate lacked real post-seed barrier", readErr)
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err = dbs.StopReplication(); err != nil {
					t.Fatal(err)
				}
			}
			opts := testHandlerOptions(t, Options{Auth: a, Nodes: nodes.NewService(dbs.Main, nodes.Options{}), Management: m, Ingest: ingest.NewService(dbs.Main, ingest.Options{}), PublicURL: "https://beacon.test", RecoverUnavailableMain: ha})
			_, reader, ok := dbs.Main.Stats()
			if !ok || reader.MaxOpenConnections < 1 {
				t.Fatal("missing bounded real reader pool")
			}
			var held []struct {
				lease *storage.Lease
				rows  *storage.Rows
			}
			release := func() {
				for _, item := range held {
					item.rows.Close()
					item.lease.Close()
				}
				held = nil
			}
			defer release()
			for range reader.MaxOpenConnections {
				lease, acquireErr := dbs.Main.Acquire(ctx, storage.Reader)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				rows, queryErr := lease.QueryContext(lease.Context(), "SELECT 1")
				if queryErr != nil {
					lease.Close()
					t.Fatal(queryErr)
				}
				held = append(held, struct {
					lease *storage.Lease
					rows  *storage.Rows
				}{lease, rows})
			}
			if !dbs.Main.Ready() {
				t.Fatal("fixture lost readiness before constructor read")
			}
			_, before, _ := dbs.Main.Stats()
			type result struct {
				handler *handler
				err     error
			}
			finished := make(chan result, 1)
			go func() { h, buildErr := NewHandler(opts); finished <- result{h, buildErr} }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, stats, _ := dbs.Main.Stats()
				if stats.WaitCount > before.WaitCount {
					break
				}
				select {
				case early := <-finished:
					t.Fatal("constructor did not wait at real public-settings SQL", early.err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("constructor never reached saturated reader pool")
				}
				time.Sleep(time.Millisecond)
			}
			proxy.Pause()
			defer proxy.Resume()
			release()
			var built result
			select {
			case built = <-finished:
			case <-time.After(15 * time.Second):
				t.Fatal("constructor read did not exit after network failure")
			}
			if !ha {
				if built.err == nil {
					t.Fatal("standalone public-settings failure was hidden")
				}
				return
			}
			if built.err != nil {
				t.Fatalf("registered HA startup died before coordinator recovery: %v", built.err)
			}
			h := built.handler
			if !h.publicPageUnknown.Load() {
				t.Fatal("failed constructor read defaulted to public")
			}
			for _, route := range []struct {
				path   string
				status int
			}{{"/login", 200}, {"/healthz/live", 200}, {"/healthz/ready", 503}, {"/api/public/v1/nodes", 503}} {
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route.path, nil))
				if response.Code != route.status {
					t.Fatalf("startup recovery %s=%d want%d", route.path, response.Code, route.status)
				}
			}
			dbs.SetMainSwitchHooks(storage.SwitchHooks{Prepare: h.PrepareDatabaseSwitch, Publish: h.PublishDatabaseGeneration})
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(45 * time.Second)
			for !dbs.Main.Ready() {
				if time.Now().After(deadline) {
					t.Fatal("coordinator never published candidate after constructor outage")
				}
				time.Sleep(20 * time.Millisecond)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
			if response.Code != http.StatusForbidden {
				t.Fatalf("recovered candidate disabled setting not loaded: %d", response.Code)
			}
		})
	}
}
