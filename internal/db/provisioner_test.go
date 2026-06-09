package db

import (
	"context"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/config"
)

// ── fakeCommander ─────────────────────────────────────────────────────────────

type fakeCall struct {
	Name string
	Args []string
}

type fakeCommander struct {
	runCalls    []fakeCall
	outputCalls []fakeCall
	outputData  map[string][]byte // keyed by "name arg0 arg1 …"
	outputErr   map[string]error
}

func (f *fakeCommander) Run(name string, args ...string) error {
	f.runCalls = append(f.runCalls, fakeCall{Name: name, Args: args})
	return nil
}

func (f *fakeCommander) Output(name string, args ...string) ([]byte, error) {
	f.outputCalls = append(f.outputCalls, fakeCall{Name: name, Args: args})
	key := name + " " + strings.Join(args, " ")
	if err, ok := f.outputErr[key]; ok {
		return nil, err
	}
	if data, ok := f.outputData[key]; ok {
		return data, nil
	}
	return []byte{}, nil
}

// ── fakeSM ────────────────────────────────────────────────────────────────────

type fakeSM struct {
	store map[string]string
}

func (f *fakeSM) Get(_, key string) (string, error) { return f.store[key], nil }
func (f *fakeSM) Set(_, key, val string) error      { f.store[key] = val; return nil }
func (f *fakeSM) IsSet(_, key string) bool          { _, ok := f.store[key]; return ok }
func (f *fakeSM) Delete(_, key string) error        { delete(f.store, key); return nil }

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestGeneratePassword(t *testing.T) {
	t.Run("length 32", func(t *testing.T) {
		pw := generatePassword(32)
		if len(pw) != 32 {
			t.Fatalf("expected length 32, got %d", len(pw))
		}
	})
	t.Run("length 0", func(t *testing.T) {
		pw := generatePassword(0)
		if len(pw) != 0 {
			t.Fatalf("expected length 0, got %d", len(pw))
		}
	})
	t.Run("charset only", func(t *testing.T) {
		pw := generatePassword(256)
		for _, r := range pw {
			if !strings.ContainsRune(passwordChars, r) {
				t.Fatalf("unexpected char %c", r)
			}
		}
	})
	t.Run("deterministic uniqueness", func(t *testing.T) {
		a := generatePassword(32)
		b := generatePassword(32)
		if a == b {
			t.Fatal("two passwords should be different")
		}
	})
}

