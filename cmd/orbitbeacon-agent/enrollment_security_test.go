package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestEnrollWithoutTokenFileReachesRuntimePolicyBoundary(t *testing.T) {
	err := run([]string{"enroll"})
	if err == nil || !strings.Contains(err.Error(), "--install-policy") {
		t.Fatalf("ticketless enrollment must reach policy/runtime setup, not require --token-file: %v", err)
	}
}

// runEnrollment is the command's enrollment operation, after policy and runtime
// identity validation. Its callback is Runtime.Enroll in the real command.
func TestEnrollmentCommandRecoversWithoutReadingTicketFile(t *testing.T) {
	called := false
	result, err := runEnrollment(context.Background(), "", func(_ context.Context, token string) (v1.EnrollmentResponse, error) {
		called = true
		if token != "" {
			t.Fatalf("ticketless recovery passed a ticket: %q", token)
		}
		return v1.EnrollmentResponse{NodeID: "recovered-node"}, nil
	})
	if err != nil || !called || result.NodeID != "recovered-node" {
		t.Fatalf("command did not return runtime recovery: result=%+v called=%v err=%v", result, called, err)
	}
}

func TestEnrollmentCommandRemovesTokenOnlyAfterConfirmedSuccess(t *testing.T) {
	for _, outcome := range []string{"success", "network failure", "credential rejection", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "enrollment.token")
			const secret = "private-enrollment-ticket"
			if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			var wantErr error
			if outcome != "success" {
				wantErr = errors.New(outcome)
			}
			_, err = runEnrollment(context.Background(), path, func(_ context.Context, token string) (v1.EnrollmentResponse, error) {
				if token != secret {
					t.Fatalf("runtime received wrong ticket: %q", token)
				}
				current, statErr := os.Lstat(path)
				if statErr != nil || !os.SameFile(before, current) {
					t.Fatalf("ticket was removed before enrollment was confirmed: %v", statErr)
				}
				return v1.EnrollmentResponse{NodeID: "confirmed-node"}, wantErr
			})
			if wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("confirmed enrollment retained its plaintext ticket: %v", err)
				}
				return
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("command hid enrollment failure: got %v want %v", err, wantErr)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("failure removed or replaced ticket: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != secret+"\n" {
				t.Fatalf("failure changed ticket contents: %q %v", data, err)
			}
		})
	}
}

func TestEnrollmentCommandNeverUnlinksReplacementTokenPath(t *testing.T) {
	for _, replacement := range []string{"regular file", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "enrollment.token")
			saved := filepath.Join(dir, "original.token")
			victim := filepath.Join(dir, "victim.token")
			if err := os.WriteFile(path, []byte("original-ticket"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(victim, []byte("attacker-chosen-file"), 0600); err != nil {
				t.Fatal(err)
			}
			var replacementInfo os.FileInfo
			_, _ = runEnrollment(context.Background(), path, func(_ context.Context, token string) (v1.EnrollmentResponse, error) {
				if token != "original-ticket" {
					t.Fatalf("wrong ticket read: %q", token)
				}
				if err := os.Rename(path, saved); err != nil {
					t.Fatal(err)
				}
				if replacement == "symlink" {
					if err := os.Symlink(victim, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("replacement-ticket"), 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				replacementInfo, err = os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				return v1.EnrollmentResponse{NodeID: "confirmed-node"}, nil
			})
			got, err := os.Lstat(path)
			if err != nil || !os.SameFile(replacementInfo, got) {
				t.Fatalf("cleanup unlinked an attacker replacement instead of the opened ticket: %v", err)
			}
			data, err := os.ReadFile(victim)
			if err != nil || string(data) != "attacker-chosen-file" {
				t.Fatalf("cleanup damaged symlink target: %q %v", data, err)
			}
		})
	}
}

func TestEnrollmentCommandRejectsUnsafeTicketBeforeRuntimeCall(t *testing.T) {
	for _, kind := range []string{"wrong mode", "symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ticket")
			if kind != "missing" {
				if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "wrong mode" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				link := path + ".link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			called := false
			_, err := runEnrollment(context.Background(), path, func(context.Context, string) (v1.EnrollmentResponse, error) {
				called = true
				return v1.EnrollmentResponse{NodeID: "must-not-enroll"}, nil
			})
			if err == nil || called {
				t.Fatalf("unsafe ticket reached runtime: called=%v err=%v", called, err)
			}
		})
	}
}
