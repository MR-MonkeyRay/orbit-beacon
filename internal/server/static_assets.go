package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type staticAsset struct {
	data        []byte
	gzipData    []byte
	contentType string
	etag        string
	gzipETag    string
}

func prepareStaticAssets(assets fs.FS) (map[string]staticAsset, error) {
	prepared := make(map[string]staticAsset)
	err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
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
