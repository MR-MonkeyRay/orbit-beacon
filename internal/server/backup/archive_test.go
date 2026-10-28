package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func framingTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func framingManifest(categories []Category) Manifest {
	now := time.Date(2026, 10, 27, 12, 0, 0, 123, time.UTC)
	m := Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: "c03eca08-973d-4b86-979f-beb99dbe5e1f", CreatedAt: now, ServerVersion: "test", SchemaVersion: 16, HistorySchemaVersion: 1, Categories: categories, Snapshots: Snapshots{MainAt: now}}
	for _, c := range categories {
		if c == Monitoring {
			m.Snapshots.MetricsAt = &now
		}
	}
	return m
}

func framingInputs(t *testing.T, categories []Category) []ArchiveInput {
	t.Helper()
	dir := framingTempDir(t)
	var inputs []ArchiveInput
	for _, c := range categories {
		name := string(c) + ".sqlite"
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("dummy sqlite "+string(c)), 0600); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, ArchiveInput{Name: name, Path: path})
		if c == Configuration {
			path = filepath.Join(dir, "secrets.key")
			if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 32), 0600); err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, ArchiveInput{Name: "secrets.key", Path: path})
		}
	}
	return inputs
}

func TestArchiveRoundTripEveryCategoryCombination(t *testing.T) {
	for mask := 1; mask < 8; mask++ {
		var categories []Category
		for i, c := range []Category{Configuration, Monitoring, Events} {
			if mask&(1<<i) != 0 {
				categories = append(categories, c)
			}
		}
		t.Run(strings.Join(categoryStrings(categories), "+"), func(t *testing.T) {
			inputs := framingInputs(t, categories)
			output := filepath.Join(framingTempDir(t), "output.tar.gz.age")
			password, err := GeneratePassword(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			info, err := WriteArchive(context.Background(), output, framingManifest(categories), inputs, password)
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(cipher)
			if info.Bytes != int64(len(cipher)) || info.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("wrong ciphertext metadata")
			}
			st, _ := os.Stat(output)
			if st.Mode().Perm() != 0600 {
				t.Fatal("archive mode")
			}
			stage := filepath.Join(framingTempDir(t), "stage")
			m, err := ReadArchive(context.Background(), output, password, stage)
			if err != nil {
				t.Fatal(err)
			}
			if m.ArchiveID != framingManifest(categories).ArchiveID || len(m.Files) != len(inputs) {
				t.Fatal("manifest changed")
			}
			st, _ = os.Stat(stage)
			if st.Mode().Perm() != 0700 {
				t.Fatal("stage mode")
			}
			for _, in := range inputs {
				got, err := os.ReadFile(filepath.Join(stage, in.Name))
				if err != nil {
					t.Fatal(err)
				}
				want, _ := os.ReadFile(in.Path)
				if !bytes.Equal(got, want) {
					t.Fatal("payload changed")
				}
				st, _ = os.Stat(filepath.Join(stage, in.Name))
				if st.Mode().Perm() != 0600 {
					t.Fatal("payload mode")
				}
			}
			entries, _ := os.ReadDir(filepath.Dir(output))
			if len(entries) != 1 {
				t.Fatal("plaintext intermediate persisted")
			}
			if _, err := WriteArchive(context.Background(), output, framingManifest(categories), inputs, password); err == nil {
				t.Fatal("overwrote existing archive")
			}
		})
	}
}

func categoryStrings(categories []Category) []string {
	result := make([]string, len(categories))
	for i, c := range categories {
		result[i] = string(c)
	}
	return result
}

