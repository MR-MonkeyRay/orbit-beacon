package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

const (
	maxArchiveBytes  int64 = 32 << 30
	maxManifestBytes       = 64 << 10
	passwordAlphabet       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// Error exposes a stable safe code while retaining the cause for local classification.
// Error strings never include archive-controlled paths or encryption diagnostics.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code }
func (e *Error) Unwrap() error { return e.Err }

type Manifest struct {
	Format               string         `json:"format"`
	Version              int            `json:"version"`
	ArchiveID            string         `json:"archive_id"`
	CreatedAt            time.Time      `json:"created_at"`
	ServerVersion        string         `json:"server_version"`
	SchemaVersion        int            `json:"schema_version"`
	HistorySchemaVersion int            `json:"history_schema_version"`
	Categories           []Category     `json:"categories"`
	Snapshots            Snapshots      `json:"snapshots"`
	Files                []ManifestFile `json:"files"`
}

type Snapshots struct {
	MainAt    time.Time  `json:"main_at"`
	MetricsAt *time.Time `json:"metrics_at"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type ArchiveInput struct {
	Name string
	Path string
}
type ArchiveInfo struct {
	Bytes  int64
	SHA256 string
}

// GeneratePassword samples each character independently without modulo bias.
func GeneratePassword(random io.Reader) (string, error) {
	if random == nil {
		return "", &Error{Code: "internal_error"}
	}
	var result [16]byte
	for i := range result {
		n, err := rand.Int(random, big.NewInt(int64(len(passwordAlphabet))))
		if err != nil {
			return "", err
		}
		result[i] = passwordAlphabet[n.Int64()]
	}
	return string(result[:]), nil
}

func archiveNames(categories []Category) []string {
	var names []string
	for _, category := range categories {
		names = append(names, string(category)+".sqlite")
		if category == Configuration {
			names = append(names, "secrets.key")
		}
	}
	return names
}

func invalidArchive(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var coded *Error
	if errors.As(err, &coded) {
		return err
	}
	return &Error{Code: "archive_invalid", Err: err}
}

func archiveTooLarge() error { return &Error{Code: "archive_too_large"} }

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

// WriteArchive streams tar -> gzip -> age, never writing a plaintext compressed file.
// The caller supplies an existing private output directory and immutable payload files.
func WriteArchive(ctx context.Context, output string, manifest Manifest, files []ArchiveInput, password string) (info ArchiveInfo, resultErr error) {
	if err := ctx.Err(); err != nil {
		return info, err
	}
	if len(password) != 16 || strings.Trim(password, passwordAlphabet) != "" {
		return info, invalidArchive(nil)
	}
	manifest.CreatedAt = manifest.CreatedAt.UTC()
	manifest.Snapshots.MainAt = manifest.Snapshots.MainAt.UTC()
	if manifest.Snapshots.MetricsAt != nil {
		at := manifest.Snapshots.MetricsAt.UTC()
		manifest.Snapshots.MetricsAt = &at
	}
	names := archiveNames(manifest.Categories)
	if len(files) != len(names) {
		return info, invalidArchive(nil)
	}
	opened := make([]*os.File, 0, len(files))
	defer func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}()
	manifest.Files = make([]ManifestFile, 0, len(files))
	var total int64
	for i, input := range files {
		if input.Name != names[i] {
			return info, invalidArchive(nil)
		}
		file, err := securefs.OpenFile(input.Path, os.O_RDONLY, 0)
		if err != nil {
			return info, invalidArchive(err)
		}
		opened = append(opened, file)
		stat, err := file.Stat()
		if err != nil {
			return info, err
		}
		if stat.Size() > maxArchiveBytes-total {
			return info, archiveTooLarge()
		}
		total += stat.Size()
		hash := sha256.New()
		count, err := io.Copy(hash, contextReader{ctx, io.LimitReader(file, stat.Size()+1)})
		if err != nil {
			return info, err
		}
		if count != stat.Size() {
			return info, invalidArchive(nil)
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return info, err
		}
		manifest.Files = append(manifest.Files, ManifestFile{Path: input.Name, Bytes: count, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	if err := validateManifest(manifest); err != nil {
		return info, err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return info, invalidArchive(err)
	}
	if len(manifestJSON) > maxManifestBytes || int64(len(manifestJSON)) > maxArchiveBytes-total {
		return info, archiveTooLarge()
	}
	if err = securefs.ValidateDirectory(filepath.Dir(output)); err != nil {
		return info, err
	}
	file, err := securefs.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return info, err
	}
	defer func() {
		_ = file.Close()
		if resultErr != nil {
			_ = os.Remove(output)
		}
	}()
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return info, invalidArchive(err)
	}
	recipient.SetWorkFactor(15)
	encrypted, err := age.Encrypt(contextWriter{ctx, file}, recipient)
	if err != nil {
		return info, err
	}
	compressed := gzip.NewWriter(encrypted)
	packed := tar.NewWriter(compressed)
	defer func() {
		if resultErr != nil {
			_ = packed.Close()
			_ = compressed.Close()
			_ = encrypted.Close()
		}
	}()
	writeHeader := func(name string, size int64) error {
		return packed.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
	}
	if err = writeHeader("manifest.json", int64(len(manifestJSON))); err != nil {
		return info, err
	}
	if _, err = packed.Write(manifestJSON); err != nil {
		return info, err
	}
	for i, payload := range opened {
		entry := manifest.Files[i]
		if err = writeHeader(entry.Path, entry.Bytes); err != nil {
			return info, err
		}
		hash := sha256.New()
		count, copyErr := io.Copy(io.MultiWriter(packed, hash), contextReader{ctx, payload})
		if copyErr != nil {
			return info, copyErr
		}
		if count != entry.Bytes || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return info, invalidArchive(nil)
		}
	}
	if err = packed.Close(); err != nil {
		return info, err
	}
	if err = compressed.Close(); err != nil {
		return info, err
	}
	if err = encrypted.Close(); err != nil {
		return info, err
	}
	if err = file.Sync(); err != nil {
		return info, err
	}
	if err = file.Close(); err != nil {
		return info, err
	}
	if err = securefs.SyncDirectory(filepath.Dir(output)); err != nil {
		return info, err
	}
	finished, err := securefs.OpenFile(output, os.O_RDONLY, 0)
	if err != nil {
		return info, err
	}
	defer finished.Close()
	hash := sha256.New()
	info.Bytes, err = io.Copy(hash, contextReader{ctx, finished})
	if err != nil {
		return ArchiveInfo{}, err
	}
	info.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return info, nil
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validateManifest(m Manifest) error {
	if m.Format != "orbitbeacon-backup" || m.Version != 1 || m.SchemaVersion != storage.CurrentSchemaVersion || m.HistorySchemaVersion != 1 {
		return invalidArchive(nil)
	}
	if !canonicalUUID(m.ArchiveID) {
		return invalidArchive(nil)
	}
	if m.CreatedAt.IsZero() || m.Snapshots.MainAt.IsZero() || m.ServerVersion == "" {
		return invalidArchive(nil)
	}
	categories, err := ValidateCategories(m.Categories)
	if err != nil || len(categories) != len(m.Categories) {
		return invalidArchive(err)
	}
	monitoring := false
	for i, c := range categories {
		if c != m.Categories[i] {
			return invalidArchive(nil)
		}
		if c == Monitoring {
			monitoring = true
		}
	}
	if monitoring != (m.Snapshots.MetricsAt != nil) || (m.Snapshots.MetricsAt != nil && m.Snapshots.MetricsAt.IsZero()) {
		return invalidArchive(nil)
	}
	names := archiveNames(categories)
	if len(m.Files) != len(names) {
		return invalidArchive(nil)
	}
	var total int64
	for i, f := range m.Files {
		if f.Path != names[i] || f.Bytes < 0 || len(f.SHA256) != 64 || strings.ToLower(f.SHA256) != f.SHA256 {
			return invalidArchive(nil)
		}
		if !validSHA256(f.SHA256) {
			_, err := hex.DecodeString(f.SHA256)
			return invalidArchive(err)
		}
		if f.Bytes > maxArchiveBytes-total {
			return archiveTooLarge()
		}
		total += f.Bytes
		if f.Path == "secrets.key" && f.Bytes != 32 {
			return invalidArchive(nil)
		}
	}
	return nil
}

// checkJSONValue rejects duplicate object keys at every nesting level.
func checkJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			token, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return invalidArchive(nil)
			}
			seen[key] = true
			if err = checkJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = checkJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return invalidArchive(nil)
	}
	_, err = d.Token()
	return err
}

func requiredJSONFields(data []byte, names ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	if len(object) != len(names) {
		return nil, invalidArchive(nil)
	}
	for _, name := range names {
		value, ok := object[name]
		if !ok || (string(value) == "null" && name != "metrics_at") {
			return nil, invalidArchive(nil)
		}
	}
	return object, nil
}

func canonicalArchiveTime(data []byte) bool {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	return err == nil && !parsed.IsZero() && parsed.UTC().Format(time.RFC3339Nano) == text
}

func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	duplicates := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(duplicates); err != nil {
		return m, invalidArchive(err)
	}
	if _, err := duplicates.Token(); err != io.EOF {
		return m, invalidArchive(err)
	}
	root, err := requiredJSONFields(data, "format", "version", "archive_id", "created_at", "server_version", "schema_version", "history_schema_version", "categories", "snapshots", "files")
	if err != nil {
		return m, invalidArchive(err)
	}
	snapshots, err := requiredJSONFields(root["snapshots"], "main_at", "metrics_at")
	if err != nil {
		return m, invalidArchive(err)
	}
	if !canonicalArchiveTime(root["created_at"]) || !canonicalArchiveTime(snapshots["main_at"]) || (string(snapshots["metrics_at"]) != "null" && !canonicalArchiveTime(snapshots["metrics_at"])) {
		return m, invalidArchive(nil)
	}
	var files []json.RawMessage
	if err = json.Unmarshal(root["files"], &files); err != nil {
		return m, invalidArchive(err)
	}
	for _, f := range files {
		if _, err = requiredJSONFields(f, "path", "bytes", "sha256"); err != nil {
			return m, invalidArchive(err)
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, invalidArchive(err)
	}
	return m, validateManifest(m)
}

// readArchiveHeader deliberately reads exactly one tar record. archive/tar's usual
// iterator accepts missing end blocks and hides padding, which is too permissive here.
func readArchiveHeader(reader io.Reader) (*tar.Header, error) {
	var block [512]byte
	if _, err := io.ReadFull(reader, block[:]); err != nil {
		return nil, invalidArchive(err)
	}
	header, err := tar.NewReader(bytes.NewReader(block[:])).Next()
	if err != nil {
		return nil, invalidArchive(err)
	}
	if header.Typeflag != tar.TypeReg {
		return nil, invalidArchive(nil)
	}
	if header.Size < 0 || header.Linkname != "" || header.PAXRecords != nil || header.Name == "" {
		return nil, invalidArchive(nil)
	}
	return header, nil
}

func readZeroBytes(reader io.Reader, count int64) error {
	var buffer [512]byte
	for count > 0 {
		n := min(count, int64(len(buffer)))
		if _, err := io.ReadFull(reader, buffer[:n]); err != nil {
			return invalidArchive(err)
		}
		for _, b := range buffer[:n] {
			if b != 0 {
				return invalidArchive(nil)
			}
		}
		count -= n
	}
	return nil
}

func readArchivePadding(reader io.Reader, size int64) error {
	return readZeroBytes(reader, (512-size%512)%512)
}

// ReadArchive authenticates the entire ciphertext and gzip member before returning.
// It only validates archive framing; SQLite schema and domain validation are separate.
func ReadArchive(ctx context.Context, input, password, stage string) (manifest Manifest, resultErr error) {
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	file, err := securefs.OpenRegular(input)
	if err != nil {
		return manifest, invalidArchive(err)
	}
	defer file.Close()
	identity, err := age.NewScryptIdentity(password)
	if err != nil {
		return manifest, invalidArchive(err)
	}
	identity.SetMaxWorkFactor(15)
	decrypted, err := age.Decrypt(contextReader{ctx, file}, identity)
	if err != nil {
		return manifest, invalidArchive(err)
	}
	buffered := bufio.NewReader(contextReader{ctx, decrypted})
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return manifest, invalidArchive(err)
	}
	defer compressed.Close()
	compressed.Multistream(false)
	reader := contextReader{ctx, compressed}
	header, err := readArchiveHeader(reader)
	if err != nil {
		return manifest, err
	}
	if header.Name != "manifest.json" {
		return manifest, invalidArchive(nil)
	}
	if header.Size > maxManifestBytes {
		return manifest, archiveTooLarge()
	}
	data := make([]byte, int(header.Size))
	if _, err = io.ReadFull(reader, data); err != nil {
		return manifest, invalidArchive(err)
	}
	if err = readArchivePadding(reader, header.Size); err != nil {
		return manifest, err
	}
	manifest, err = decodeManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	total := int64(len(data))
	for _, entry := range manifest.Files {
		if entry.Bytes > maxArchiveBytes-total {
			return Manifest{}, archiveTooLarge()
		}
		total += entry.Bytes
	}
	if err = securefs.ValidateParents(stage); err != nil {
		return Manifest{}, err
	}
	if err = os.Mkdir(stage, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Manifest{}, err
	}
	if err = securefs.ValidateDirectory(stage); err != nil {
		return Manifest{}, err
	}
	created := make([]string, 0, len(manifest.Files))
	defer func() {
		if resultErr != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	for _, entry := range manifest.Files {
		header, err = readArchiveHeader(reader)
		if err != nil {
			return Manifest{}, err
		}
		if header.Size > maxArchiveBytes {
			return Manifest{}, archiveTooLarge()
		}
		if header.Name != entry.Path || header.Size != entry.Bytes {
			return Manifest{}, invalidArchive(nil)
		}
		path := filepath.Join(stage, entry.Path)
		payload, openErr := securefs.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return Manifest{}, openErr
		}
		created = append(created, path)
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(payload, hash), reader, entry.Bytes)
		if copyErr == nil {
			copyErr = payload.Sync()
		}
		closeErr := payload.Close()
		if copyErr != nil {
			return Manifest{}, invalidArchive(copyErr)
		}
		if closeErr != nil {
			return Manifest{}, closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return Manifest{}, invalidArchive(nil)
		}
		if err = readArchivePadding(reader, entry.Bytes); err != nil {
			return Manifest{}, err
		}
	}
	// Two mandatory zero records, followed only by bounded zero record padding.
	if err = readZeroBytes(reader, 1024); err != nil {
		return Manifest{}, err
	}
	var padding [8193]byte
	n, err := io.ReadFull(reader, padding[:])
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		return Manifest{}, invalidArchive(err)
	}
	if n > 8192 || n%512 != 0 {
		return Manifest{}, invalidArchive(nil)
	}
	for _, b := range padding[:n] {
		if b != 0 {
			return Manifest{}, invalidArchive(nil)
		}
	}
	if err = compressed.Close(); err != nil {
		return Manifest{}, invalidArchive(err)
	}
	// Multistream(false) preserves any second gzip member in buffered. Requiring
	// EOF also forces age to authenticate its final chunk (including empty tails).
	if _, err = buffered.ReadByte(); err != io.EOF {
		return Manifest{}, invalidArchive(err)
	}
	if err = ctx.Err(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}
