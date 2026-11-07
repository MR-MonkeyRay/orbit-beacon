package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

const upgradeDownloadTimeout = 10 * time.Minute
const upgradeConfirmTimeout = 2 * time.Minute

// upgradeReserve keeps the outcome inside the command lifetime: three unconfirmed
// runs, four restarts and the reports that carry the result.
const upgradeReserve = 7 * time.Minute
const maxUpgradeBinary = 64 << 20
const maxUpgradeManifest = 64 << 10

type upgradeOutcome struct {
	id, status, code string
	err              error
}

type upgrader struct {
	base, stateDir, executable, version, arch string
	client                                    *http.Client
}

func newUpgrader(policy config.InstallPolicy, version string) (*upgrader, error) {
	if policy.InstallMode != "native" || policy.UpdateBaseURL == "" {
		return nil, nil
	}
	base, err := config.NormalizeUpdateBaseURL(policy.UpdateBaseURL)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if policy.CAFile != "" {
		f, err := openCABundle(policy.CAFile, 0)
		if err != nil {
			return nil, err
		}
		pem, err := io.ReadAll(io.LimitReader(f, maxCABundleSize+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || len(pem) > maxCABundleSize || !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid upgrade CA bundle")
		}
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	u := &upgrader{base: base, stateDir: policy.StateDir, executable: executable, version: version, arch: runtime.GOARCH, client: &http.Client{Transport: transport}}
	u.client.CheckRedirect = upgradeRedirect
	return u, nil
}

func upgradeRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" || req.URL.User != nil || len(via) > 5 {
		return errors.New("unsafe upgrade redirect")
	}
	req.Header.Del("Authorization")
	return nil
}

func (u *upgrader) preflight(target string) string {
	next, err := v1.ParseVersion(target)
	current, currentErr := v1.ParseVersion(u.version)
	if err != nil || currentErr != nil || next.Compare(current) <= 0 {
		return "downgrade"
	}
	if runtime.GOOS != "linux" || (u.arch != "amd64" && u.arch != "arm64") {
		return "unsupported"
	}
	info, err := os.Lstat(u.executable)
	if err != nil || !info.Mode().IsRegular() {
		return "unsupported"
	}
	f, err := os.CreateTemp(filepath.Dir(u.executable), ".orbitbeacon-agent.*")
	if err != nil {
		return "unsupported"
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return ""
}

func cleanupUpgradeTemps(executable string) {
	names, _ := filepath.Glob(filepath.Join(filepath.Dir(executable), ".orbitbeacon-agent.*"))
	for _, name := range names {
		_ = os.Remove(name)
	}
}

func (u *upgrader) download(ctx context.Context, raw string, dst io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil {
		return errors.New("HTTPS required")
	}
	res, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength > limit {
		return errors.New("invalid upgrade response")
	}
	n, err := io.Copy(dst, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("upgrade size exceeded")
	}
	return nil
}

func manifestChecksum(data []byte, asset string) ([]byte, error) {
	var sum []byte
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[1] != asset && fields[1] != "*"+asset) {
			continue
		}
		count++
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, errors.New("invalid manifest line")
		}
		var err error
		sum, err = hex.DecodeString(fields[0])
		if err != nil {
			return nil, err
		}
	}
	if count != 1 {
		return nil, errors.New("manifest must name exactly one asset")
	}
	return sum, nil
}

type boundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 4096 {
		b.exceeded = true
		return n, errors.New("self check output exceeded")
	}
	_, err := b.Buffer.Write(p)
	return n, err
}
func (u *upgrader) selfCheck(ctx context.Context, name, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, "version", "--json")
	cmd.Env = []string{}
	cmd.WaitDelay = time.Second
	var output, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	var info struct {
		Version string `json:"version"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if output.exceeded || stderr.exceeded || json.Unmarshal(output.Bytes(), &info) != nil || info.Version != target || info.OS != runtime.GOOS || info.Arch != u.arch {
		return errors.New("candidate identity mismatch")
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func durableFile(path string, src io.Reader, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".orbitbeacon-agent.*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (u *upgrader) run(ctx context.Context, c v1.Command) upgradeOutcome {
	id := strings.ToLower(c.ID)
	failed := func(code string, err error) upgradeOutcome {
		return upgradeOutcome{id: id, status: "failed", code: code, err: err}
	}
	if code := u.preflight(c.TargetVersion); code != "" {
		return upgradeOutcome{id: id, status: "rejected", code: code}
	}
	asset := "orbitbeacon-agent-linux-" + u.arch
	base := u.base + "/v" + c.TargetVersion + "/"
	var manifest bytes.Buffer
	if err := u.download(ctx, base+"SHA256SUMS", &manifest, maxUpgradeManifest); err != nil {
		return failed("download", err)
	}
	sum, err := manifestChecksum(manifest.Bytes(), asset)
	if err != nil {
		return failed("checksum", err)
	}
	candidate, err := os.CreateTemp(filepath.Dir(u.executable), ".orbitbeacon-agent.*")
	if err != nil {
		return failed("download", err)
	}
	defer os.Remove(candidate.Name())
	defer candidate.Close()
	hash := sha256.New()
	if err := u.download(ctx, base+asset, io.MultiWriter(candidate, hash), maxUpgradeBinary); err != nil {
		return failed("download", err)
	}
	if !bytes.Equal(sum, hash.Sum(nil)) {
		return failed("checksum", errors.New("checksum mismatch"))
	}
	if err := errors.Join(candidate.Chmod(0750), candidate.Sync(), candidate.Close()); err != nil {
		return failed("self_check", err)
	}
	if err := u.selfCheck(ctx, candidate.Name(), c.TargetVersion); err != nil {
		return failed("self_check", err)
	}
	if err := ctx.Err(); err != nil {
		return failed("self_check", err)
	}
	current, err := os.Open(u.executable)
	if err != nil {
		return failed("failed", err)
	}
	err = durableFile(filepath.Join(filepath.Dir(u.executable), "orbitbeacon-agent.prev"), current, 0750)
	_ = current.Close()
	if err != nil {
		return failed("failed", err)
	}
	if err := ctx.Err(); err != nil {
		return failed("failed", err)
	}
	marker := filepath.Join(u.stateDir, "upgrade-pending")
	if err := durableFile(marker, strings.NewReader("0"), 0600); err != nil {
		_ = os.Remove(marker)
		return failed("failed", err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(marker)
		return failed("failed", err)
	}
	if err := os.Rename(candidate.Name(), u.executable); err != nil {
		_ = os.Remove(marker)
		return failed("failed", err)
	}
	// After the switch, restart even if directory syncing fails: recovery owns the marker.
	_ = syncDirectory(filepath.Dir(u.executable))
	return upgradeOutcome{id: id, status: "succeeded", code: "ok"}
}
