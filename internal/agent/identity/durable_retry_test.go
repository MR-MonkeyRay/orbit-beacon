package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestEnrollmentRetryRequiresDirectorySyncBeforePublishing(t *testing.T) {
	for _, first := range []string{"stage", "remember"} {
		t.Run(first, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "identity.json")
			store, err := Open(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			before := store.Current()
			other, err := Open(t.Context(), path, Options{Random: bytes.NewReader(nil)})
			if err != nil {
				t.Fatal(err)
			}
			ticket := sha256.Sum256([]byte(first))
			store.syncDirectory = func(directory string) error {
				if directory != filepath.Dir(path) {
					t.Fatalf("sync directory = %q", directory)
				}
				return syscall.EIO
			}
			var got Identity
			var operation Enrollment
			if first == "stage" {
				got, operation, err = store.StageEnrollment(ctx, ticket, true)
			} else {
				got, operation, err = store.RememberEnrollment(ctx, ticket, before.NodeToken)
			}
			if !errors.Is(err, syscall.EIO) || got != (Identity{}) || operation != (Enrollment{}) {
				t.Fatalf("failed publication exposed state: identity=%+v operation=%+v err=%v", got, operation, err)
			}
			if store.Current() != before {
				t.Fatal("failed publication updated Current")
			}
			visible, data, inode := readVisibleIdentity(t, path)
			candidate, exists := visible.EnrollmentAttempts[hex.EncodeToString(ticket[:])]
			if !exists || candidate.NodeToken == "" {
				t.Fatal("rename did not publish the attempted operation")
			}
			if first == "stage" && (candidate.NodeToken == before.NodeToken || visible.PendingNodeToken != candidate.NodeToken) {
				t.Fatal("rename did not publish the rotation candidate")
			}
			// 另一个 Store 实例必须读取实际文件并确认其已持久化；
			// 不能把另一实例替换后可见的文件视为已持久化。
			other.syncDirectory = store.syncDirectory
			for _, reader := range []*Store{store, other} {
				for range 2 {
					got, operation, exists, err = reader.LookupEnrollment(ctx, ticket)
					if !errors.Is(err, syscall.EIO) || got != (Identity{}) || operation != (Enrollment{}) || exists {
						t.Fatalf("lookup published uncertain operation: identity=%+v operation=%+v exists=%v err=%v", got, operation, exists, err)
					}
					got, operation, exists, err = reader.LookupEnrollment(ctx, [32]byte{})
					if !errors.Is(err, syscall.EIO) || got != (Identity{}) || operation != (Enrollment{}) || exists {
						t.Fatalf("missing-operation lookup published uncertain identity: identity=%+v exists=%v err=%v", got, exists, err)
					}
					got, operation, err = reader.StageEnrollment(ctx, ticket, true)
					if !errors.Is(err, syscall.EIO) || got != (Identity{}) || operation != (Enrollment{}) {
						t.Fatalf("stage retry bypassed sync: identity=%+v operation=%+v err=%v", got, operation, err)
					}
					got, operation, err = reader.RememberEnrollment(ctx, ticket, candidate.NodeToken)
					if !errors.Is(err, syscall.EIO) || got != (Identity{}) || operation != (Enrollment{}) {
						t.Fatalf("remember retry bypassed sync: identity=%+v operation=%+v err=%v", got, operation, err)
					}
					if got, err = reader.Reload(ctx); !errors.Is(err, syscall.EIO) || got != (Identity{}) {
						t.Fatalf("reload published uncertain identity: identity=%+v err=%v", got, err)
					}
					if reader.Current() != before {
						t.Fatal("failed retry changed cached identity")
					}
					assertIdentityUnchanged(t, path, data, inode)
				}
			}
			for _, reader := range []*Store{store, other} {
				reader.syncDirectory = syncIdentityDirectory
				// 没有可用熵：恢复必须复用磁盘上的操作。
				reader.random = bytes.NewReader(nil)
				got, operation, err = reader.StageEnrollment(ctx, ticket, true)
				if err != nil || got != visible.Identity || operation != candidate || reader.Current() != visible.Identity {
					t.Fatalf("stage did not recover exact visible operation: identity=%+v operation=%+v err=%v", got, operation, err)
				}
				got, operation, err = reader.RememberEnrollment(ctx, ticket, candidate.NodeToken)
				if err != nil || got != visible.Identity || operation != candidate {
					t.Fatalf("remember did not recover exact operation: %v", err)
				}
				got, operation, exists, err = reader.LookupEnrollment(ctx, ticket)
				if err != nil || !exists || got != visible.Identity || operation != candidate {
					t.Fatalf("lookup did not recover exact operation: exists=%v err=%v", exists, err)
				}
				if got, err = reader.Reload(ctx); err != nil || got != visible.Identity {
					t.Fatalf("reload did not recover visible identity: %v", err)
				}
				assertIdentityUnchanged(t, path, data, inode)
			}
			reopened, err := Open(t.Context(), path, Options{Random: bytes.NewReader(nil)})
			if err != nil || reopened.Current() != visible.Identity {
				t.Fatalf("Open did not recover the visible identity without entropy: %v", err)
			}
			assertIdentityUnchanged(t, path, data, inode)
		})
	}
}