func TestEscID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", `"simple"`},
		{`with"quote`, `"with""quote"`},
		{`a"b"c`, `"a""b""c"`},
		{"", `""`},
	}
	for _, tt := range tests {
		got := escID(tt.input)
		if got != tt.want {
			t.Errorf("escID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestContainerName(t *testing.T) {
	tests := []struct {
		dbType config.DBType
		want   string
	}{
		{config.DBPostgres, "homelab-postgres"},
		{config.DBMariaDB, "homelab-mariadb"},
		{config.DBRedis, "homelab-redis"},
		{"unknown", ""},
	}
	for _, tt := range tests {
		p := &Provisioner{}
		got := p.containerName(tt.dbType)
		if got != tt.want {
			t.Errorf("containerName(%q) = %q, want %q", tt.dbType, got, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	p := New("/cfg", nil)
	if p == nil {
		t.Fatal("New() returned nil")
	}
	if p.ConfigDir != "/cfg" {
		t.Errorf("expected ConfigDir /cfg, got %s", p.ConfigDir)
	}
	if p.RC == nil {
		t.Error("expected non-nil RC")
	}
}

func TestEnsurePassword(t *testing.T) {
	t.Run("generates and returns password", func(t *testing.T) {
		sm := &fakeSM{store: make(map[string]string)}
		p := New("/tmp", sm)
		pw, err := p.ensurePassword("mysvc")
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 32 {
			t.Fatalf("expected 32-char password, got %d", len(pw))
		}
		// Verify stored in keyring
		key := config.DBPasswordKey("mysvc")
		if sm.store[key] != pw {
			t.Error("password not stored in keyring")
		}
	})

	t.Run("reuses existing password", func(t *testing.T) {
		sm := &fakeSM{store: map[string]string{
			config.DBPasswordKey("mysvc"): "existing-pass-12345",
		}}
		p := New("/tmp", sm)
		pw, err := p.ensurePassword("mysvc")
		if err != nil {
			t.Fatal(err)
		}
		if pw != "existing-pass-12345" {
			t.Errorf("expected existing password, got %q", pw)
		}
	})

	t.Run("nil SM returns password without storing", func(t *testing.T) {
		p := New("/tmp", nil)
		pw, err := p.ensurePassword("mysvc")
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 32 {
			t.Fatalf("expected 32-char password, got %d", len(pw))
		}
	})
}

func TestEnsureRunning(t *testing.T) {
	ctx := context.Background()

	t.Run("container running", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				"docker inspect --format={{.State.Status}} homelab-postgres": []byte("running"),
			},
		}
		p := &Provisioner{RC: fc}
		err := p.EnsureRunning(ctx, config.DBPostgres)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("container not found", func(t *testing.T) {
		fc := &fakeCommander{
			outputErr: map[string]error{
				"docker inspect --format={{.State.Status}} homelab-postgres": errFake("exit code 1"),
			},
		}
		p := &Provisioner{RC: fc}
		err := p.EnsureRunning(ctx, config.DBPostgres)
		if err == nil {
			t.Fatal("expected error for missing container")
		}
		if !strings.Contains(err.Error(), "homelab add") {
			t.Errorf("error should mention installation, got: %v", err)
		}
	})

	t.Run("container stopped", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				"docker inspect --format={{.State.Status}} homelab-postgres": []byte("exited"),
			},
		}
		p := &Provisioner{RC: fc}
		err := p.EnsureRunning(ctx, config.DBPostgres)
		if err == nil {
			t.Fatal("expected error for stopped container")
		}
		if !strings.Contains(err.Error(), "exited") || !strings.Contains(err.Error(), "homelab up") {
			t.Errorf("error should mention status and service up, got: %v", err)
		}
	})

	t.Run("unknown db type", func(t *testing.T) {
		p := &Provisioner{RC: &fakeCommander{}}
		err := p.EnsureRunning(ctx, config.DBType("foobar"))
		if err == nil {
			t.Fatal("expected error for unknown db type")
		}
	})
}

func TestProvision(t *testing.T) {
	ctx := context.Background()
	decl := config.ServiceDBDecl{
		Database:   "testdb",
		User:       "testuser",
		Extensions: []string{"pgcrypto"},
		Env:        map[string]string{"host": "TEST_DB_HOST"},
	}

	t.Run("postgres provisions DB and user", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				// DB does not exist yet → empty output
				"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_database WHERE datname='testdb'": []byte(""),
			},
		}
		sm := &fakeSM{store: make(map[string]string)}
		p := &Provisioner{RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		// Verify expected docker exec calls were made
		var hasCreateDB, hasCreateUser, hasGrantDB, hasGrantSchema, hasExtension bool
		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, `CREATE DATABASE "testdb"`) {
				hasCreateDB = true
			}
			if strings.Contains(line, "CREATE USER") && strings.Contains(line, "testuser") {
				hasCreateUser = true
			}
			if strings.Contains(line, "GRANT ALL PRIVILEGES ON DATABASE") && strings.Contains(line, "testdb") {
				hasGrantDB = true
			}
			if strings.Contains(line, "GRANT ALL ON SCHEMA public") && strings.Contains(line, "testuser") {
				hasGrantSchema = true
			}
			if strings.Contains(line, "CREATE EXTENSION") && strings.Contains(line, "pgcrypto") {
				hasExtension = true
			}
		}
		if !hasCreateDB {
			t.Error("missing CREATE DATABASE call")
		}
		if !hasCreateUser {
			t.Error("missing CREATE USER call")
		}
		if !hasGrantDB {
			t.Error("missing GRANT ON DATABASE call")
		}
		if !hasGrantSchema {
			t.Error("missing GRANT ON SCHEMA call")
		}
		if !hasExtension {
			t.Error("missing CREATE EXTENSION call")
		}
	})

	t.Run("postgres skips existing DB", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_database WHERE datname='testdb'": []byte("1"),
			},
		}
		sm := &fakeSM{store: make(map[string]string)}
		p := &Provisioner{RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, "CREATE DATABASE") {
				t.Errorf("CREATE DATABASE should not be called for existing DB, got: %s", line)
			}
		}
	})

	t.Run("mariadb provisions DB and user", func(t *testing.T) {
		fc := &fakeCommander{}
		sm := &fakeSM{store: make(map[string]string)}
		p := &Provisioner{RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, "CREATE DATABASE") && strings.Contains(line, "testdb") {
				found = true
			}
		}
		if !found {
			t.Error("missing CREATE DATABASE for mariadb")
		}
	})

	t.Run("redis provision is no-op", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc}
		if err := p.Provision(ctx, config.DBRedis, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		if len(fc.runCalls) > 0 {
			t.Errorf("expected no docker calls for Redis, got %d", len(fc.runCalls))
		}
	})

	t.Run("unsupported db type returns error", func(t *testing.T) {
		p := &Provisioner{RC: &fakeCommander{}}
		err := p.Provision(ctx, config.DBType("mssql"), "mysvc", decl)
		if err == nil {
			t.Fatal("expected error for unsupported db type")
		}
	})
}

func TestDeprovision(t *testing.T) {
	ctx := context.Background()
	decl := config.ServiceDBDecl{
		Database: "testdb",
		User:     "testuser",
	}

	t.Run("postgres drops user", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc}
		if err := p.Deprovision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, "DROP USER") && strings.Contains(line, "testuser") {
				found = true
			}
		}
		if !found {
			t.Error("missing DROP USER for postgres")
		}
	})

	t.Run("mariadb drops user", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc}
		if err := p.Deprovision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, "DROP USER") && strings.Contains(line, "testuser") {
				found = true
			}
		}
		if !found {
			t.Error("missing DROP USER for mariadb")
		}
	})

	t.Run("redis deprovision is no-op", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc}
		if err := p.Deprovision(ctx, config.DBRedis, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		if len(fc.runCalls) > 0 {
			t.Errorf("expected no docker calls for Redis, got %d", len(fc.runCalls))
		}
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

type errString string

func (e errString) Error() string { return string(e) }
func errFake(s string) error      { return errString(s) }