func TestArchivePasswordsAndCancellation(t *testing.T) {
	a, err := GeneratePassword(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePassword(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 || len(b) != 16 || a == b || strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		t.Fatal("password alphabet/entropy")
	}
	if _, err := GeneratePassword(bytes.NewReader(nil)); err == nil {
		t.Fatal("random failure ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(framingTempDir(t), "cancelled")
	if _, err := WriteArchive(ctx, path, framingManifest([]Category{Events}), framingInputs(t, []Category{Events}), a); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled artifact remains")
	}
}

func TestArchiveRejectsWrongPasswordDamageAndTruncation(t *testing.T) {
	path := filepath.Join(framingTempDir(t), "archive")
	password := "aB0123456789cDef"
	if _, err := WriteArchive(context.Background(), path, framingManifest([]Category{Events}), framingInputs(t, []Category{Events}), password); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	for _, kind := range []string{"password", "tamper", "truncate"} {
		t.Run(kind, func(t *testing.T) {
			data := bytes.Clone(original)
			pass := password
			switch kind {
			case "password":
				pass = "wrong"
			case "tamper":
				data[len(data)-1] ^= 1
			case "truncate":
				data = data[:len(data)-1]
			}
			bad := filepath.Join(framingTempDir(t), "bad")
			if err := os.WriteFile(bad, data, 0600); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(framingTempDir(t), "stage")
			if _, err := ReadArchive(context.Background(), bad, pass, stage); err == nil {
				t.Fatal("accepted damaged archive")
			}
			entries, _ := os.ReadDir(stage)
			if len(entries) != 0 {
				t.Fatal("partial extraction remains")
			}
		})
	}
}

func forgedArchive(t *testing.T, raw []byte, secondMember bool) string {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if secondMember {
		gz = gzip.NewWriter(&compressed)
		_, _ = gz.Write([]byte("second"))
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var encrypted bytes.Buffer
	r, err := age.NewScryptRecipient("aB0123456789cDef")
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(15)
	w, err := age.Encrypt(&encrypted, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(framingTempDir(t), "forged")
	if err := os.WriteFile(path, encrypted.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveRejectsMaliciousFraming(t *testing.T) {
	payload := []byte("events")
	sum := sha256.Sum256(payload)
	base := framingManifest([]Category{Events})
	base.Files = []ManifestFile{{Path: "events.sqlite", Bytes: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}}
	for _, kind := range []string{"extra", "duplicate", "missing", "path", "symlink", "hardlink", "device", "size", "hash", "manifest_unknown", "manifest_duplicate", "categories", "schema", "history_schema", "uuid", "time", "gzip_member", "tar_trailing", "missing_tar_end", "nonzero_padding"} {
		t.Run(kind, func(t *testing.T) {
			m := base
			m.Categories = append([]Category(nil), base.Categories...)
			m.Files = append([]ManifestFile(nil), base.Files...)
			switch kind {
			case "hash":
				m.Files[0].SHA256 = strings.Repeat("0", 64)
			case "categories":
				m.Categories = []Category{Events, Configuration}
			case "schema":
				m.SchemaVersion = 15
			case "history_schema":
				m.HistorySchemaVersion = 2
			case "uuid":
				m.ArchiveID = "not-uuid"
			case "time":
				m.CreatedAt = time.Time{}
			}
			manifest, _ := json.Marshal(m)
			if kind == "manifest_unknown" {
				manifest = append(manifest[:len(manifest)-1], []byte(`,"unknown":1}`)...)
			}
			if kind == "manifest_duplicate" {
				manifest = append(manifest[:len(manifest)-1], []byte(`,"version":1}`)...)
			}
			var raw bytes.Buffer
			tw := tar.NewWriter(&raw)
			add := func(name string, data []byte, typ byte) {
				h := &tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: typ}
				if typ != tar.TypeReg {
					h.Size = 0
					h.Linkname = "events.sqlite"
				}
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if typ == tar.TypeReg {
					if _, err := tw.Write(data); err != nil {
						t.Fatal(err)
					}
				}
			}
			add("manifest.json", manifest, tar.TypeReg)
			name := "events.sqlite"
			typ := byte(tar.TypeReg)
			switch kind {
			case "path":
				name = "../events.sqlite"
			case "symlink":
				typ = tar.TypeSymlink
			case "hardlink":
				typ = tar.TypeLink
			case "device":
				typ = tar.TypeChar
			}
			if kind == "size" {
				if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 32<<30 + 1}); err != nil {
					t.Fatal(err)
				}
			} else if kind != "missing" {
				add(name, payload, typ)
			}
			if kind == "extra" {
				add("other", nil, tar.TypeReg)
			}
			if kind == "duplicate" {
				add(name, payload, tar.TypeReg)
			}
			if kind != "size" {
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
			}
			data := raw.Bytes()
			if kind == "tar_trailing" {
				data = append(data, []byte("evil")...)
			}
			if kind == "missing_tar_end" {
				data = data[:len(data)-1024]
			}
			if kind == "nonzero_padding" {
				data = bytes.Clone(data)
				data[512+len(manifest)] = 1
			}
			stage := filepath.Join(framingTempDir(t), "stage")
			if _, err := ReadArchive(context.Background(), forgedArchive(t, data, kind == "gzip_member"), "aB0123456789cDef", stage); err == nil {
				t.Fatal("accepted malicious framing")
			}
			entries, _ := os.ReadDir(stage)
			if len(entries) != 0 {
				t.Fatal("partial extraction remains")
			}
		})
	}
}

func TestArchiveRejectsOversizedManifestAndWriterInputs(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: 64<<10 + 1})
	_, _ = io.CopyN(tw, strings.NewReader(strings.Repeat(" ", 64<<10+1)), 64<<10+1)
	_ = tw.Close()
	if _, err := ReadArchive(context.Background(), forgedArchive(t, raw.Bytes(), false), "aB0123456789cDef", filepath.Join(framingTempDir(t), "stage")); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	inputs := framingInputs(t, []Category{Events})
	inputs = append(inputs, inputs[0])
	if _, err := WriteArchive(context.Background(), filepath.Join(framingTempDir(t), "out"), framingManifest([]Category{Events}), inputs, "aB0123456789cDef"); err == nil {
		t.Fatal("duplicate writer input accepted")
	}
	large := filepath.Join(framingTempDir(t), "large.sqlite")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate((32 << 30) + 1); err != nil {
		file.Close()
		t.Skipf("sparse files unavailable: %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = WriteArchive(context.Background(), filepath.Join(framingTempDir(t), "out"), framingManifest([]Category{Events}), []ArchiveInput{{Name: "events.sqlite", Path: large}}, "aB0123456789cDef"); err == nil {
		t.Fatal("oversized source accepted")
	}
	symlink := filepath.Join(framingTempDir(t), "link.sqlite")
	if err = os.Symlink(inputs[0].Path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err = WriteArchive(context.Background(), filepath.Join(framingTempDir(t), "out"), framingManifest([]Category{Events}), []ArchiveInput{{Name: "events.sqlite", Path: symlink}}, "aB0123456789cDef"); err == nil {
		t.Fatal("source symlink accepted")
	}
}

func TestArchiveRejectsScryptWorkFactorAboveLimit(t *testing.T) {
	var data bytes.Buffer
	recipient, err := age.NewScryptRecipient("aB0123456789cDef")
	if err != nil {
		t.Fatal(err)
	}
	recipient.SetWorkFactor(16)
	encrypted, err := age.Encrypt(&data, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if err = encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(framingTempDir(t), "high-work")
	if err = os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadArchive(context.Background(), path, "aB0123456789cDef", filepath.Join(framingTempDir(t), "stage")); err == nil {
		t.Fatal("expensive scrypt input accepted")
	}
}

func TestArchiveRejectsSymlinkAncestorsAndPreservesExistingStage(t *testing.T) {
	inputs := framingInputs(t, []Category{Events})
	manifest := framingManifest([]Category{Events})
	password := "aB0123456789cDef"
	actual := framingTempDir(t)
	link := filepath.Join(framingTempDir(t), "linked")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteArchive(context.Background(), filepath.Join(link, "out"), manifest, inputs, password); err == nil {
		t.Fatal("output symlink ancestor accepted")
	}
	archive := filepath.Join(framingTempDir(t), "archive")
	if _, err := WriteArchive(context.Background(), archive, manifest, inputs, password); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(archive, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(context.Background(), archive, password, filepath.Join(framingTempDir(t), "readable")); err != nil {
		t.Fatalf("ordinary external archive rejected: %v", err)
	}
	if err := os.Rename(archive, filepath.Join(actual, "archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(context.Background(), filepath.Join(link, "archive"), password, filepath.Join(framingTempDir(t), "stage")); err == nil {
		t.Fatal("input symlink ancestor accepted")
	}
	stage := framingTempDir(t)
	keep := filepath.Join(stage, "events.sqlite")
	if err := os.WriteFile(keep, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(context.Background(), filepath.Join(actual, "archive"), password, stage); err == nil {
		t.Fatal("existing payload overwritten")
	}
	data, err := os.ReadFile(keep)
	if err != nil || string(data) != "user data" {
		t.Fatal("existing stage file removed or changed")
	}
	if err := os.Chmod(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(context.Background(), filepath.Join(actual, "archive"), password, stage); err == nil {
		t.Fatal("unsafe stage accepted")
	}
	info, err := os.Stat(stage)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("existing stage permissions changed")
	}
}
