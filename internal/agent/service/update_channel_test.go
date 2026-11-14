package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

func TestUpdateDiscoveryUsesOnlyLocalSourceURL(t *testing.T) {
	for _, test := range []struct {
		base, endpoint, channel, body string
	}{
		{"https://github.com/mr-monkeyray/orbit-beacon/releases/download", "https://api.github.com/repos/mr-monkeyray/orbit-beacon/releases/latest", "github", `{"tag_name":"v1.2.4","draft":false,"prerelease":false,"assets":[{"browser_download_url":"https://attacker.test/binary"}]}`},
		{"https://github.com/another-owner/another-agent/releases/download/", "https://api.github.com/repos/another-owner/another-agent/releases/latest", "github", `{"tag_name":"v1.2.4"}`},
		{"https://releases.example.test", "https://releases.example.test/latest.json", "custom", `{"version":"1.2.4","url":"https://attacker.test/binary"}`},
		{"https://github.com.example.test/owner/repo/releases/download", "https://github.com.example.test/owner/repo/releases/download/latest.json", "custom", `{"version":"1.2.4"}`},
	} {
		t.Run(test.base, func(t *testing.T) {
			u, _ := upgradeFixture(t)
			u.base = test.base
			u.client = &http.Client{Transport: upgradeTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != test.endpoint || r.Header.Get("Authorization") != "" {
					t.Fatalf("unexpected release request: %s %v", r.URL, r.Header)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(test.body)), Request: r}, nil
			})}
			result := u.check(t.Context(), testCommand("check_update"))
			if result.status != "succeeded" || result.ready || result.update == nil || result.update.LatestVersion != "1.2.4" || result.update.Channel != test.channel {
				t.Fatal(result)
			}
			assertUnswitched(t, u)
		})
	}
}

func TestUpdateDiscoveryRejectsMalformedReleaseWithoutReplacingBinary(t *testing.T) {
	for _, body := range []string{`{}`, `{"version":"../1.2.4"}`, `{"version":"v1.2.4"}`, `{"version":"1.2.4-rc1"}`, `{"version":"1.2.4"} trailing`} {
		t.Run(fmt.Sprint(body), func(t *testing.T) {
			u, _ := upgradeFixture(t)
			u.client = &http.Client{Transport: upgradeTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Request: r}, nil
			})}
			result := u.check(t.Context(), upgradeCommand())
			if result.status != "failed" || result.code != "download" {
				t.Fatal(result)
			}
			assertUnswitched(t, u)
		})
	}
}

func TestUpdateInfoRoundtripValidation(t *testing.T) {
	info := &v1.UpdateInfo{Channel: "custom", CurrentVersion: "1.2.3", LatestVersion: "1.2.4"}
	if info.Validate() == nil {
		t.Fatal("missing check time accepted")
	}
}

func fixtureUpdateInfo() *v1.UpdateInfo {
	return &v1.UpdateInfo{Channel: "custom", CurrentVersion: "1.2.3", LatestVersion: "1.2.4", CheckedAt: time.Now().UTC()}
}

func runUpgrade(ctx context.Context, u *upgrader, c v1.Command) upgradeOutcome {
	result := u.check(ctx, c)
	if result.ready {
		return u.install(ctx, c, result.update)
	}
	return result
}

