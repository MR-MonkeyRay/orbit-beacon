package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/webui"
	"orbitbeacon/internal/uuid"
)

type staticAsset struct {
	data        []byte
	gzipData    []byte
	contentType string
	etag        string
	gzipETag    string
}

type assetManifest struct {
	AssetBase *string           `json:"asset_base"`
	Versions  map[string]string `json:"versions"`
}

func readAssetManifest(assets fs.FS) (*assetManifest, string, error) {
	data, err := fs.ReadFile(assets, "asset-manifest.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var manifest assetManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, "", fmt.Errorf("asset manifest: %w", err)
	}
	if manifest.AssetBase == nil || manifest.Versions == nil {
		return nil, "", fmt.Errorf("asset manifest requires asset_base and versions")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, "", fmt.Errorf("asset manifest has trailing data")
	}
	base := *manifest.AssetBase
	if base == "" {
		return &manifest, "", nil
	}
	origin, err := config.CanonicalOrigin(base)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return nil, "", fmt.Errorf("asset_base must be an HTTPS URL without credentials, query or fragment")
	}
	u, err := url.Parse(base)
	unsafeBase := strings.ContainsAny(base, "\\%\"'<>`;@") || strings.IndexFunc(base, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0
	if err != nil || u.RawPath != "" || unsafeBase || (u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) {
		return nil, "", fmt.Errorf("asset_base must have a clean, unencoded ASCII path prefix")
	}
	return &manifest, origin, nil
}

func prepareUIAssets(assets fs.FS, origin string) (map[string]staticAsset, map[string]string, error) {
	manifest, assetOrigin, err := readAssetManifest(assets)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := prepareStaticAssetsWithManifest(assets, manifest)
	if err != nil {
		return nil, nil, err
	}
	policies, err := htmlPoliciesWithAssetOrigin(assets, origin, assetOrigin)
	return prepared, policies, err
}

func prepareStaticAssets(assets fs.FS) (map[string]staticAsset, error) {
	manifest, _, err := readAssetManifest(assets)
	if err != nil {
		return nil, err
	}
	return prepareStaticAssetsWithManifest(assets, manifest)
}

func prepareStaticAssetsWithManifest(assets fs.FS, manifest *assetManifest) (map[string]staticAsset, error) {
	prepared := make(map[string]staticAsset)
	err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || name == "asset-manifest.json" {
			return nil
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		asset := staticAsset{
			data:        data,
			contentType: mime.TypeByExtension(path.Ext(name)),
			etag:        assetETag(data),
		}
		if asset.contentType == "" {
			asset.contentType = http.DetectContentType(data)
		}
		if compressibleAsset(name) {
			var compressed bytes.Buffer
			writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
			if _, err := writer.Write(data); err != nil {
				return fmt.Errorf("compress %s: %w", name, err)
			}
			if err := writer.Close(); err != nil {
				return fmt.Errorf("compress %s: %w", name, err)
			}
			if compressed.Len() < len(data) {
				asset.gzipData = compressed.Bytes()
				asset.gzipETag = assetETag(asset.gzipData)
			}
		}
		prepared[name] = asset
		return nil
	})
	if err != nil {
		return nil, err
	}
	if manifest != nil {
		for name, version := range manifest.Versions {
			asset, ok := prepared[name]
			if !ok || !strings.HasPrefix(name, "_app/assets/") || !fs.ValidPath(name) || version != asset.etag[8:40] {
				return nil, fmt.Errorf("asset manifest version does not match %q", name)
			}
		}
		for name := range prepared {
			if strings.HasPrefix(name, "_app/assets/") && manifest.Versions[name] == "" {
				return nil, fmt.Errorf("asset manifest is missing %q", name)
			}
		}
	}
	return prepared, nil
}

func assetETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"sha256-` + hex.EncodeToString(sum[:16]) + `"`
}

func compressibleAsset(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".js", ".css", ".svg", ".json", ".webmanifest":
		return true
	default:
		return false
	}
}

func acceptsGzip(value string) bool {
	wildcard := false
	for _, encoding := range strings.Split(value, ",") {
		parts := strings.Split(encoding, ";")
		name := strings.TrimSpace(parts[0])
		if !strings.EqualFold(name, "gzip") && name != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		if strings.EqualFold(name, "gzip") {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}

func etagMatches(value, etag string) bool {
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// 内嵌清单由浏览器路由器的定义生成。
var consolePaths = func() []string {
	data, err := fs.ReadFile(webui.Assets(), "console-routes.json")
	if err != nil {
		panic(err)
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		panic(err)
	}
	return paths
}()

func consoleRoute(p string) bool {
	for _, pattern := range consolePaths {
		if prefix, dynamic := strings.CutSuffix(pattern, "{id}"); dynamic {
			if strings.HasPrefix(p, prefix) && uuid.ValidUUID(strings.TrimPrefix(p, prefix)) {
				return true
			}
		} else if p == pattern {
			return true
		}
	}
	return false
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if (p == "/" || p == "/public.html") && !h.publicPageRoot(w, r) {
		return
	}
	name := strings.TrimPrefix(p, "/")
	uiRoute := consoleRoute(p)
	if uiRoute {
		name = "200.html"
	}
	if p == "/" {
		name = "public.html"
	}
	if name == "asset-manifest.json" || !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	asset, ok := h.staticAssets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if uiRoute || strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", h.htmlPolicies[name])
		w.Header().Set("X-Frame-Options", "DENY")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasPrefix(name, "_app/assets/") {
			query, err := url.ParseQuery(r.URL.RawQuery)
			versions, versioned := query["v"]
			if err != nil || (versioned && (len(versions) != 1 || versions[0] != asset.etag[8:40])) {
				w.Header().Set("Cache-Control", "no-store")
				http.NotFound(w, r)
				return
			}
			if versioned {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
	data := asset.data
	etag := asset.etag
	if asset.gzipData != nil {
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			data = asset.gzipData
			etag = asset.gzipETag
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

var scriptElement = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
var scriptSource = regexp.MustCompile(`(?i)(?:^|\s)src\s*=`)

func htmlPolicies(assets fs.FS, origin string) (map[string]string, error) {
	_, assetOrigin, err := readAssetManifest(assets)
	if err != nil {
		return nil, err
	}
	return htmlPoliciesWithAssetOrigin(assets, origin, assetOrigin)
}

func htmlPoliciesWithAssetOrigin(assets fs.FS, origin, assetOrigin string) (map[string]string, error) {
	assetSource := ""
	if assetOrigin != "" {
		assetSource = " " + assetOrigin
	}
	policies := make(map[string]string)
	websocketOrigin := "ws" + strings.TrimPrefix(origin, "http")
	err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".html") {
			return nil
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		policy := "default-src 'self'; script-src 'self'" + assetSource
		seen := make(map[string]bool)
		for _, script := range scriptElement.FindAllSubmatch(data, -1) {
			if scriptSource.Match(script[1]) {
				continue
			}
			sum := sha256.Sum256(script[2])
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !seen[hash] {
				policy += " " + hash
				seen[hash] = true
			}
		}
		policies[name] = policy + "; style-src 'self'" + assetSource + "; img-src 'self' data:" + assetSource + "; font-src 'self'" + assetSource + "; connect-src 'self' " + websocketOrigin + "; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
		return nil
	})
	return policies, err
}
