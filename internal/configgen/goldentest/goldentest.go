// Package goldentest holds the fixtures and comparison for the generated-Caddy
// golden tests. Each layer's own test package renders the fixture services and
// checks the blocks against internal/configgen/testdata/golden/want/<svc>/
// <legacy conf dir>/, and internal/routing checks that a service's sites file
// is exactly those blocks in registry and port order — so a refactor of how
// blocks are produced cannot silently change what Caddy loads.
//
// Set HOMELAB_UPDATE_GOLDEN=1 to rewrite the expected files from the current
// output instead of comparing.
package goldentest

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/configgen"
)

// Services are the fixture services: one port; several ports including a named
// subdomain and a udp port; an HTTP port plus a raw listen port (22:22); and a
// caddy.routes.conf service.
var Services = []string{"single", "multi", "listen", "routes"}

func dataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", "golden")
}

// Install copies the fixture service svc into root/services/svc.
func Install(t *testing.T, root, svc string) {
	t.Helper()
	src := filepath.Join(dataDir(), "services", svc)
	dst := filepath.Join(root, "services", svc)
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // test fixture
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Check compares the blocks one layer rendered for svc with the expected
// files in want/<svc>/<dir> — one per block, named after its port as the
// per-layer files were (configgen.PortFileName) — or rewrites them when
// HOMELAB_UPDATE_GOLDEN is set. Lines are compared with surrounding whitespace
// trimmed: Caddy ignores indentation.
func Check(t *testing.T, svc, dir string, blocks []configgen.CaddyBlock) {
	t.Helper()
	got := map[string]string{}
	for _, b := range blocks {
		got[configgen.PortFileName(svc, b.PortName)+".conf"] = b.Content
	}
	wantDir := filepath.Join(dataDir(), "want", svc, dir)

	if os.Getenv("HOMELAB_UPDATE_GOLDEN") != "" {
		_ = os.RemoveAll(wantDir)
		if len(got) == 0 {
			return
		}
		if err := os.MkdirAll(wantDir, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, data := range got {
			if err := os.WriteFile(filepath.Join(wantDir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	want := Want(t, svc, dir)
	if keys(got) != keys(want) {
		t.Fatalf("%s/%s: files %s, want %s", svc, dir, keys(got), keys(want))
	}
	for name := range want {
		if Normalize(got[name]) != Normalize(want[name]) {
			t.Errorf("%s/%s/%s:\n--- got\n%s--- want\n%s", svc, dir, name, got[name], want[name])
		}
	}
}

// Want returns the expected blocks of svc for one layer, by file name.
func Want(t *testing.T, svc, dir string) map[string]string {
	t.Helper()
	return readDir(t, filepath.Join(dataDir(), "want", svc, dir))
}

func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // test dir
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func keys(m map[string]string) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// Normalize trims every line, since Caddy ignores indentation.
func Normalize(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}
