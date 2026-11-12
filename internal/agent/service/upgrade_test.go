package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

type upgradeTransport func(*http.Request) (*http.Response, error)

func (f upgradeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func upgradeFixture(t *testing.T) (*upgrader, []byte) {
	t.Helper()
	root := t.TempDir()
	bin, state := filepath.Join(root, "bin"), filepath.Join(root, "state")
	if err := os.Mkdir(bin, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "orbitbeacon-agent")
	if err := os.WriteFile(path, []byte("current binary"), 0750); err != nil {
		t.Fatal(err)
	}
	candidate := []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"1.2.4\",\"os\":\"linux\",\"arch\":\"" + runtime.GOARCH + "\"}'\n")
	return &upgrader{base: "https://releases.example.test", stateDir: state, executable: path, version: "1.2.3", arch: runtime.GOARCH}, candidate
}
func upgradeCommand() v1.Command {
	c := testCommand("upgrade")
	c.TargetVersion = "1.2.4"
	c.ExpiresAt = time.Now().Add(20 * time.Minute)
	return c
}
func serveUpgrade(t *testing.T, u *upgrader, candidate []byte, change func(*http.Request) (*http.Response, error)) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	sum := sha256.Sum256(candidate)
	manifest := fmt.Sprintf("%x  orbitbeacon-agent-linux-%s\n", sum, u.arch)
	u.client = &http.Client{CheckRedirect: upgradeRedirect, Transport: upgradeTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Scheme != "https" {
			t.Error("download reached an HTTP redirect target")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("download leaked Authorization")
		}
		if change != nil {
			if res, err := change(r); res != nil || err != nil {
				return res, err
			}
		}
		body := candidate
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			body = []byte(manifest)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	})}
	return calls
}
func assertUnswitched(t *testing.T, u *upgrader) {
	t.Helper()
	data, err := os.ReadFile(u.executable)
	if err != nil || string(data) != "current binary" {
		t.Fatalf("current modified: %q %v", data, err)
	}
	names, _ := filepath.Glob(filepath.Join(filepath.Dir(u.executable), ".orbitbeacon-agent.*"))
	if len(names) != 0 {
		t.Fatal("leaked temps", names)
	}
	if _, err := os.Stat(filepath.Join(u.stateDir, "upgrade-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected pending marker", err)
	}
}
func TestUpgradeFilesAndFailureCodes(t *testing.T) {
	for _, scenario := range []string{"success", "checksum", "missing_line", "empty_manifest", "oversize", "manifest_oversize", "self_check", "redirect_http", "download_error"} {
		t.Run(scenario, func(t *testing.T) {
			u, candidate := upgradeFixture(t)
			if scenario == "self_check" {
				candidate = []byte("#!/bin/sh\nprintf '%s' '{\"version\":\"wrong\",\"os\":\"linux\",\"arch\":\"amd64\"}'\n")
			}
			serveUpgrade(t, u, candidate, func(r *http.Request) (*http.Response, error) {
				manifest := strings.HasSuffix(r.URL.Path, "SHA256SUMS")
				text := ""
				status := 200
				length := int64(-1)
				switch {
				case scenario == "download_error":
					return nil, errors.New("download failed")
				case scenario == "redirect_http":
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://insecure.example.test/binary"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				case scenario == "empty_manifest" && manifest:
					text = ""
				case scenario == "missing_line" && manifest:
					text = "unrelated manifest\n"
				case scenario == "checksum" && !manifest:
					text = "corrupted"
				case scenario == "oversize" && !manifest:
					text = "large"
					length = maxUpgradeBinary + 1
				case scenario == "manifest_oversize" && manifest:
					text = strings.Repeat("x", maxUpgradeManifest+1)
				default:
					return nil, nil
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(text)), ContentLength: length, Request: r}, nil
			})
			result := u.run(t.Context(), upgradeCommand())
			if scenario == "success" {
				if result.status != "succeeded" {
					t.Fatal(result)
				}
				data, err := os.ReadFile(u.executable)
				if err != nil || !bytes.Equal(data, candidate) {
					t.Fatal(string(data), err)
				}
				previous, err := os.ReadFile(filepath.Join(filepath.Dir(u.executable), "orbitbeacon-agent.prev"))
				if err != nil || string(previous) != "current binary" {
					t.Fatal(string(previous), err)
				}
				marker, err := os.ReadFile(filepath.Join(u.stateDir, "upgrade-pending"))
				if err != nil || string(marker) != "0" {
					t.Fatal(string(marker), err)
				}
				info, err := os.Stat(u.executable)
				if err != nil || info.Mode().Perm() != 0750 {
					t.Fatal(info, err)
				}
			} else {
				want := map[string]string{"checksum": "checksum", "missing_line": "checksum", "empty_manifest": "checksum", "oversize": "download", "manifest_oversize": "download", "self_check": "self_check", "redirect_http": "download", "download_error": "download"}[scenario]
				store := openCommandStore(filepath.Join(u.stateDir, "commands.json"), slog.New(slog.DiscardHandler), "1.2.3")
				command := upgradeCommand()
				if store.handle(command, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
					t.Fatal("not executing")
				}
				store.finishUpgrade(result)
				got := store.results()
				if len(got) != 1 || got[0].Status != "failed" || got[0].Code != want {
					t.Fatal(got, result)
				}
				assertUnswitched(t, u)
			}
		})
	}
}

