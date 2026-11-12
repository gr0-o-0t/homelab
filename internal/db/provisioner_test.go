package db

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/groot/homelab/internal/config"
)

// ── fakeCommander ─────────────────────────────────────────────────────────────

type fakeCall struct {
	Name  string
	Args  []string
	Stdin string // what RunFrom fed the command, if anything
}

// line renders the call's argv plus its stdin, so assertions about the SQL
// issued hold whichever channel carried it.
func (c fakeCall) line() string { return strings.Join(c.Args, " ") + " " + c.Stdin }

type fakeCommander struct {
	runCalls    []fakeCall
	outputCalls []fakeCall
	outputData  map[string][]byte // keyed by "name arg0 arg1 …"
	outputErr   map[string]error
	// outputFn, when set, answers Output calls before outputData/outputErr —
	// for fakes whose reply depends on earlier calls (Garage state).
	outputFn func(key string) ([]byte, error, bool)
}

func (f *fakeCommander) Run(name string, args ...string) error {
	f.runCalls = append(f.runCalls, fakeCall{Name: name, Args: args})
	return nil
}

func (f *fakeCommander) RunFrom(r io.Reader, name string, args ...string) error {
	in, _ := io.ReadAll(r)
	f.runCalls = append(f.runCalls, fakeCall{Name: name, Args: args, Stdin: string(in)})
	return nil
}

func (f *fakeCommander) OutputFrom(r io.Reader, name string, args ...string) ([]byte, error) {
	in, _ := io.ReadAll(r)
	f.outputCalls = append(f.outputCalls, fakeCall{Name: name, Args: args, Stdin: string(in)})
	key := name + " " + strings.Join(args, " ")
	if err, ok := f.outputErr[key]; ok {
		return nil, err
	}
	if data, ok := f.outputData[key]; ok {
		return data, nil
	}
	// Default: a redis-cli session where every command succeeded.
	return []byte(strings.Repeat("OK\n", strings.Count(string(in), "\n"))), nil
}

func (f *fakeCommander) Output(name string, args ...string) ([]byte, error) {
	f.outputCalls = append(f.outputCalls, fakeCall{Name: name, Args: args})
	key := name + " " + strings.Join(args, " ")
	if f.outputFn != nil {
		if data, err, ok := f.outputFn(key); ok {
			return data, err
		}
	}
	if err, ok := f.outputErr[key]; ok {
		return nil, err
	}
	if data, ok := f.outputData[key]; ok {
		return data, nil
	}
	return []byte{}, nil
}

// ── fakeSM ────────────────────────────────────────────────────────────────────

// fakeSM stores root-namespace keys bare and service keys as "ns/key", so
// tests written against the root namespace read naturally.
type fakeSM struct {
	store  map[string]string
	getErr error
}

func smKey(ns, key string) string {
	if ns == "" {
		return key
	}
	return ns + "/" + key
}

