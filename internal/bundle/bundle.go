package bundle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxManifestBytes = 64 * 1024
	MaxArtifactBytes = 50 * 1024 * 1024
	MaxArtifacts     = 128
)

type Manifest struct {
	Format      int        `json:"format"`
	Version     string     `json:"version"`
	ReleaseSeq  uint64     `json:"release_seq"`
	ProtocolMin int        `json:"protocol_min"`
	ProtocolMax int        `json:"protocol_max"`
	Artifacts   []Artifact `json:"artifacts"`
}

type Artifact struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kind     string `json:"kind"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

func VerifyBundle(directory, publicKeyFile string) (Manifest, error) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(publicKeyFile) == "" {
		return Manifest{}, errors.New("bundle directory and public key are required")
	}
	bundleDirectory, err := openBundleDirectory(directory)
	if err != nil {
		return Manifest{}, err
	}
	defer bundleDirectory.Close()
	manifestBytes, err := readBundleFile(bundleDirectory, "manifest.json", MaxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	signature, err := readBundleFile(bundleDirectory, "manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return Manifest{}, errors.New("manifest signature must be 64 raw Ed25519 bytes")
	}
	if !utf8.Valid(manifestBytes) {
		return Manifest{}, errors.New("manifest must be valid UTF-8")
	}
	publicKey, err := readPublicKey(publicKeyFile)
	if err != nil {
		return Manifest{}, err
	}
	if !ed25519.Verify(publicKey, manifestBytes, signature) {
		return Manifest{}, errors.New("manifest signature verification failed")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	for _, artifact := range manifest.Artifacts {
		if err := verifyArtifact(bundleDirectory, artifact); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func (manifest Manifest) Validate() error {
	if manifest.Format != 1 || strings.TrimSpace(manifest.Version) == "" || len(manifest.Version) > 128 || manifest.ReleaseSeq == 0 {
		return errors.New("manifest format, version or release sequence is invalid")
	}
	if manifest.ProtocolMin < 1 || manifest.ProtocolMax < manifest.ProtocolMin {
		return errors.New("manifest protocol range is invalid")
	}
	if len(manifest.Artifacts) == 0 || len(manifest.Artifacts) > MaxArtifacts {
		return errors.New("manifest artifact count is invalid")
	}
	seen := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.OS == "" || artifact.Arch == "" || artifact.Kind == "" || len(artifact.OS) > 32 || len(artifact.Arch) > 32 || len(artifact.Kind) > 32 {
			return errors.New("manifest artifact platform or kind is invalid")
		}
		if !safeFilename(artifact.Filename) {
			return fmt.Errorf("manifest artifact filename is unsafe: %q", artifact.Filename)
		}
		if _, exists := seen[artifact.Filename]; exists {
			return fmt.Errorf("manifest artifact filename is duplicated: %q", artifact.Filename)
		}
		seen[artifact.Filename] = struct{}{}
		if artifact.Size < 0 || artifact.Size > MaxArtifactBytes {
			return fmt.Errorf("manifest artifact size is invalid: %q", artifact.Filename)
		}
		if len(artifact.SHA256) != sha256.Size*2 || strings.Trim(artifact.SHA256, "0123456789abcdef") != "" {
			return fmt.Errorf("manifest artifact hash is invalid: %q", artifact.Filename)
		}
	}
	return nil
}

func verifyArtifact(directory *os.File, artifact Artifact) error {
	file, err := openBundleFile(directory, artifact.Filename)
	if err != nil {
		return fmt.Errorf("open artifact %q: %w", artifact.Filename, err)
	}
	defer file.Close()
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, MaxArtifactBytes+1))
	if err != nil {
		return fmt.Errorf("read artifact %q: %w", artifact.Filename, err)
	}
	if count != artifact.Size {
		return fmt.Errorf("artifact %q size mismatch: got %d want %d", artifact.Filename, count, artifact.Size)
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return fmt.Errorf("artifact %q SHA-256 mismatch", artifact.Filename)
	}
	return nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := readRegularPath(path, 4096)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	if block, _ := pem.Decode(data); block != nil {
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		publicKey, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("public key is not Ed25519")
		}
		return publicKey, nil
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be PKIX PEM or 32 raw Ed25519 bytes")
	}
	return ed25519.PublicKey(append([]byte(nil), data...)), nil
}

func readRegularPath(path string, limit int64) ([]byte, error) {
	file, err := openRegularPath(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func safeFilename(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsRune(name, 0) && !strings.ContainsAny(name, `/\\`)
}