func TestUpgradeTLSDownloads(t *testing.T) {
	u, candidate := upgradeFixture(t)
	sum := sha256.Sum256(candidate)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential sent to download host")
		}
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			fmt.Fprintf(w, "%x *orbitbeacon-agent-linux-%s\n", sum, u.arch)
		} else {
			_, _ = w.Write(candidate)
		}
	}))
	defer server.Close()
	u.base = server.URL
	u.client = server.Client()
	transport := u.client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	u.client.Transport = transport
	u.client.CheckRedirect = upgradeRedirect
	if result := u.run(t.Context(), upgradeCommand()); result.status != "succeeded" {
		t.Fatal(result)
	}
}

func TestUpgradePreflightNoNetworkAndRedirectLimit(t *testing.T) {
	for _, current := range []string{"1.2.3", "1.2.4", "development"} {
		u, candidate := upgradeFixture(t)
		u.version = current
		calls := serveUpgrade(t, u, candidate, nil)
		c := upgradeCommand()
		c.TargetVersion = "1.2.3"
		result := u.run(t.Context(), c)
		if result.status != "rejected" || result.code != "downgrade" || calls.Load() != 0 {
			t.Fatal(result, calls.Load())
		}
		assertUnswitched(t, u)
	}
	u, candidate := upgradeFixture(t)
	u.arch = "386"
	calls := serveUpgrade(t, u, candidate, nil)
	if got := u.run(t.Context(), upgradeCommand()); got.code != "unsupported" || calls.Load() != 0 {
		t.Fatal(got)
	}
	req, _ := http.NewRequest("GET", "https://release.example.test", nil)
	if upgradeRedirect(req, make([]*http.Request, 5)) != nil || upgradeRedirect(req, make([]*http.Request, 6)) == nil {
		t.Fatal("redirect hop bound")
	}
}