func (f *fakeSM) Get(ns, key string) (string, error) { return f.store[smKey(ns, key)], f.getErr }
func (f *fakeSM) Set(ns, key, val string) error      { f.store[smKey(ns, key)] = val; return nil }
func (f *fakeSM) IsSet(ns, key string) bool          { _, ok := f.store[smKey(ns, key)]; return ok }
func (f *fakeSM) Delete(ns, key string) error        { delete(f.store, smKey(ns, key)); return nil }

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

	// A keyring read failure is not "no password yet": minting a new one would
	// rotate the live role's password out from under the running service.
	t.Run("keyring read error fails instead of rotating", func(t *testing.T) {
		sm := &fakeSM{store: make(map[string]string), getErr: errFake("dbus: no session")}
		p := New("/tmp", sm)
		if _, err := p.ensurePassword("mysvc"); err == nil {
			t.Fatal("expected an error when the keyring cannot be read")
		}
		if len(sm.store) != 0 {
			t.Error("no new password should be stored after a read failure")
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		// Verify expected docker exec calls were made. The database is created
		// owned by the service role (not merely granted to it), and the public
		// schema is handed over too — since PG 15 it belongs to
		// pg_database_owner and is not world-writable.
		var hasCreateDB, hasCreateUser, hasSchemaOwner, hasExtension bool
		for _, call := range fc.runCalls {
			line := call.Name + " " + call.line()
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}
		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		userIdx, dbIdx := -1, -1
		for i, call := range fc.runCalls {
			line := call.line()
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}
		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		var altered bool
		for _, call := range fc.runCalls {
			if strings.Contains(call.line(), `ALTER DATABASE "testdb" OWNER TO "testuser"`) {
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		for _, call := range fc.runCalls {
			line := call.line()
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		var altered, created bool
		for _, call := range fc.runCalls {
			line := call.line()
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
		pc := &Provisioner{ContainerFor: fixedNames, RC: fcCreate, SM: &fakeSM{store: make(map[string]string)}}
		if err := pc.Provision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		fcDrop := &fakeCommander{}
		pd := &Provisioner{ContainerFor: fixedNames, RC: fcDrop, SM: &fakeSM{store: make(map[string]string)}}
		if err := pd.Deprovision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		const account = "'testuser'@'%'"
		joined := func(f *fakeCommander) string {
			var all string
			for _, c := range f.runCalls {
				all += c.line() + "\n"
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
				p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}

				su := decl
				su.Superuser = tc.superuser
				if err := p.Provision(ctx, config.DBPostgres, "mysvc", su); err != nil {
					t.Fatal(err)
				}

				var got bool
				for _, call := range fc.runCalls {
					if strings.Contains(call.line(), wantSQL) {
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		for _, call := range fc.runCalls {
			line := call.Name + " " + call.line()
			if strings.Contains(line, "CREATE DATABASE") {
				t.Errorf("CREATE DATABASE should not be called for existing DB, got: %s", line)
			}
		}
	})

	t.Run("mariadb provisions DB and user", func(t *testing.T) {
		fc := &fakeCommander{}
		sm := &fakeSM{store: make(map[string]string)}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}

		if err := p.Provision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}

		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + call.line()
			if strings.Contains(line, "CREATE DATABASE") && strings.Contains(line, "testdb") {
				found = true
			}
		}
		if !found {
			t.Error("missing CREATE DATABASE for mariadb")
		}
	})

	// argv is world-readable via ps; any statement carrying the password must
	// travel over stdin.
	t.Run("password never appears on argv", func(t *testing.T) {
		for _, dbType := range []config.DBType{config.DBPostgres, config.DBMariaDB} {
			fc := &fakeCommander{}
			sm := &fakeSM{store: map[string]string{config.DBPasswordKey("mysvc"): "s3cretPW"}}
			p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}
			if err := p.Provision(ctx, dbType, "mysvc", decl); err != nil {
				t.Fatal(err)
			}
			var sent bool
			for _, call := range fc.runCalls {
				if strings.Contains(strings.Join(call.Args, " "), "s3cretPW") {
					t.Errorf("%s: password on argv: %v", dbType, call.Args)
				}
				sent = sent || strings.Contains(call.Stdin, "s3cretPW")
			}
			if !sent {
				t.Errorf("%s: password was never sent over stdin", dbType)
			}
		}
	})

	t.Run("postgres probe failure aborts before CREATE", func(t *testing.T) {
		for _, probe := range []string{
			"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_roles WHERE rolname='testuser'",
			"docker exec homelab-postgres psql -U postgres -d postgres -t -A -c SELECT 1 FROM pg_database WHERE datname='testdb'",
		} {
			fc := &fakeCommander{outputErr: map[string]error{probe: errFake("connection refused")}}
			p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: make(map[string]string)}}
			if err := p.Provision(ctx, config.DBPostgres, "mysvc", decl); err == nil {
				t.Fatalf("expected error when probe fails: %s", probe)
			}
			for _, call := range fc.runCalls {
				if l := call.line(); strings.Contains(l, "CREATE USER") && strings.Contains(probe, "pg_roles") ||
					strings.Contains(l, "CREATE DATABASE") {
					t.Errorf("CREATE issued after failed probe: %s", l)
				}
			}
		}
	})

	// CREATE USER IF NOT EXISTS keeps an existing account's old password, so
	// the ALTER is what makes `homelab setup` re-sync it; names must be escaped.
	t.Run("mariadb re-syncs password and escapes names", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: map[string]string{config.DBPasswordKey("mysvc"): "pw"}}}
		evil := config.ServiceDBDecl{Database: "a`b", User: `o'k\`}
		if err := p.Provision(ctx, config.DBMariaDB, "mysvc", evil); err != nil {
			t.Fatal(err)
		}
		sql := fc.runCalls[0].Stdin
		for _, want := range []string{
			"CREATE DATABASE IF NOT EXISTS `a``b`;",
			`ALTER USER 'o''k\\'@'%' IDENTIFIED BY 'pw';`,
			"GRANT ALL PRIVILEGES ON `a``b`.* TO 'o''k\\\\'@'%';",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("SQL missing %q:\n%s", want, sql)
			}
		}
	})

	t.Run("unsupported db type returns error", func(t *testing.T) {
		p := &Provisioner{ContainerFor: fixedNames, RC: &fakeCommander{}}
		err := p.Provision(ctx, config.DBType("mssql"), "mysvc", decl)
		if err == nil {
			t.Fatal("expected error for unsupported db type")
		}
	})
}

// `docker compose up -d` returns before Postgres accepts connections, so
// auto-started dependencies must be waited on or the dependent service comes up
// against a database that is not listening yet.

func TestDeprovision(t *testing.T) {
	ctx := context.Background()
	decl := config.ServiceDBDecl{
		Database: "testdb",
		User:     "testuser",
	}

	t.Run("postgres drops user", func(t *testing.T) {
		fc := &fakeCommander{}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		if err := p.Deprovision(ctx, config.DBPostgres, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + call.line()
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
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		if err := p.Deprovision(ctx, config.DBMariaDB, "mysvc", decl); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, call := range fc.runCalls {
			line := call.Name + " " + call.line()
			if strings.Contains(line, "DROP USER") && strings.Contains(line, "testuser") {
				found = true
			}
		}
		if !found {
			t.Error("missing DROP USER for mariadb")
		}
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

type errString string

func (e errString) Error() string { return string(e) }
func errFake(s string) error      { return errString(s) }

// ── read-only access to another service's database ──────────────────────────

var roDecl = config.ServiceDBDecl{Database: "taash", User: "taash_ro", ReadOnly: true}

// pgState answers the read-only provisioner's queries about database taash
// and the declared role.
type pgState struct {
	dbExists bool
	owner    string // pg_database.datdba of taash
	role     string // attribute row "privileged|ownsdb|member|hasmembers" ("f|f|f|f" plain); "" = no such role
	ownsInDB string // whether the role owns a relation or schema in taash: "f" or "t"
}

func (s pgState) commander() *fakeCommander {
	return &fakeCommander{outputFn: func(key string) ([]byte, error, bool) {
		switch {
		case strings.Contains(key, "SELECT 1 FROM pg_database WHERE datname='taash'"):
			if s.dbExists {
				return []byte("1\n"), nil, true
			}
			return []byte(""), nil, true
		case strings.Contains(key, "pg_get_userbyid(datdba)"):
			return []byte(s.owner + "\n"), nil, true
		case strings.Contains(key, "pg_class"):
			if !strings.Contains(key, "-d taash") {
				return nil, errFake("ownership must be checked inside taash"), true
			}
			return []byte(s.ownsInDB + "\n"), nil, true
		case strings.Contains(key, "pg_auth_members"):
			return []byte(s.role + "\n"), nil, true
		case strings.Contains(key, "SELECT 1 FROM pg_roles WHERE rolname="):
			if s.role != "" {
				return []byte("1\n"), nil, true
			}
			return []byte(""), nil, true
		}
		return nil, nil, false
	}}
}

// allSQL joins every command run, argv and stdin, for content assertions.
func allSQL(fc *fakeCommander) string {
	var b strings.Builder
	for _, c := range fc.runCalls {
		b.WriteString(c.line())
		b.WriteString("\n")
	}
	return b.String()
}

func TestProvision_ReadOnlyGrants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state pgState
		verb  string
	}{
		{"new role", pgState{dbExists: true, owner: "taash"}, "CREATE"},
		{"existing plain role re-synced", pgState{dbExists: true, owner: "taash", role: "f|f|f|f", ownsInDB: "f"}, "ALTER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := tc.state.commander()
			sm := &fakeSM{store: map[string]string{}}
			p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}

			if err := p.Provision(context.Background(), config.DBPostgres, "grafana", roDecl); err != nil {
				t.Fatal(err)
			}
			pw := sm.store[config.DBPasswordKey("grafana")]
			if pw == "" {
				t.Fatal("the consumer's DB_PASSWORD_GRAFANA was not generated")
			}
			sql := allSQL(fc)
			for _, want := range []string{
				tc.verb + ` USER "taash_ro" WITH LOGIN PASSWORD`,
				`GRANT CONNECT ON DATABASE "taash" TO "taash_ro"`,
				`GRANT USAGE ON SCHEMA public TO "taash_ro"`,
				`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "taash_ro"`,
				`GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO "taash_ro"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "taash" IN SCHEMA public GRANT SELECT ON TABLES TO "taash_ro"`,
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("missing %q in:\n%s", want, sql)
				}
			}
			for _, never := range []string{"CREATE DATABASE", "OWNER TO", "SUPERUSER", "CREATE EXTENSION", "GRANT ALL", "GRANT CREATE"} {
				if strings.Contains(sql, never) {
					t.Errorf("read-only provisioning must not issue %q:\n%s", never, sql)
				}
			}
			// The grants run inside the target database, and no password reaches argv.
			for _, c := range fc.runCalls {
				if strings.Contains(strings.Join(c.Args, " "), pw) {
					t.Errorf("password in argv: %v", c.Args)
				}
				if strings.Contains(c.Stdin, "GRANT SELECT") && !strings.Contains(strings.Join(c.Args, " "), "-d taash") {
					t.Errorf("grants must run in database taash: %v", c.Args)
				}
			}
		})
	}
}

// up of the consumer before the owner: a clear error, nothing created.
func TestProvision_ReadOnlyMissingDatabase(t *testing.T) {
	fc := pgState{}.commander()
	p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: &fakeSM{store: map[string]string{}}}

	err := p.Provision(context.Background(), config.DBPostgres, "grafana", roDecl)
	if err == nil {
		t.Fatal("expected an error for a missing database")
	}
	if want := "database taash does not exist yet — `homelab up taash` first"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, ErrDatabaseMissing) {
		t.Error("the error must match ErrDatabaseMissing")
	}
	if len(fc.runCalls) != 0 {
		t.Errorf("nothing may change in postgres, got %d commands:\n%s", len(fc.runCalls), allSQL(fc))
	}
}

// Read access never takes over an existing role that is not a plain login
// role: its password would be reset to the consumer's.
func TestProvision_ReadOnlyRefusesNonPlainRole(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		state      pgState
		why        string
	}{
		{"superuser", "postgres", pgState{dbExists: true, owner: "taash", role: "t|t|f|f", ownsInDB: "t"},
			"role postgres already exists and is not a plain login role (has superuser, createrole, createdb, replication or bypassrls, owns a database, owns tables or schemas in taash)"},
		{"target database owner", "taash", pgState{dbExists: true, owner: "taash", role: "f|t|f|f", ownsInDB: "t"},
			"role taash already exists and is not a plain login role (owns a database, owns tables or schemas in taash)"},
		{"another service's owner role", "immich", pgState{dbExists: true, owner: "taash", role: "f|t|f|f", ownsInDB: "f"},
			"role immich already exists and is not a plain login role (owns a database)"},
		{"member of a role", "taash_ro", pgState{dbExists: true, owner: "taash", role: "f|f|t|f", ownsInDB: "f"},
			"role taash_ro already exists and is not a plain login role (is a member of another role)"},
		{"group role", "taash_ro", pgState{dbExists: true, owner: "taash", role: "f|f|f|t", ownsInDB: "f"},
			"role taash_ro already exists and is not a plain login role (has members: it is a group role)"},
		{"owns a table in the database", "taash_ro", pgState{dbExists: true, owner: "taash", role: "f|f|f|f", ownsInDB: "t"},
			"role taash_ro already exists and is not a plain login role (owns tables or schemas in taash)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := tc.state.commander()
			sm := &fakeSM{store: map[string]string{}}
			p := &Provisioner{ContainerFor: fixedNames, RC: fc, SM: sm}
			decl := roDecl
			decl.User = tc.user
			err := p.Provision(context.Background(), config.DBPostgres, "grafana", decl)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.why)
			}
			if pw := sm.store[config.DBPasswordKey("grafana")]; pw != "" && strings.Contains(err.Error(), pw) {
				t.Error("password in the error")
			}
			if len(fc.runCalls) != 0 {
				t.Errorf("no ALTER or GRANT may run:\n%s", allSQL(fc))
			}
		})
	}
}

func TestDeprovision_ReadOnly(t *testing.T) {
	t.Run("drops what the role holds in the database, then the role", func(t *testing.T) {
		fc := pgState{dbExists: true, role: "f|f|f|f", ownsInDB: "f"}.commander()
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		if err := p.Deprovision(context.Background(), config.DBPostgres, "grafana", roDecl); err != nil {
			t.Fatal(err)
		}
		if len(fc.runCalls) != 2 {
			t.Fatalf("want 2 commands, got:\n%s", allSQL(fc))
		}
		if l := fc.runCalls[0].line(); !strings.Contains(l, "-d taash") || !strings.Contains(l, `DROP OWNED BY "taash_ro"`) {
			t.Errorf("first command = %q", l)
		}
		if l := fc.runCalls[1].line(); !strings.Contains(l, `DROP ROLE IF EXISTS "taash_ro"`) {
			t.Errorf("second command = %q", l)
		}
	})
	t.Run("database gone: only the role", func(t *testing.T) {
		fc := pgState{role: "f|f|f|f"}.commander()
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		if err := p.Deprovision(context.Background(), config.DBPostgres, "grafana", roDecl); err != nil {
			t.Fatal(err)
		}
		sql := allSQL(fc)
		if strings.Contains(sql, "DROP OWNED") || !strings.Contains(sql, `DROP ROLE IF EXISTS "taash_ro"`) {
			t.Errorf("got:\n%s", sql)
		}
	})
	t.Run("role never created: nothing to drop", func(t *testing.T) {
		fc := pgState{dbExists: true}.commander()
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		if err := p.Deprovision(context.Background(), config.DBPostgres, "grafana", roDecl); err != nil {
			t.Fatal(err)
		}
		if len(fc.runCalls) != 0 {
			t.Errorf("want no commands, got:\n%s", allSQL(fc))
		}
	})
	t.Run("a role that is not plain is left alone", func(t *testing.T) {
		fc := pgState{dbExists: true, role: "f|t|f|f", ownsInDB: "t"}.commander()
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		decl := roDecl
		decl.User = "taash"
		err := p.Deprovision(context.Background(), config.DBPostgres, "grafana", decl)
		if err == nil || !strings.Contains(err.Error(), "left alone") {
			t.Fatalf("err = %v, want a left-alone error", err)
		}
		if len(fc.runCalls) != 0 {
			t.Errorf("no DROP may run:\n%s", allSQL(fc))
		}
	})
	t.Run("a group role is left alone", func(t *testing.T) {
		fc := pgState{dbExists: true, role: "f|f|f|t", ownsInDB: "f"}.commander()
		p := &Provisioner{ContainerFor: fixedNames, RC: fc}
		err := p.Deprovision(context.Background(), config.DBPostgres, "grafana", roDecl)
		if err == nil || !strings.Contains(err.Error(), "group role") || !strings.Contains(err.Error(), "left alone") {
			t.Fatalf("err = %v, want a left-alone group-role error", err)
		}
		if len(fc.runCalls) != 0 {
			t.Errorf("no DROP may run:\n%s", allSQL(fc))
		}
	})
}
