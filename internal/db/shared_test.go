package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/config"
)

// installSvc creates services/<name>/ so the allocator sees it as installed.
func installSvc(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "services", name), 0o750); err != nil {
		t.Fatal(err)
	}
}

func redisProvisioner(t *testing.T) (*Provisioner, *fakeCommander, string) {
	t.Helper()
	dir := t.TempDir()
	fc := &fakeCommander{}
	sm := &fakeSM{store: map[string]string{"redis/" + config.RedisPasswordKey: "sharedpw"}}
	return &Provisioner{ConfigDir: dir, RC: fc, SM: sm}, fc, dir
}

func TestProvisionRedis_AllocatesStableUniqueNumbers(t *testing.T) {
	ctx := context.Background()
	p, fc, dir := redisProvisioner(t)

	for _, svc := range []string{"immich", "searxng", "immich"} {
		installSvc(t, dir, svc)
		if err := p.Provision(ctx, config.DBRedis, svc, config.ServiceDBDecl{}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := config.LoadRedisDBs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m["immich"] != 1 || m["searxng"] != 2 || len(m) != 2 {
		t.Fatalf("allocations = %v, want immich:1 searxng:2", m)
	}

	// Only a fresh number is flushed; the repeat provision of immich is not.
	var flushes []string
	for _, c := range fc.outputCalls {
		if strings.Contains(c.Stdin, "FLUSHDB") {
			flushes = append(flushes, c.Stdin)
		}
	}
	if len(flushes) != 2 {
		t.Fatalf("FLUSHDB sent %d times, want 2 (fresh allocations only)", len(flushes))
	}
	// The password travels over stdin, never argv.
	for _, c := range fc.outputCalls {
		if strings.Contains(strings.Join(c.Args, " "), "sharedpw") {
			t.Errorf("password leaked into argv: %v", c.Args)
		}
		if !strings.HasPrefix(c.Stdin, "AUTH sharedpw\nSELECT ") {
			t.Errorf("redis-cli script = %q", c.Stdin)
		}
	}
}

func TestProvisionRedis_MultiInstance(t *testing.T) {
	ctx := context.Background()
	p, _, dir := redisProvisioner(t)
	installSvc(t, dir, "netbox")

	for _, name := range []string{"tasks", "cache"} {
		if err := p.Provision(ctx, config.DBRedis, "netbox", config.ServiceDBDecl{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := config.LoadRedisDBs(dir)
	if m["netbox/tasks"] == 0 || m["netbox/cache"] == 0 || m["netbox/tasks"] == m["netbox/cache"] {
		t.Fatalf("netbox instances need distinct numbers, got %v", m)
	}
}

func TestDeprovisionRedis_FreesNumberForReuse(t *testing.T) {
	ctx := context.Background()
	p, _, dir := redisProvisioner(t)
	for _, svc := range []string{"a", "b"} {
		installSvc(t, dir, svc)
		if err := p.Provision(ctx, config.DBRedis, svc, config.ServiceDBDecl{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Deprovision(ctx, config.DBRedis, "a", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	m, _ := config.LoadRedisDBs(dir)
	if _, ok := m["a"]; ok {
		t.Fatalf("a still allocated after deprovision: %v", m)
	}
	installSvc(t, dir, "c")
	if err := p.Provision(ctx, config.DBRedis, "c", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	m, _ = config.LoadRedisDBs(dir)
	if m["c"] != 1 || m["b"] != 2 {
		t.Fatalf("freed number not reused: %v", m)
	}
}

func TestAllocateRedisDB_NeverTakesFromInstalledService(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i < config.RedisDBCount; i++ {
		svc := fmt.Sprintf("svc%02d", i)
		if i == 1 {
			svc = "gone" // never installed: reclaimable
		} else {
			installSvc(t, dir, svc)
		}
		if _, _, err := config.AllocateRedisDB(dir, svc, ""); err != nil {
			t.Fatal(err)
		}
	}
	installSvc(t, dir, "new")
	n, fresh, err := config.AllocateRedisDB(dir, "new", "")
	if err != nil || n != 1 || !fresh {
		t.Fatalf("got (%d, %v, %v), want the stale entry's number 1", n, fresh, err)
	}
	if _, _, err := config.AllocateRedisDB(dir, "another", ""); err == nil {
		t.Fatal("expected exhaustion error: every remaining owner is installed")
	}
}

func TestProvisionRedis_RejectsUnauthenticatedInstance(t *testing.T) {
	p, fc, dir := redisProvisioner(t)
	installSvc(t, dir, "searxng")
	fc.outputData = map[string][]byte{
		"docker exec -i homelab-redis redis-cli": []byte("ERR AUTH <password> called without any password configured\nOK\nOK\n"),
	}
	err := p.Provision(context.Background(), config.DBRedis, "searxng", config.ServiceDBDecl{})
	if err == nil || !strings.Contains(err.Error(), "homelab up redis") {
		t.Fatalf("want a recreate-redis error, got %v", err)
	}
}

func TestProvisionRedis_NeedsSharedPassword(t *testing.T) {
	p, _, dir := redisProvisioner(t)
	p.SM = &fakeSM{store: map[string]string{}}
	installSvc(t, dir, "searxng")
	if err := p.Provision(context.Background(), config.DBRedis, "searxng", config.ServiceDBDecl{}); err == nil {
		t.Fatal("expected an error with no shared redis password")
	}
}

// ── Garage ────────────────────────────────────────────────────────────────────

// fakeGarage models just enough Garage state for the provisioner: layout
// version, buckets and keys.
type fakeGarage struct {
	layout  int
	buckets map[string]bool
	keys    map[string]bool
	created int
}

const gx = "docker exec homelab-garage /garage "

func (g *fakeGarage) answer(key string) ([]byte, error, bool) {
	if !strings.HasPrefix(key, gx) {
		return nil, nil, false
	}
	args := strings.Fields(strings.TrimPrefix(key, gx))
	notFound := errors.New("exit 1")
	switch {
	case args[0] == "layout" && args[1] == "show":
		return []byte("==== CURRENT CLUSTER LAYOUT ====\n\nCurrent cluster layout version: " +
			string(rune('0'+g.layout)) + "\n"), nil, true
	case args[0] == "node":
		return []byte("50cbb72b3332ae30aaaa@127.0.0.1:3901\n"), nil, true
	case args[0] == "layout" && args[1] == "apply":
		g.layout = 1
		return nil, nil, true
	case args[0] == "bucket" && args[1] == "info":
		if !g.buckets[args[2]] {
			return nil, notFound, true
		}
		return nil, nil, true
	case args[0] == "bucket" && args[1] == "create":
		g.buckets[args[2]] = true
		return nil, nil, true
	case args[0] == "key" && args[1] == "info":
		if !g.keys[args[2]] {
			return nil, notFound, true
		}
		return nil, nil, true
	case args[0] == "key" && args[1] == "create":
		g.created++
		id := "GK00000000000000000000000" + string(rune('0'+g.created))
		g.keys[id] = true
		return []byte("==== ACCESS KEY INFORMATION ====\nKey ID:              " + id +
			"\nKey name:            " + args[2] + "\nSecret key:          s3cr3t" + string(rune('0'+g.created)) + "\n"), nil, true
	case args[0] == "key" && args[1] == "delete":
		delete(g.keys, args[len(args)-1])
		return nil, nil, true
	}
	return nil, nil, true
}

func garageProvisioner() (*Provisioner, *fakeCommander, *fakeGarage, *fakeSM) {
	g := &fakeGarage{buckets: map[string]bool{}, keys: map[string]bool{}}
	fc := &fakeCommander{outputFn: g.answer}
	sm := &fakeSM{store: map[string]string{}}
	return &Provisioner{RC: fc, SM: sm}, fc, g, sm
}

func garageArgs(fc *fakeCommander) []string {
	var out []string
	for _, c := range fc.outputCalls {
		line := c.Name + " " + strings.Join(c.Args, " ")
		out = append(out, strings.TrimPrefix(line, gx))
	}
	return out
}

func TestProvisionGarage_FirstRunSequence(t *testing.T) {
	p, fc, _, sm := garageProvisioner()
	if err := p.Provision(context.Background(), config.DBS3, "appflowy", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"layout show",
		"node id -q",
		"layout assign -z dc1 -c 1T 50cbb72b3332ae30aaaa",
		"layout apply --version 1",
		"bucket info appflowy",
		"bucket create appflowy",
		"key create appflowy",
		"bucket allow --read --write --owner appflowy --key GK000000000000000000000001",
	}
	if got := garageArgs(fc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("garage calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if sm.store["appflowy/"+config.GarageAccessKeyIDKey] != "GK000000000000000000000001" ||
		sm.store["appflowy/"+config.GarageSecretKeyKey] != "s3cr3t1" {
		t.Fatalf("key not stored under the service namespace: %v", sm.store)
	}
}

func TestProvisionGarage_SecondRunReusesEverything(t *testing.T) {
	p, fc, g, _ := garageProvisioner()
	ctx := context.Background()
	decl := config.ServiceDBDecl{Bucket: "files"}
	if err := p.Provision(ctx, config.DBS3, "svc", decl); err != nil {
		t.Fatal(err)
	}
	fc.outputCalls = nil
	if err := p.Provision(ctx, config.DBS3, "svc", decl); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"layout show",
		"bucket info files",
		"key info GK000000000000000000000001",
		"bucket allow --read --write --owner files --key GK000000000000000000000001",
	}
	if got := garageArgs(fc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("garage calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if g.created != 1 {
		t.Fatalf("created %d keys, want 1", g.created)
	}
}

func TestProvisionGarage_ReplacesKeyGarageForgot(t *testing.T) {
	p, _, g, sm := garageProvisioner()
	ctx := context.Background()
	if err := p.Provision(ctx, config.DBS3, "svc", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	g.keys = map[string]bool{} // metadata volume wiped
	if err := p.Provision(ctx, config.DBS3, "svc", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	if sm.store["svc/"+config.GarageAccessKeyIDKey] != "GK000000000000000000000002" {
		t.Fatalf("stale key kept: %v", sm.store)
	}
}

func TestDeprovisionGarage_DeletesKeyKeepsBucket(t *testing.T) {
	p, fc, g, sm := garageProvisioner()
	ctx := context.Background()
	if err := p.Provision(ctx, config.DBS3, "svc", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	fc.outputCalls = nil
	if err := p.Deprovision(ctx, config.DBS3, "svc", config.ServiceDBDecl{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range garageArgs(fc) {
		if strings.HasPrefix(c, "bucket delete") {
			t.Errorf("deprovision must keep the bucket, ran %q", c)
		}
	}
	if len(g.keys) != 0 || !g.buckets["svc"] {
		t.Fatalf("keys=%v buckets=%v, want key gone and bucket kept", g.keys, g.buckets)
	}
	if _, ok := sm.store["svc/"+config.GarageAccessKeyIDKey]; ok {
		t.Fatal("key id left in keyring")
	}
}

func TestEnsureGeneratedSecrets(t *testing.T) {
	dir := t.TempDir()
	installSvc(t, dir, "garage")
	cfg := []byte("secrets:\n  RPC:\n    required: true\n    generate: hex32\n" +
		"  TOKEN:\n    required: true\n    generate: password\n  TYPED:\n    required: true\n")
	if err := os.WriteFile(config.ServiceConfigFile(dir, "garage"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	sm := &fakeSM{store: map[string]string{"garage/TOKEN": "keep-me"}}
	p := &Provisioner{ConfigDir: dir, SM: sm, RC: &fakeCommander{}}

	created, err := p.EnsureGeneratedSecrets("garage")
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0] != "RPC" {
		t.Fatalf("created %v, want [RPC]", created)
	}
	if rpc := sm.store["garage/RPC"]; len(rpc) != 64 || strings.Trim(rpc, "0123456789abcdef") != "" {
		t.Fatalf("RPC = %q, want 64 hex chars", rpc)
	}
	if sm.store["garage/TOKEN"] != "keep-me" {
		t.Fatal("existing secret was replaced")
	}
	if _, ok := sm.store["garage/TYPED"]; ok {
		t.Fatal("a secret without generate: must be left for the user")
	}
}