func TestUpgradeRecoveryExpiryAndReplay(t *testing.T) {
	for _, scenario := range []string{"success", "rollback", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			s := commandTestStore(t)
			c := upgradeCommand()
			if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
				t.Fatal("no action")
			}
			if s.state.Processed[0].TargetVersion != c.TargetVersion {
				t.Fatal("target not persisted")
			}
			if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "" {
				t.Fatal("replayed")
			}
			version := "1.2.3"
			if scenario == "success" {
				version = c.TargetVersion
			}
			if scenario == "rollback" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), "upgrade-rolled-back"), []byte("4"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			recovered := openCommandStore(s.path, s.logger, version)
			want := map[string]string{"success": "ok", "rollback": "rolled_back", "interrupted": "interrupted"}[scenario]
			if recovered.blocked || len(recovered.results()) != 1 || recovered.results()[0].Code != want {
				t.Fatal(recovered.state, recovered.blocked)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(s.path), "upgrade-rolled-back")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rollback marker retained", err)
			}
			recovered.acknowledge(recovered.results())
			if recovered.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "" {
				t.Fatal("replayed after ack")
			}
		})
	}
	for _, remaining := range []time.Duration{-time.Second, upgradeReserve - time.Second, upgradeReserve} {
		s := commandTestStore(t)
		c := upgradeCommand()
		now := time.Now()
		c.ExpiresAt = now.Add(remaining)
		action := s.handle(c, now, config.DefaultRuntimeSettings())
		if remaining >= upgradeReserve {
			if action != "upgrade" {
				t.Fatal(action)
			}
		} else if action != "" || s.results()[0].Code != "expired" {
			t.Fatal(action, s.results())
		}
	}
	s := commandTestStore(t)
	settings := config.DefaultRuntimeSettings()
	settings.RemoteCommands = []string{}
	if s.handle(upgradeCommand(), time.Now(), settings) != "" || s.results()[0].Code != "disabled_locally" {
		t.Fatal(s.state)
	}
}

func TestUpgradeRejectsInvalidRecoveredTarget(t *testing.T) {
	s := commandTestStore(t)
	c := upgradeCommand()
	s.handle(c, time.Now(), config.DefaultRuntimeSettings())
	state := s.copyState()
	state.Processed[0].TargetVersion = "../1.2.3"
	if err := s.write(state); err != nil {
		t.Fatal(err)
	}
	if !openCommandStore(s.path, s.logger, "1.2.3").blocked {
		t.Fatal("invalid target accepted")
	}
}

func TestUpgradeCanceledDownloadCleansFiles(t *testing.T) {
	u, candidate := upgradeFixture(t)
	started := make(chan struct{})
	serveUpgrade(t, u, candidate, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			return nil, nil
		}
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan upgradeOutcome, 1)
	go func() { done <- u.run(ctx, upgradeCommand()) }()
	<-started
	cancel()
	if got := <-done; got.code != "download" {
		t.Fatal(got)
	}
	assertUnswitched(t, u)
}

