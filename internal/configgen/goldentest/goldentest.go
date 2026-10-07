// Package goldentest holds the fixtures and comparison for the generated-Caddy
// golden tests. Each layer's own test package renders the fixture services and
// checks the files it wrote against internal/configgen/testdata/golden/want,
// so a refactor of how blocks are produced cannot silently change what lands
// on disk.
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

// Check compares root/caddy/<confDir> with the expected files for svc, or
// rewrites them when HOMELAB_UPDATE_GOLDEN is set. Lines are compared with
// surrounding whitespace trimmed: Caddy ignores indentation.
func Check(t *testing.T, root, svc, confDir string) {
	t.Helper()
	got := readDir(t, filepath.Join(root, "caddy", confDir))
	wantDir := filepath.Join(dataDir(), "want", svc, confDir)

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

	want := readDir(t, wantDir)
	if keys(got) != keys(want) {
		t.Fatalf("%s/%s: files %s, want %s", svc, confDir, keys(got), keys(want))
	}
	for name := range want {
		if normalize(got[name]) != normalize(want[name]) {
			t.Errorf("%s/%s/%s:\n--- got\n%s--- want\n%s", svc, confDir, name, got[name], want[name])
		}
	}
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

func normalize(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}
