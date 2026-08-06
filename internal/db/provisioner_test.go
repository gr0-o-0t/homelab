package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

		// Verify expected docker exec calls were made. The database is created
		// owned by the service role (not merely granted to it), and the public
		// schema is handed over too — since PG 15 it belongs to
		// pg_database_owner and is not world-writable.
		var hasCreateDB, hasCreateUser, hasSchemaOwner, hasExtension bool
		for _, call := range fc.runCalls {
			line := call.Name + " " + strings.Join(call.Args, " ")
			if strings.Contains(line, `CREATE DATABASE "testdb" OWNER "testuser"`) {
				hasCreateDB = true
			}
			if strings.Contains(line, "CREATE USER") && strings.Contains(line, "testuser") {
				hasCreateUser = true
			}
			if strings.Contains(line, `ALTER SCHEMA public OWNER TO "testuser"`) {
				hasSchemaOwner = true
			}
			if strings.Contains(line, "CREATE EXTENSION") && strings.Contains(line, "pgcrypto") {
				hasExtension = true
			}
		}
		if !hasCreateDB {
			t.Error("missing CREATE DATABASE … OWNER call")
		}
		if !hasCreateUser {
			t.Error("missing CREATE USER call")
		}
		if !hasSchemaOwner {
			t.Error("missing ALTER SCHEMA public OWNER call")
		}
		if !hasExtension {
			t.Error("missing CREATE EXTENSION call")
		}
	})

	// The role has to exist before CREATE DATABASE can name it as OWNER.
	t.Run("postgres creates the role before the database", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc, SM: &fakeSM{store: make(map[string]string)}}
		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		userIdx, dbIdx := -1, -1
		for i, call := range fc.runCalls {
			line := strings.Join(call.Args, " ")
			if userIdx < 0 && strings.Contains(line, "CREATE USER") {
				userIdx = i
			}
			if dbIdx < 0 && strings.Contains(line, "CREATE DATABASE") {
				dbIdx = i
			}
		}
		if userIdx < 0 || dbIdx < 0 {
			t.Fatalf("expected both CREATE USER and CREATE DATABASE (got %d, %d)", userIdx, dbIdx)
		}
		if userIdx > dbIdx {
			t.Error("CREATE DATABASE … OWNER would fail: role is created after the database")
		}
	})

	// An existing database predating the ownership model must be transferred,
	// not left owned by postgres with the service holding bare grants.
	t.Run("postgres transfers ownership of an existing database", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_database WHERE datname='testdb'": []byte("1"),
			},
		}
		p := &Provisioner{RC: fc, SM: &fakeSM{store: make(map[string]string)}}
		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		var altered bool
		for _, call := range fc.runCalls {
			if strings.Contains(strings.Join(call.Args, " "), `ALTER DATABASE "testdb" OWNER TO "testuser"`) {
				altered = true
			}
		}
		if !altered {
			t.Error("existing database should be transferred with ALTER DATABASE … OWNER TO")
		}
	})

	// PostgreSQL's CREATE ROLE/CREATE USER grammar has no IF NOT EXISTS clause,
	// so emitting one is a syntax error (SQLSTATE 42601) and provisioning fails
	// for every Postgres-backed service. The role must be looked up first.
	t.Run("postgres never emits CREATE USER IF NOT EXISTS", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{RC: fc, SM: &fakeSM{store: make(map[string]string)}}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		for _, call := range fc.runCalls {
			line := strings.Join(call.Args, " ")
			if strings.Contains(line, "CREATE USER") && strings.Contains(line, "IF NOT EXISTS") {
				t.Errorf("invalid PostgreSQL syntax: %s", line)
			}
		}
	})

	t.Run("postgres alters an existing role instead of recreating it", func(t *testing.T) {
		fc := &fakeCommander{
			outputData: map[string][]byte{
				"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_roles WHERE rolname='testuser'": []byte("1"),
			},
		}
		p := &Provisioner{RC: fc, SM: &fakeSM{store: make(map[string]string)}}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		var altered, created bool
		for _, call := range fc.runCalls {
			line := strings.Join(call.Args, " ")
			if strings.Contains(line, `ALTER USER "testuser" WITH LOGIN PASSWORD`) {
				altered = true
			}
			if strings.Contains(line, `CREATE USER "testuser"`) {
				created = true
			}
		}
		if !altered {
			t.Error("existing role should be updated with ALTER USER … PASSWORD")
		}
		if created {
			t.Error("existing role should not be issued a CREATE USER")
		}
	})

	// The MariaDB account identity includes the host, so a DROP that spells the
	// wildcard differently from the CREATE silently leaves the user behind.
	t.Run("mariadb create and drop agree on the wildcard host", func(t *testing.T) {
		fcCreate := &fakeCommander{}
		pc := &Provisioner{RC: fcCreate, SM: &fakeSM{store: make(map[string]string)}}
		if err := pc.Provision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		fcDrop := &fakeCommander{}
		pd := &Provisioner{RC: fcDrop, SM: &fakeSM{store: make(map[string]string)}}
		if err := pd.Deprovision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		const account = "'testuser'@'%'"
		joined := func(f *fakeCommander) string {
			var all string
			for _, c := range f.runCalls {
				all += strings.Join(c.Args, " ") + "\n"
			}
			return all
		}
		if !strings.Contains(joined(fcCreate), "CREATE USER IF NOT EXISTS "+account) {
			t.Errorf("CREATE did not use %s:\n%s", account, joined(fcCreate))
		}
		if !strings.Contains(joined(fcDrop), "DROP USER IF EXISTS "+account) {
			t.Errorf("DROP did not use %s:\n%s", account, joined(fcDrop))
		}
	})

	// Immich upgrades its own vector extensions on every start and backs up
	// with pg_dumpall, so its role has to be a superuser. A silently dropped
	// ALTER shows up much later as an Immich startup failure.
	t.Run("postgres grants superuser only when asked", func(t *testing.T) {
		const wantSQL = `ALTER USER "testuser" WITH SUPERUSER`

		for _, tc := range []struct {
			name      string
			superuser bool
			want      bool
		}{
			{"not requested", false, false},
			{"requested", true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fc := &fakeCommander{}
				p := &Provisioner{RC: fc, SM: &fakeSM{store: make(map[string]string)}}

				su := decl
				su.Superuser = tc.superuser
				if err := p.Provision(ctx, config.DBPostgres, "mysvc", su); err != nil {
					t.Fatal(err)
				}

				var got bool
				for _, call := range fc.runCalls {
					if strings.Contains(strings.Join(call.Args, " "), wantSQL) {
						got = true
					}
				}
				if got != tc.want {
					t.Errorf("ALTER USER … SUPERUSER issued = %v, want %v", got, tc.want)
				}
			})
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

// `docker compose up -d` returns before Postgres accepts connections, so
// auto-started dependencies must be waited on or the dependent service comes up
// against a database that is not listening yet.
func TestWaitHealthy(t *testing.T) {
	ctx := context.Background()
	const inspectKey = "docker inspect --format={{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}} homelab-postgres"

	t.Run("returns once healthy", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspectKey: []byte("healthy\n")}}
		p := &Provisioner{RC: fc, PollInterval: time.Millisecond}

		if err := p.WaitHealthy(ctx, config.DBPostgres, time.Second); err != nil {
			t.Fatal(err)
		}
	})

	// Containers without a healthcheck report no health state; "running" is then
	// the only signal available and must be accepted rather than timing out.
	t.Run("accepts running when no healthcheck is defined", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspectKey: []byte("running\n")}}
		p := &Provisioner{RC: fc, PollInterval: time.Millisecond}

		if err := p.WaitHealthy(ctx, config.DBPostgres, time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("times out with actionable guidance", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspectKey: []byte("starting\n")}}
		p := &Provisioner{RC: fc, PollInterval: time.Millisecond}

		err := p.WaitHealthy(ctx, config.DBPostgres, 5*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout")
		}
		for _, want := range []string{"did not become healthy", "starting", "homelab logs postgres", "homelab setup postgres"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	})

	t.Run("honours context cancellation", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspectKey: []byte("starting\n")}}
		p := &Provisioner{RC: fc, PollInterval: time.Hour}

		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.WaitHealthy(cctx, config.DBPostgres, time.Hour); !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("rejects unknown db type", func(t *testing.T) {
		p := &Provisioner{RC: &fakeCommander{}, PollInterval: time.Millisecond}
		if err := p.WaitHealthy(ctx, config.DBType("mongo"), time.Second); err == nil {
			t.Fatal("expected an error for an unsupported engine")
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