func TestConfirmEnrollmentRetrySyncsWithoutReplacingVisibleIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	store.syncDirectory = func(string) error { return syscall.EIO }
	if err := store.ConfirmEnrollment(ctx, initial.NodeToken, "node"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("confirmation ignored sync failure: %v", err)
	}
	visible, data, inode := readVisibleIdentity(t, path)
	if visible.NodeID != "node" || store.Current() != initial {
		t.Fatal("failed confirmation was not visible on disk only")
	}
	for range 2 {
		if err := store.ConfirmEnrollment(ctx, initial.NodeToken, "node"); !errors.Is(err, syscall.EIO) {
			t.Fatalf("confirmation retry ignored sync failure: %v", err)
		}
		if store.Current() != initial {
			t.Fatal("failed confirmation retry changed Current")
		}
		assertIdentityUnchanged(t, path, data, inode)
	}
	store.syncDirectory = syncIdentityDirectory
	if err := store.ConfirmEnrollment(ctx, initial.NodeToken, "node"); err != nil || store.Current() != visible.Identity {
		t.Fatalf("confirmation recovery failed: %v", err)
	}
	assertIdentityUnchanged(t, path, data, inode)
}

func TestBeginRunSyncFailureDoesNotPublishVisibleGeneration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	store.syncDirectory = func(string) error { return syscall.EIO }
	if got, err := store.BeginRun(ctx); !errors.Is(err, syscall.EIO) || got != (Identity{}) {
		t.Fatalf("run published before directory sync: identity=%+v err=%v", got, err)
	}
	visible, data, inode := readVisibleIdentity(t, path)
	if visible.RunGeneration != initial.RunGeneration+1 || visible.SessionID == "" || store.Current() != initial {
		t.Fatal("failed run was not visible on disk only")
	}
	if got, err := store.Reload(ctx); !errors.Is(err, syscall.EIO) || got != (Identity{}) || store.Current() != initial {
		t.Fatalf("reload exposed failed run: identity=%+v err=%v", got, err)
	}
	store.syncDirectory = syncIdentityDirectory
	if got, err := store.Reload(ctx); err != nil || got != visible.Identity || store.Current() != visible.Identity {
		t.Fatalf("reload did not recover visible run: identity=%+v err=%v", got, err)
	}
	assertIdentityUnchanged(t, path, data, inode)
}

func readVisibleIdentity(t *testing.T, path string) (diskIdentity, []byte, os.FileInfo) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var visible diskIdentity
	if err := json.Unmarshal(data, &visible); err != nil {
		t.Fatal(err)
	}
	inode, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return visible, data, inode
}

func assertIdentityUnchanged(t *testing.T, path string, before []byte, inode os.FileInfo) {
	t.Helper()
	_, after, info := readVisibleIdentity(t, path)
	if !bytes.Equal(before, after) || !os.SameFile(inode, info) {
		t.Fatal("retry rewrote the already visible identity")
	}
}
