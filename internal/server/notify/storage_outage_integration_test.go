package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/timekey"
)

func outageTLSMailbox(t *testing.T) (Config, *httptest.Server, <-chan string) {
	t.Helper()
	certificate := httptest.NewTLSServer(nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan string, 64)
	var connections sync.WaitGroup
	accepting := make(chan struct{})
	t.Cleanup(func() { listener.Close(); <-accepting; connections.Wait(); certificate.Close() })
	go func() {
		defer close(accepting)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func(conn net.Conn) {
				defer connections.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(12 * time.Second))
				reader := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 storage SMTP\r\n")
				secure := false
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO "):
						fmt.Fprint(conn, "250-local\r\n250 STARTTLS\r\n")
					case line == "STARTTLS\r\n":
						fmt.Fprint(conn, "220 TLS\r\n")
						encrypted := tls.Server(conn, certificate.TLS)
						if encrypted.Handshake() != nil {
							return
						}
						conn = encrypted
						reader = bufio.NewReader(conn)
						secure = true
					case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
						if !secure {
							t.Error("mail without TLS")
							return
						}
						fmt.Fprint(conn, "250 OK\r\n")
					case line == "DATA\r\n":
						fmt.Fprint(conn, "354 body\r\n")
						var raw strings.Builder
						for {
							part, err := reader.ReadString('\n')
							if err != nil {
								return
							}
							if part == ".\r\n" {
								break
							}
							raw.WriteString(part)
						}
						messages <- raw.String()
						fmt.Fprint(conn, "250 queued\r\n")
					case line == "QUIT\r\n":
						fmt.Fprint(conn, "221 bye\r\n")
						return
					default:
						fmt.Fprint(conn, "500 invalid\r\n")
					}
				}
			}(connection)
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return Config{Host: host, Port: number, From: "storage@example.com", To: []string{"admin@example.com"}}, certificate, messages
}

func TestMySQLStorageOutageSendsTLSHTTPAndMailWithoutMainThenProjectsOnce(t *testing.T) {
	f := storagetest.Start(t)
	primaryProxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standbyProxy, standby := storagetest.ProxyEndpoint(t, f.Endpoint("standby"))
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Minute, ManualAfter: 4 * time.Second}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	events := make(chan StoragePayload, 64)
	httpReceiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p StoragePayload
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			t.Error("invalid payload")
		}
		events <- p
		w.WriteHeader(204)
	}))
	defer httpReceiver.Close()
	mailConfig, mailCertificate, messages := outageTLSMailbox(t)
	box, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	storageBox, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon storage notifications v1")
	s := New(dbs.Main, box, Options{Storage: dbs, StorageBox: storageBox, AllowPrivateNetworks: true, Now: time.Now})
	roots := x509.NewCertPool()
	roots.AddCert(httpReceiver.Certificate())
	roots.AddCert(mailCertificate.Certificate())
	s.sender.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	s.sender.client.Transport.(*http.Transport).TLSClientConfig = s.sender.tlsConfig
	enabled := true
	webhook, name := "webhook", "outage-webhook"
	raw, _ := json.Marshal(Config{URL: httpReceiver.URL + "/credential"})
	web, err := s.Save(ctx, "", Patch{Name: &name, Kind: &webhook, Enabled: &enabled, Config: raw})
	if err != nil {
		t.Fatal(err)
	}
	email, name := "email", "outage-mail"
	raw, _ = json.Marshal(mailConfig)
	mail, err := s.Save(ctx, "", Patch{Name: &name, Kind: &email, Enabled: &enabled, Config: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateStorageNotificationSettings(ctx, storage.StorageNotificationPreferences{Inbox: true, ChannelIDs: []string{web.ID, mail.ID}}); err != nil {
		t.Fatal(err)
	}
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, e := dbs.RecoveryStatus(ctx)
		caught := e == nil && len(status.Domains) == 2
		for _, domain := range status.Domains {
			caught = caught && domain.ReplicationStatus == "caught_up"
		}
		if caught {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replicas did not catch up", e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	primaryProxy.Pause()
	standbyProxy.Pause()
	defer primaryProxy.Resume()
	defer standbyProxy.Resume()
	failed, readErr := dbs.Main.Acquire(ctx, storage.Reader)
	if readErr == nil {
		var one int
		readErr = failed.QueryRowContext(failed.Context(), "SELECT 1").Scan(&one)
		failed.Close()
	}
	if readErr == nil || dbs.Main.Ready() {
		t.Fatal("primary fault did not close real SQL admission")
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.RunStorage(run) }()
	defer func() { stop(); <-done }()
	var manual StoragePayload
	for manual.Transition != "manual_required" {
		select {
		case event := <-events:
			if event.Transition == "manual_required" && event.Domain == "main" {
				manual = event
			}
		case <-ctx.Done():
			t.Fatal("no manual-required HTTP notification during full outage")
		}
	}
	mailReceived := false
	for !mailReceived {
		select {
		case raw := <-messages:
			parts := strings.SplitN(raw, "\r\n\r\n", 2)
			if len(parts) != 2 {
				continue
			}
			body, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(parts[1], "\r\n", ""))
			mailReceived = e == nil && strings.Contains(string(body), "manual_required") && strings.Contains(string(body), "Domain: main")
		case <-ctx.Done():
			t.Fatal("no manual-required TLS mail during full outage")
		}
	}
	if dbs.Main.Ready() {
		t.Fatal("test sent while main was still serving")
	}
	stop()
	<-done
	projectedAt := manual.OccurredAt.Add(91 * 24 * time.Hour)
	s.opts.Now = func() time.Time { return projectedAt }
	primaryProxy.Resume()
	standbyProxy.Resume()
	deadline = time.Now().Add(30 * time.Second)
	for !dbs.Main.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("main did not recover")
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.projectStorage(ctx)
	s.projectStorage(ctx)
	if _, err = inbox.New(dbs.Main).Cleanup(ctx, projectedAt.Add(-90*24*time.Hour), 500); err != nil {
		t.Fatal(err)
	}
	lease, err := dbs.Main.Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var count int
	if err = lease.QueryRowContext(lease.Context(), `SELECT count(*) FROM inbox_notifications WHERE kind='storage' AND source_id=? AND transition='manual_required'`, manual.EventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("projected=%d err=%v", count, err)
	}
	var created, rawDetails string
	if err = lease.QueryRowContext(lease.Context(), `SELECT created_at,details_json FROM inbox_notifications WHERE kind='storage' AND source_id=? AND transition='manual_required'`, manual.EventID).Scan(&created, &rawDetails); err != nil {
		t.Fatal(err)
	}
	createdAt, err := timekey.Parse(created)
	if err != nil || !createdAt.Equal(projectedAt) {
		t.Fatal("late projection did not retain its actual creation time", err)
	}
	var details struct {
		OccurredAt time.Time `json:"occurred_at"`
	}
	if err = json.Unmarshal([]byte(rawDetails), &details); err != nil || !details.OccurredAt.Equal(manual.OccurredAt) {
		t.Fatal("late projection changed the original occurrence time", err)
	}
}
