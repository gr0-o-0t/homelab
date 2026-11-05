package agentstate

import (
	"os"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/secrets"
)

const testSecret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func newVault(t *testing.T, p Paths) *secrets.Manager {
	t.Helper()
	sm, err := secrets.OpenVault(p.VaultFile, p.KeyFile)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	return sm
}

func TestAt_Layout(t *testing.T) {
	p := At("/x")
	want := Paths{
		StateDir:    "/x/state",
		VaultFile:   "/x/vault.bin",
		KeyFile:     "/x/vault.key",
		ClusterFile: "/x/cluster.yaml",
	}
	if p != want {
		t.Fatalf("At = %+v, want %+v", p, want)
	}
	if Root != "/var/lib/homelab" {
		t.Fatalf("Root = %q", Root)
	}
}

func TestCluster_RoundTrip(t *testing.T) {
	p := At(t.TempDir())
	sm := newVault(t, p)
	in := Cluster{Secret: testSecret, LeaderAddr: "100.64.0.1:2377", TSHostname: "minis"}
	if err := SaveCluster(p, sm, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadCluster(p, sm)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	st, err := os.Stat(p.ClusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

func TestCluster_SecretNotInYAML(t *testing.T) {
	p := At(t.TempDir())
	sm := newVault(t, p)
	if err := SaveCluster(p, sm, Cluster{Secret: testSecret, LeaderAddr: "100.64.0.1:2377", TSHostname: "minis"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.ClusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testSecret) {
		t.Fatalf("secret leaked into cluster.yaml:\n%s", b)
	}
	got, err := sm.Get("", "CLUSTER_SECRET")
	if err != nil || got != testSecret {
		t.Fatalf("vault secret = %q, %v", got, err)
	}
}

func TestLoadCluster_Missing(t *testing.T) {
	p := At(t.TempDir())
	sm := newVault(t, p)
	c, err := LoadCluster(p, sm)
	if err != nil || c != (Cluster{}) {
		t.Fatalf("got %+v, %v; want zero, nil", c, err)
	}
	// Secret present but cluster.yaml gone: inconsistent, an error.
	if err := sm.Set("", "CLUSTER_SECRET", testSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCluster(p, sm); err == nil {
		t.Fatal("want error when secret exists without cluster.yaml")
	}
}
