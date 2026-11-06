package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
)

func TestTOTPPayloadValidationAndProjection(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{9}, 32)
	box, err := secretbox.New(key, "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Seal("totp:"+validationID, []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	source := validationDB(t, Configuration)
	if _, err = source.Exec(`UPDATE users SET totp_ciphertext=?,totp_pending_ciphertext=?,totp_pending_at=?,totp_last_step=123`, cipher, cipher, validationTime); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("recovery"))
	if _, err = source.Exec(`INSERT INTO recovery_codes VALUES(?,?)`, validationID, hash[:]); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePayloadData(ctx, source, Configuration, key); err != nil {
		t.Fatal(err)
	}
	dest := validationDB(t, Configuration)
	dest.Exec(`DELETE FROM users`)
	if err = projectCategory(ctx, dest, source, nil, Configuration, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePayloadData(ctx, dest, Configuration, key); err != nil {
		t.Fatal(err)
	}
	var got []byte
	var step int64
	if err = dest.QueryRow(`SELECT totp_ciphertext,totp_last_step FROM users`).Scan(&got, &step); err != nil || !bytes.Equal(got, cipher) || step != 123 {
		t.Fatal("projection mismatch", err)
	}
	if err = dest.QueryRow(`SELECT code_hash FROM recovery_codes`).Scan(&got); err != nil || !bytes.Equal(got, hash[:]) {
		t.Fatal("recovery projection", err)
	}
	for _, mutation := range []string{
		`UPDATE users SET totp_ciphertext=x'01'`,
		`UPDATE users SET totp_last_step=-1`,
		`UPDATE users SET totp_pending_at=NULL`,
		`UPDATE users SET totp_pending_ciphertext=NULL`,
		`UPDATE recovery_codes SET code_hash=x'01'`,
		`UPDATE recovery_codes SET user_id='00000000-0000-4000-8000-000000000099'`,
	} {
		copy := validationDB(t, Configuration)
		copy.Exec(`DELETE FROM users`)
		if err = projectCategory(ctx, copy, source, nil, Configuration, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err = copy.Exec(mutation); err != nil {
			t.Fatal(err)
		}
		if err = ValidatePayloadData(ctx, copy, Configuration, key); err == nil {
			t.Errorf("accepted %s", mutation)
		}
	}
	if err = ValidatePayloadData(ctx, source, Configuration, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestTOTPBackupRestoreClearsPending(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := datadir.LoadKey(filepath.Join(source, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon account secrets v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	cipher, err := box.Seal("totp:"+restoreUser, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE users SET totp_ciphertext=?,totp_pending_ciphertext=?,totp_pending_at=?,totp_last_step=321 WHERE id=?`, cipher, cipher, validationTime, restoreUser); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("restore-recovery"))
	if _, err = db.Exec(`INSERT INTO recovery_codes VALUES(?,?)`, restoreUser, hash[:]); err != nil {
		t.Fatal(err)
	}
	archive, password := restoreArchive(t, source, []Category{Configuration})
	target := restoreFixture(t, "target")
	if _, err = Restore(ctx, target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored := restoreDB(t, target)
	var gotCipher, pending, gotHash []byte
	var at sql.NullString
	var step int64
	if err = restored.QueryRow(`SELECT totp_ciphertext,totp_pending_ciphertext,totp_pending_at,totp_last_step FROM users WHERE id=?`, restoreUser).Scan(&gotCipher, &pending, &at, &step); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cipher, gotCipher) || pending != nil || at.Valid || step != 321 {
		t.Fatal("TOTP state mismatch")
	}
	if err = restored.QueryRow(`SELECT code_hash FROM recovery_codes WHERE user_id=?`, restoreUser).Scan(&gotHash); err != nil || !bytes.Equal(gotHash, hash[:]) {
		t.Fatal("recovery not restored", err)
	}
	key, err = datadir.LoadKey(filepath.Join(target, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err = secretbox.New(key, "orbitbeacon account secrets v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open("totp:"+restoreUser, gotCipher)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatal("restored secret unreadable", err)
	}
}

func TestTOTPPayloadRejectsWrongSecretLength(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	box, err := secretbox.New(key, "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Seal("totp:"+validationID, []byte("1234567890123456789"))
	if err != nil {
		t.Fatal(err)
	}
	source := validationDB(t, Configuration)
	if _, err = source.Exec(`UPDATE users SET totp_ciphertext=?`, cipher); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePayloadData(context.Background(), source, Configuration, key); err == nil {
		t.Fatal("short TOTP secret accepted")
	}
}
