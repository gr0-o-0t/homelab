package secrets

import (
	"path/filepath"
	"testing"
)

func TestOpenVault_NoFallbackWarning(t *testing.T) {
	d := t.TempDir()
	m, err := OpenVault(filepath.Join(d, "v.enc"), filepath.Join(d, "v.key"))
	if err != nil {
		t.Fatal(err)
	}
	if w := m.fallbackWarning(); w != "" {
		t.Fatalf("unexpected warning: %q", w)
	}
}
