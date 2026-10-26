package architecture_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "orbitbeacon"

// allowedInternalImports is the repository's intentionally small dependency
// graph. Every internal package must be listed so a new package cannot silently
// bypass the boundary review.
var allowedInternalImports = map[string]map[string]bool{
	"orbitbeacon/internal/architecture": {},
	"orbitbeacon/internal/agent/collect": {
		"orbitbeacon/internal/protocol/v1": true,
	},
	"orbitbeacon/internal/agent/filelock": {},
	"orbitbeacon/internal/agent/identity": {
		"orbitbeacon/internal/agent/filelock": true,
	},
	"orbitbeacon/internal/agent/service": {
		"orbitbeacon/internal/agent/collect":  true,
		"orbitbeacon/internal/agent/filelock": true,
		"orbitbeacon/internal/agent/identity": true,
		"orbitbeacon/internal/config":         true,
		"orbitbeacon/internal/protocol/v1":    true,
	},
	"orbitbeacon/internal/config":      {},
	"orbitbeacon/internal/identity":    {},
	"orbitbeacon/internal/protocol/v1": {},
	"orbitbeacon/internal/timekey":     {},
	"orbitbeacon/internal/server": {
		"orbitbeacon/internal/config":            true,
		"orbitbeacon/internal/identity":          true,
		"orbitbeacon/internal/protocol/v1":       true,
		"orbitbeacon/internal/server/audit":      true,
		"orbitbeacon/internal/server/auth":       true,
		"orbitbeacon/internal/server/enrollment": true,
		"orbitbeacon/internal/server/ingest":     true,
		"orbitbeacon/internal/server/management": true,
		"orbitbeacon/internal/server/nodes":      true,
		"orbitbeacon/internal/server/storage":    true,
		"orbitbeacon/internal/server/webui":      true,
	},
	"orbitbeacon/internal/server/audit": {
		"orbitbeacon/internal/identity": true,
		"orbitbeacon/internal/timekey":  true,
	},
	"orbitbeacon/internal/server/auth": {
		"orbitbeacon/internal/identity":          true,
		"orbitbeacon/internal/server/audit":      true,
		"orbitbeacon/internal/server/management": true,
		"orbitbeacon/internal/timekey":           true,
	},
	"orbitbeacon/internal/server/enrollment": {
		"orbitbeacon/internal/identity":     true,
		"orbitbeacon/internal/protocol/v1":  true,
		"orbitbeacon/internal/server/audit": true,
		"orbitbeacon/internal/server/nodes": true,
		"orbitbeacon/internal/timekey":      true,
	},
	"orbitbeacon/internal/server/ingest": {
		"orbitbeacon/internal/protocol/v1":  true,
		"orbitbeacon/internal/server/nodes": true,
		"orbitbeacon/internal/timekey":      true,
	},
	"orbitbeacon/internal/server/management": {
		"orbitbeacon/internal/identity":     true,
		"orbitbeacon/internal/server/audit": true,
		"orbitbeacon/internal/timekey":      true,
	},
	"orbitbeacon/internal/server/nodes": {
		"orbitbeacon/internal/identity":       true,
		"orbitbeacon/internal/protocol/v1":    true,
		"orbitbeacon/internal/server/audit":   true,
		"orbitbeacon/internal/server/storage": true,
		"orbitbeacon/internal/timekey":        true,
	},
	"orbitbeacon/internal/server/securefs": {},
	"orbitbeacon/internal/server/storage": {
		"orbitbeacon/internal/config":          true,
		"orbitbeacon/internal/identity":        true,
		"orbitbeacon/internal/protocol/v1":     true,
		"orbitbeacon/internal/server/securefs": true,
		"orbitbeacon/internal/timekey":         true,
	},
	"orbitbeacon/internal/server/webui": {},
}

type packageJSON struct {
	ImportPath string
	Imports    []string
}

func TestInternalDependencyBoundaries(t *testing.T) {
	root := moduleRoot(t)
	command := exec.Command("go", "list", "-json", "./internal/...")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list dependency graph: %v\n%s", err, output)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	seen := make(map[string]bool)
	for {
		var pkg packageJSON
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode go list package: %v", err)
		}
		seen[pkg.ImportPath] = true
		allowed, declared := allowedInternalImports[pkg.ImportPath]
		if !declared {
			t.Errorf("internal package %s has no dependency policy", pkg.ImportPath)
			continue
		}
		for _, imported := range pkg.Imports {
			if !strings.HasPrefix(imported, modulePath+"/internal/") {
				continue
			}
			if !allowed[imported] {
				t.Errorf("%s imports forbidden internal package %s", pkg.ImportPath, imported)
			}
		}
	}
	for pkg := range allowedInternalImports {
		if !seen[pkg] {
			t.Errorf("dependency policy lists missing internal package %s", pkg)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("go.mod not found from architecture test directory")
		}
		directory = parent
	}
}
