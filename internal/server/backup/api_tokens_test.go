package backup

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestAPITokensExcludedAndRestoreRevokesOldTokens(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	// 固定的有效凭据分别存在于来源和目标，验证两者都不能穿越恢复。
	token := "obt_" + strings.Repeat("A", 43)
	digest := sha256.Sum256([]byte(token))
	seed := func(dir string) {
		t.Helper()
		db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = db.Exec(`INSERT INTO api_tokens(id,name,token_hash,created_by,created_at) VALUES(?,?,?,?,?)`, restorePlan, "restore-token", digest[:], restoreUser, timekey.Format(time.Now())); err != nil {
			t.Fatal(err)
		}
		if err = auth.NewService(db, auth.Options{}).AuthenticateAPIToken(ctx, token); err != nil {
			t.Fatal("seed token invalid", err)
		}
	}
	seed(source)
	for _, category := range []Category{Configuration, Monitoring, Events} {
		t.Run(string(category), func(t *testing.T) {
			target := restoreFixture(t, "target")
			seed(target)
			archive, password := restoreArchive(t, source, []Category{category})
			if _, err := Restore(ctx, target, archive, password, true); err != nil {
				t.Fatal(err)
			}
			restored := restoreDB(t, target)
			var count int
			if err := restored.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&count); err != nil || count != 0 {
				t.Fatal("tokens retained", count, err)
			}
			if err := auth.NewService(restored, auth.Options{}).AuthenticateAPIToken(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatal("old token survived restore", err)
			}
		})
	}
}