func TestUpgradeReportsContinueAndExit75(t *testing.T) {
	u, candidate := upgradeFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	serveUpgrade(t, u, candidate, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			return nil, nil
		}
		close(started)
		select {
		case <-release:
			return nil, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	store := openCommandStore(filepath.Join(u.stateDir, "commands.json"), slog.New(slog.DiscardHandler), "1.2.3")
	id, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(SchedulerOptions{Commands: store, Upgrade: u})
	scheduler.commandSettings = config.DefaultRuntimeSettings()
	notify := make(chan struct{}, 1)
	reports := make(chan struct{}, 8)
	command := upgradeCommand()
	scheduler.options.Report = func(context.Context, v1.Report) (v1.ReportResponse, error) {
		reports <- struct{}{}
		return v1.ReportResponse{ServerTime: time.Now(), Command: &command}, nil
	}
	scheduler.options.Identity = id
	scheduler.latest = v1.Report{Seq: 1}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.reportLoop(ctx, notify) }()
	notify <- struct{}{}
	<-reports
	<-started
	scheduler.mu.Lock()
	scheduler.latest.Seq = 2
	scheduler.mu.Unlock()
	notify <- struct{}{}
	<-reports
	released.Store(true)
	close(release)
	select {
	case err := <-done:
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 75 {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade did not exit")
	}
	if len(store.state.Processed) != 1 || store.state.Processed[0].State != "executing" {
		t.Fatal(store.state)
	}
	marker, err := os.ReadFile(filepath.Join(u.stateDir, "upgrade-pending"))
	if err != nil || string(marker) != "0" {
		t.Fatal(string(marker), err)
	}
}

func TestUpgradeMarkerClearedOnFirst200(t *testing.T) {
	for _, version := range []string{"1.2.3", "1.2.4"} {
		t.Run(version, func(t *testing.T) {
			store := commandTestStore(t)
			marker := filepath.Join(filepath.Dir(store.path), "upgrade-pending")
			if err := os.WriteFile(marker, []byte("2"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := NewScheduler(SchedulerOptions{Commands: store})
			s.latest = v1.Report{Seq: 1, System: &v1.System{AgentVersion: version}}
			calls := 0
			s.options.Report = func(context.Context, v1.Report) (v1.ReportResponse, error) {
				calls++
				if calls == 1 {
					return v1.ReportResponse{}, errors.New("retry")
				}
				return v1.ReportResponse{}, nil
			}
			notify := make(chan struct{}, 1)
			notify <- struct{}{}
			done := make(chan error, 1)
			go func() { done <- s.reportLoop(ctx, notify) }()
			deadline := time.After(4 * time.Second)
			for {
				if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
					break
				}
				select {
				case <-deadline:
					t.Fatal("marker retained after 200")
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancel()
			<-done
		})
	}
}

func TestUpgradeStaleTempsAndSelfCheckBounds(t *testing.T) {
	u, _ := upgradeFixture(t)
	stale := filepath.Join(filepath.Dir(u.executable), ".orbitbeacon-agent.stale")
	if err := os.WriteFile(stale, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupUpgradeTemps(u.executable)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, body := range []string{"#!/bin/sh\nprintf '%5000s' x\n", "#!/bin/sh\nprintf '%s' \"$HOME\"\n"} {
		path := filepath.Join(filepath.Dir(u.executable), "candidate")
		if err := os.WriteFile(path, []byte(body), 0750); err != nil {
			t.Fatal(err)
		}
		if u.selfCheck(t.Context(), path, "1.2.4") == nil {
			t.Fatal("invalid self check accepted")
		}
	}
}

func TestUpgradeSchedulerCancellationAndConcurrentCommand(t *testing.T) {
	u, candidate := upgradeFixture(t)
	started := make(chan struct{})
	calls := serveUpgrade(t, u, candidate, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			return nil, nil
		}
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	store := openCommandStore(filepath.Join(u.stateDir, "commands.json"), slog.New(slog.DiscardHandler), "1.2.3")
	s := NewScheduler(SchedulerOptions{Commands: store, Upgrade: u})
	s.commandSettings = config.DefaultRuntimeSettings()
	command := upgradeCommand()
	notify := make(chan struct{}, 1)
	reports := make(chan struct{}, 3)
	s.latest = v1.Report{Seq: 1}
	reportCount := 0
	s.options.Report = func(context.Context, v1.Report) (v1.ReportResponse, error) {
		reportCount++
		c := command
		if reportCount > 1 {
			c.ID = "00000000-0000-4000-8000-000000000002"
		}
		reports <- struct{}{}
		return v1.ReportResponse{ServerTime: time.Now(), Command: &c}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.reportLoop(ctx, notify) }()
	notify <- struct{}{}
	<-reports
	<-started
	s.mu.Lock()
	s.latest.Seq = 2
	s.mu.Unlock()
	notify <- struct{}{}
	<-reports
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not wait for download cleanup")
	}
	if calls.Load() != 2 || len(store.state.Processed) != 1 {
		t.Fatal("additional upgrade was not ignored", calls.Load(), store.state)
	}
	assertUnswitched(t, u)
}

func TestUpgradeManifestAndExecutableValidation(t *testing.T) {
	asset := "orbitbeacon-agent-linux-amd64"
	sum := strings.Repeat("a", 64)
	for _, line := range []string{sum + " " + asset + "\n" + sum + " *" + asset, sum + " " + asset + " extra", strings.Repeat("g", 64) + " " + asset, strings.Repeat("a", 63) + " " + asset} {
		if _, err := manifestChecksum([]byte(line), asset); err == nil {
			t.Fatal("accepted malformed manifest", line)
		}
	}
	for _, kind := range []string{"symlink", "directory", "missing"} {
		t.Run(kind, func(t *testing.T) {
			u, candidate := upgradeFixture(t)
			calls := serveUpgrade(t, u, candidate, nil)
			if err := os.Remove(u.executable); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink("elsewhere", u.executable); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(u.executable, 0750); err != nil {
					t.Fatal(err)
				}
			}
			if result := u.run(t.Context(), upgradeCommand()); result.status != "rejected" || result.code != "unsupported" || calls.Load() != 0 {
				t.Fatal(result, calls.Load())
			}
		})
	}
}

func TestUpgradeReportRequiresHTTP200(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		client := &Client{baseURL: "https://beacon.example.test", http: &http.Client{Transport: upgradeTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"node_id":"node","accepted_seq":1}`)), Request: r}, nil
		})}}
		var response v1.ReportResponse
		err := client.post(t.Context(), "/api/agent/v1/report", "credential", v1.Report{}, &response)
		if (err == nil) != (status == http.StatusOK) {
			t.Fatal(status, err)
		}
	}
}

func TestUpgradeTLSFailureIsolation(t *testing.T) {
	for _, scenario := range []string{"checksum", "missing_line", "oversize", "self_check", "http_redirect"} {
		t.Run(scenario, func(t *testing.T) {
			u, candidate := upgradeFixture(t)
			if scenario == "self_check" {
				candidate = []byte("#!/bin/sh\nexit 1\n")
			}
			sum := sha256.Sum256(candidate)
			var insecureRequests atomic.Int32
			insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { insecureRequests.Add(1) }))
			defer insecure.Close()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("authorization reached release host")
				}
				if scenario == "http_redirect" {
					http.Redirect(w, r, insecure.URL, http.StatusFound)
					return
				}
				if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
					if scenario == "missing_line" {
						fmt.Fprint(w, "unrelated\n")
						return
					}
					fmt.Fprintf(w, "%x  orbitbeacon-agent-linux-%s\n", sum, u.arch)
					return
				}
				if scenario == "oversize" {
					w.Header().Set("Content-Length", fmt.Sprint(maxUpgradeBinary+1))
					return
				}
				if scenario == "checksum" {
					fmt.Fprint(w, "corrupted")
					return
				}
				_, _ = w.Write(candidate)
			}))
			defer server.Close()
			u.base = server.URL
			u.client = server.Client()
			u.client.CheckRedirect = upgradeRedirect
			result := u.run(t.Context(), upgradeCommand())
			want := map[string]string{"checksum": "checksum", "missing_line": "checksum", "oversize": "download", "self_check": "self_check", "http_redirect": "download"}[scenario]
			if result.status != "failed" || result.code != want || insecureRequests.Load() != 0 {
				t.Fatal(result, insecureRequests.Load())
			}
			assertUnswitched(t, u)
		})
	}
}

func TestUpgradeClientIsIndependentAndTLS12(t *testing.T) {
	u, err := newUpgrader(config.InstallPolicy{InstallMode: "native", UpdateBaseURL: "https://release.example.test", StateDir: t.TempDir()}, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer u.client.CloseIdleConnections()
	transport, ok := u.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.RootCAs == nil || u.client.Jar != nil {
		t.Fatal("unexpected upgrade transport")
	}
	if u.client.CheckRedirect == nil {
		t.Fatal("missing redirect boundary")
	}
}

func writeUpgradeMarker(t *testing.T, s *commandStore, name string) string {
	t.Helper()
	marker := filepath.Join(filepath.Dir(s.path), name)
	if err := os.WriteFile(marker, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestUpgradeRollbackNeverInheritsSuccess(t *testing.T) {
	s := commandTestStore(t)
	c := upgradeCommand()
	if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
		t.Fatal("no action")
	}
	marker := writeUpgradeMarker(t, s, "upgrade-pending")
	for range 3 {
		started := openCommandStore(s.path, s.logger, c.TargetVersion)
		if started.blocked || len(started.results()) != 0 || started.state.Processed[0].State != "executing" {
			t.Fatal("start recorded a result", started.state)
		}
	}
	if err := os.Rename(marker, filepath.Join(filepath.Dir(s.path), "upgrade-rolled-back")); err != nil {
		t.Fatal(err)
	}
	previous := openCommandStore(s.path, s.logger, "1.2.3")
	if got := previous.results(); len(got) != 1 || got[0].Status != "failed" || got[0].Code != "rolled_back" {
		t.Fatal(got)
	}
}

func TestUpgradeSucceedsOnlyAfterAcceptedReport(t *testing.T) {
	s := commandTestStore(t)
	c := upgradeCommand()
	if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
		t.Fatal("no action")
	}
	marker := writeUpgradeMarker(t, s, "upgrade-pending")
	store := openCommandStore(s.path, s.logger, c.TargetVersion)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scheduler := NewScheduler(SchedulerOptions{Commands: store})
	scheduler.latest = v1.Report{Seq: 1}
	reports := make(chan []v1.CommandResult, 8)
	scheduler.options.Report = func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
		reports <- report.CommandResults
		return v1.ReportResponse{}, nil
	}
	notify := make(chan struct{}, 1)
	notify <- struct{}{}
	done := make(chan error, 1)
	go func() { done <- scheduler.reportLoop(ctx, notify) }()
	if first := <-reports; len(first) != 0 {
		t.Fatal("result sent before confirmation", first)
	}
	scheduler.mu.Lock()
	scheduler.latest.Seq = 2
	scheduler.mu.Unlock()
	notify <- struct{}{}
	second := <-reports
	if len(second) != 1 || second[0].Status != "succeeded" || second[0].Code != "ok" {
		t.Fatal(second)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker retained", err)
	}
	cancel()
	<-done
}

func TestUpgradeWatchdog(t *testing.T) {
	for _, scenario := range []string{"never reports", "unmarked", "confirmed"} {
		t.Run(scenario, func(t *testing.T) {
			store := commandTestStore(t)
			if scenario != "unmarked" {
				writeUpgradeMarker(t, store, "upgrade-pending")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			s := NewScheduler(SchedulerOptions{Commands: store, UpgradeConfirmTimeout: 50 * time.Millisecond})
			s.latest = v1.Report{Seq: 1}
			s.options.Report = func(context.Context, v1.Report) (v1.ReportResponse, error) {
				if scenario == "confirmed" {
					return v1.ReportResponse{}, nil
				}
				return v1.ReportResponse{}, errors.New("rejected")
			}
			notify := make(chan struct{}, 1)
			notify <- struct{}{}
			err := s.reportLoop(ctx, notify)
			var exit *ExitError
			if scenario == "never reports" {
				if !errors.As(err, &exit) || exit.Code != 75 {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpgradeBudgetFitsTheCommandLifetime(t *testing.T) {
	const restart = 10 * time.Second
	if worst := 3*upgradeConfirmTimeout + 4*restart + 10*time.Second; upgradeReserve < worst {
		t.Fatalf("reserve %v below rollback path %v", upgradeReserve, worst)
	}
	if upgradeReserve+upgradeDownloadTimeout > 20*time.Minute {
		t.Fatal("staging and rollback exceed the server lifetime")
	}
}

func TestUpgradeCrashAfterConfirmationCannotRollBackOntoSuccess(t *testing.T) {
	s := commandTestStore(t)
	c := upgradeCommand()
	if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
		t.Fatal("no action")
	}
	marker := writeUpgradeMarker(t, s, "upgrade-pending")
	store := openCommandStore(s.path, s.logger, c.TargetVersion)
	saved := false
	store.persist = func(commandState) error {
		saved = true
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("success saved while rollback is armed", err)
		}
		return errors.New("crash before save")
	}
	if !store.confirmUpgrade() || !saved {
		t.Fatal("confirmation did not reach save")
	}
	restarted := openCommandStore(s.path, s.logger, c.TargetVersion)
	if got := restarted.results(); len(got) != 1 || got[0].Status != "succeeded" {
		t.Fatal(got)
	}
}

func TestUpgradeUnremovableMarkerStaysUnconfirmed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := commandTestStore(t)
	c := upgradeCommand()
	if s.handle(c, time.Now(), config.DefaultRuntimeSettings()) != "upgrade" {
		t.Fatal("no action")
	}
	writeUpgradeMarker(t, s, "upgrade-pending")
	store := openCommandStore(s.path, s.logger, c.TargetVersion)
	dir := filepath.Dir(s.path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if store.confirmUpgrade() || len(store.results()) != 0 {
		t.Fatal("recorded success with rollback still armed", store.state)
	}
}