func TestUpdateCurrentVersionDoesNotInstallOrRestart(t *testing.T) {
	for _, latest := range []string{"1.2.3", "1.2.2"} {
		u, _ := upgradeFixture(t)
		u.client = &http.Client{Transport: upgradeTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != u.base+"/latest.json" {
				t.Fatalf("unexpected binary request: %s", r.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"version":"` + latest + `"}`)), Request: r}, nil
		})}
		result := runUpgrade(t.Context(), u, upgradeCommand())
		wantStatus, wantCode := "succeeded", "ok"
		if latest == "1.2.2" {
			wantStatus, wantCode = "rejected", "downgrade"
		}
		if result.status != wantStatus || result.code != wantCode || result.restart || result.update.LatestVersion != latest {
			t.Fatal(result)
		}
		assertUnswitched(t, u)
	}
}

func TestUpdateSelectionPersistenceFailureNeverDownloadsBinary(t *testing.T) {
	u, candidate := upgradeFixture(t)
	calls := serveUpgrade(t, u, candidate, nil)
	store := openCommandStore(filepath.Join(u.stateDir, "commands.json"), slog.New(slog.DiscardHandler), "1.2.3")
	command := upgradeCommand()
	scheduler := NewScheduler(SchedulerOptions{Commands: store, Upgrade: u})
	scheduler.commandSettings = config.DefaultRuntimeSettings()
	scheduler.latest = v1.Report{Seq: 1}
	scheduler.options.Report = func(context.Context, v1.Report) (v1.ReportResponse, error) {
		return v1.ReportResponse{ServerTime: time.Now(), Command: &command}, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	writes := 0
	store.persist = func(state commandState) error {
		writes++
		if writes == 2 {
			cancel()
			return errors.New("disk full")
		}
		return store.write(state)
	}
	notify := make(chan struct{}, 1)
	notify <- struct{}{}
	if err := scheduler.reportLoop(ctx, notify); err != nil {
		t.Fatal(err)
	}
	if writes != 2 || !store.blocked || calls.Load() != 1 {
		t.Fatal("selected target was not fenced", writes, store.blocked, calls.Load())
	}
	assertUnswitched(t, u)
}

func TestSchedulerReportsCheckAndNoUpdateWithoutRestart(t *testing.T) {
	for _, kind := range []string{"check_update", "upgrade"} {
		t.Run(kind, func(t *testing.T) {
			u, candidate := upgradeFixture(t)
			if kind == "upgrade" {
				u.version = "1.2.4"
			}
			calls := serveUpgrade(t, u, candidate, nil)
			store := openCommandStore(filepath.Join(u.stateDir, "commands.json"), slog.New(slog.DiscardHandler), u.version)
			finished := make(chan struct{}, 1)
			store.persist = func(state commandState) error {
				if err := store.write(state); err != nil {
					return err
				}
				if len(state.Pending) > 0 {
					finished <- struct{}{}
				}
				return nil
			}
			scheduler := NewScheduler(SchedulerOptions{Commands: store, Upgrade: u})
			scheduler.commandSettings = config.DefaultRuntimeSettings()
			scheduler.latest = v1.Report{Seq: 1}
			command := upgradeCommand()
			command.Kind = kind
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			results := make(chan []v1.CommandResult, 1)
			scheduler.options.Report = func(_ context.Context, report v1.Report) (v1.ReportResponse, error) {
				if len(report.CommandResults) > 0 {
					results <- report.CommandResults
					cancel()
				}
				return v1.ReportResponse{ServerTime: time.Now(), Command: &command}, nil
			}
			notify := make(chan struct{}, 1)
			notify <- struct{}{}
			done := make(chan error, 1)
			go func() { done <- scheduler.reportLoop(ctx, notify) }()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("check never produced a durable result")
			}
			scheduler.mu.Lock()
			scheduler.latest.Seq = 2
			scheduler.mu.Unlock()
			notify <- struct{}{}
			if err := <-done; err != nil {
				t.Fatal("check/no-op requested restart", err)
			}
			select {
			case result := <-results:
				if len(result) != 1 || result[0].Update == nil || result[0].Update.LatestVersion != "1.2.4" || result[0].Update.CurrentVersion != u.version || result[0].Status != "succeeded" {
					t.Fatal(result)
				}
			default:
				t.Fatal("check result was not reported")
			}
			recovered := openCommandStore(store.path, store.logger, u.version)
			if recovered.blocked || len(recovered.results()) != 1 || recovered.results()[0].Update == nil || calls.Load() != 1 {
				t.Fatal("discovery info was lost or binary downloaded", recovered.state, calls.Load())
			}
			assertUnswitched(t, u)
		})
	}
}
