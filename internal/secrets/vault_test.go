package secrets_test

import (
	"path/filepath"
	"testing"

	"github.com/groot/homelab/internal/secrets"
)

func TestOpenVault_ManagerNamespaces(t *testing.T) {
	d := t.TempDir()
	m, err := secrets.OpenVault(filepath.Join(d, "v.enc"), filepath.Join(d, "v.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set("", "CF", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("gitea", "PW", "pw"); err != nil {
		t.Fatal(err)
	}
	if v, err := m.Get("", "CF"); err != nil || v != "tok" {
		t.Fatalf("root get = %q, %v", v, err)
	}
	if v, err := m.Get("gitea", "PW"); err != nil || v != "pw" {
		t.Fatalf("svc get = %q, %v", v, err)
	}
	if v, _ := m.Get("", "MISSING"); v != "" {
		t.Fatalf("missing = %q", v)
	}
}

// A key the vault does not hold reads as ("", nil), like the keyring
// backends: the agent reads optional secrets (CF_TUNNEL_TOKEN, TS_AUTHKEY) on
// every reload, and an error there would fail each one.
func TestOpenVault_MissingKeyIsEmptyNotError(t *testing.T) {
	d := t.TempDir()
	m, err := secrets.OpenVault(filepath.Join(d, "v.enc"), filepath.Join(d, "v.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"", "gitea"} {
		v, err := m.Get(ns, "MISSING")
		if err != nil || v != "" {
			t.Fatalf("Get(%q, MISSING) = %q, %v; want \"\", nil", ns, v, err)
		}
		if m.IsSet(ns, "MISSING") {
			t.Fatalf("IsSet(%q, MISSING) = true", ns)
		}
	}
}

func TestOpen_UsesVaultWhenEnvSet(t *testing.T) {
	d := t.TempDir()
	data, key := filepath.Join(d, "v.enc"), filepath.Join(d, "v.key")
	t.Setenv("HOMELAB_VAULT", data)
	t.Setenv("HOMELAB_VAULT_KEY", key)
	m, err := secrets.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Set("", "X", "y"); err != nil {
		t.Fatal(err)
	}
	m2, err := secrets.OpenVault(data, key)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.Get("", "X"); v != "y" {
		t.Fatalf("vault not used by Open: %q", v)
	}
}
