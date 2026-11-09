// Package importer brings a Compose-era config dir (~/.config/homelab) into a
// new cluster: `homelab init --import <dir>`.
//
// It runs in two halves. On the host, Build writes the new state tree — the
// root and service config files, the files services bind-mount, exposure,
// rendered routing, desired state — into a private temp dir, and reads every
// declared secret from the desktop keyring into memory. bootstrap then copies
// the tree into the agent container (Tar, CopyToContainer) and hands the
// secrets to `homelab agent-seed --import` on the stdin of a one-shot exec.
// In the container, ApplySeed checks every secret name against the copied
// config files, stores the secrets in the vault and moves the tree into the
// state dir.
//
// Secret values exist only in the keyring, in memory, on that stdin and in
// the vault: never in the tree, the tar stream, argv, logs or errors.
// Docker named volumes are not touched; the Swarm services reuse them by name.
package importer

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// Report is what an import changed or could not bring along.
type Report struct {
	// Services are the imported services, sorted.
	Services []string
	// Changed lists every rewrite and every kept-but-unserved layer.
	Changed []string
	// MissingSecrets are declared secrets the keyring did not hold, as
	// secrets.RootKey / secrets.ServiceKey names; `homelab setup <svc>`
	// enters them afterwards. Generated secrets are not listed: the agent
	// mints a missing one.
	MissingSecrets []string
}

// Secrets is agent-seed --import's stdin: root and per-service secrets.
type Secrets struct {
	Root     map[string]string            `json:"root,omitempty"`
	Services map[string]map[string]string `json:"services,omitempty"`
}

// Names are the secret names an imported tree may carry.
type Names struct {
	Root     map[string]bool
	Services map[string]map[string]bool
	// generated are the names homelab mints when they are missing.
	generated map[string]bool // secrets.RootKey / ServiceKey form
	// dbCreds are the shared-database credentials among them, by name, with
	// the service they belong to and what they are.
	dbCreds map[string]dbCred
}

// Per-service files that travel besides the bind-mounted ones.
var serviceFiles = []string{"config.yaml", "docker-compose.yml", exposure.FileName, "caddy.routes.conf"}

// desiredRel is where internal/desired reads the desired state, relative to
// the state dir.
var desiredRel, _ = filepath.Rel("/", desired.DesiredFile("/"))

// Modes in the tree: nothing in it is anyone's but the agent's.
const (
	fileMask = 0o600
	dirMode  = 0o700
)

// Build writes the state tree for the Compose-era config dir oldDir into dst
// (an existing empty dir) and reads its declared secrets from keyring. A
// service name that is not valid refuses the whole import.
func Build(oldDir, dst string, keyring *secrets.Manager) (Report, Secrets, error) {
	var rep Report
	if _, err := os.Stat(filepath.Join(oldDir, "config.yaml")); err != nil {
		return rep, Secrets{}, fmt.Errorf("%s is not a homelab config dir (no config.yaml): %w", oldDir, err)
	}
	svcs, err := listServices(filepath.Join(oldDir, "services"))
	if err != nil {
		return rep, Secrets{}, err
	}
	rep.Services = svcs

	for _, f := range []struct{ from, to string }{
		{"config.yaml", "config.yaml"},
		{"redis-dbs.yaml", "redis-dbs.yaml"},
		{filepath.Join("state", "desired.yaml"), desiredRel},
	} {
		if err := copyIfExists(filepath.Join(oldDir, f.from), filepath.Join(dst, f.to)); err != nil {
			return rep, Secrets{}, err
		}
	}
	for _, svc := range svcs {
		src, to := filepath.Join(oldDir, "services", svc), filepath.Join(dst, "services", svc)
		if err := os.MkdirAll(to, dirMode); err != nil {
			return rep, Secrets{}, err
		}
		for _, f := range serviceFiles {
			if err := copyIfExists(filepath.Join(src, f), filepath.Join(to, f)); err != nil {
				return rep, Secrets{}, err
			}
		}
		changed, err := tcpOnlyRawPorts(svc, filepath.Join(to, "config.yaml"))
		if err != nil {
			return rep, Secrets{}, err
		}
		rep.Changed = append(rep.Changed, changed...)
	}

	if err := migrateLegacy(oldDir, dst); err != nil {
		return rep, Secrets{}, err
	}

	names, err := AllowedNames(dst)
	if err != nil {
		return rep, Secrets{}, err
	}
	for _, svc := range svcs {
		changed, err := copyBinds(oldDir, dst, svc, names)
		if err != nil {
			return rep, Secrets{}, err
		}
		rep.Changed = append(rep.Changed, changed...)
	}

	served := layers.New(dst, nil, nil)
	for _, svc := range svcs {
		if _, err := os.Stat(exposure.Path(dst, svc)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var unserved *routing.UnservedError
		if err := routing.Sync(dst, served, svc); err != nil && !errors.As(err, &unserved) {
			return rep, Secrets{}, fmt.Errorf("rendering %s's routing: %w", svc, err)
		}
		st, err := exposure.Load(dst, svc)
		if err != nil {
			return rep, Secrets{}, err
		}
		for _, l := range routing.Unserved(served, st) {
			rep.Changed = append(rep.Changed, fmt.Sprintf("%s: layer %s kept but not served in cluster mode", svc, l))
		}
	}

	sec, missing, warnings, err := readSecrets(keyring, names)
	if err != nil {
		return rep, Secrets{}, err
	}
	rep.MissingSecrets = missing
	rep.Changed = append(rep.Changed, warnings...)
	sort.Strings(rep.Changed)
	return rep, sec, nil
}

// listServices is every service dir, sorted; an invalid name is an error.
func listServices(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		if err := config.ValidServiceName(de.Name()); err != nil {
			return nil, fmt.Errorf("refusing to import %s: %w", filepath.Join(dir, de.Name()), err)
		}
		out = append(out, de.Name())
	}
	return out, nil
}

// AllowedNames are the secret names the config files under root declare:
//
//   - root: the root config.yaml's secrets, and DB_PASSWORD_<SVC> for every
//     service with a postgres or mariadb database;
//   - per service: its config.yaml's secrets (generated ones included), the
//     Garage key pair for one with an s3 database, REDIS_PASSWORD for redis.
func AllowedNames(root string) (Names, error) {
	n := Names{Root: map[string]bool{}, Services: map[string]map[string]bool{}, generated: map[string]bool{},
		dbCreds: map[string]dbCred{}}
	rc, err := config.Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		return n, err
	}
	if rc != nil {
		for k, s := range rc.Secrets {
			n.Root[k] = true
			if s.Generate != "" {
				n.generated[secrets.RootKey(k)] = true
			}
		}
	}
	svcs, err := listServices(filepath.Join(root, "services"))
	if err != nil {
		return n, err
	}
	for _, svc := range svcs {
		m := map[string]bool{}
		n.Services[svc] = m
		cfg, err := config.Load(config.ServiceConfigFile(root, svc))
		if err != nil {
			return n, err
		}
		if svc == config.SharedDBName(config.DBRedis) {
			m[config.RedisPasswordKey] = true
		}
		if cfg == nil {
			continue
		}
		for k, s := range cfg.Secrets {
			m[k] = true
			if s.Generate != "" {
				n.generated[secrets.ServiceKey(svc, k)] = true
			}
		}
		dbs, err := cfg.ServiceDatabases()
		if err != nil {
			return n, fmt.Errorf("%s: %w", svc, err)
		}
		for i := range dbs {
			d := &dbs[i]
			switch d.Type {
			case config.DBPostgres, config.DBMariaDB:
				k := config.DBPasswordKey(svc)
				n.Root[k] = true
				n.generated[secrets.RootKey(k)] = true // the provisioner mints and syncs it
				n.dbCreds[secrets.RootKey(k)] = dbCred{svc, "database password",
					"a new one will be set on the existing role"}
			case config.DBS3:
				for _, k := range []string{config.GarageAccessKeyIDKey, config.GarageSecretKeyKey} {
					m[k] = true
					n.generated[secrets.ServiceKey(svc, k)] = true // provisioned on up
					n.dbCreds[secrets.ServiceKey(svc, k)] = dbCred{svc, "Garage access key",
						"a new key will be created for the existing bucket"}
				}
			}
		}
	}
	return n, nil
}

// dbCred is a shared-database credential the provisioner creates when it is
// missing: whose, what it is, and what happens then.
type dbCred struct{ svc, what, then string }

// readSecrets reads every allowed name from the keyring. A keyring failure is
// an error (it is not "missing"); a missing declared, non-generated secret is
// reported in missing. A missing database password or Garage key is not
// missing — the provisioner sets a new one — but it is warned about: the
// running database role keeps its old password until then, and an app that
// stored the old one in its own data breaks.
func readSecrets(kr *secrets.Manager, names Names) (sec Secrets, missing, warnings []string, err error) {
	sec = Secrets{Root: map[string]string{}, Services: map[string]map[string]string{}}
	warned := map[string]bool{}
	get := func(ns, k, label string) (string, error) {
		v, err := kr.Get(ns, k)
		if err != nil {
			return "", fmt.Errorf("reading %s from the keyring: %w", label, err)
		}
		if v != "" {
			return v, nil
		}
		if c, ok := names.dbCreds[label]; ok {
			if w := c.svc + "/" + c.what; !warned[w] {
				warned[w] = true
				warnings = append(warnings, fmt.Sprintf("%s: %s not found in the keyring — %s; "+
					"apps that store it in their own config (e.g. nextcloud) must be updated", c.svc, c.what, c.then))
			}
		} else if !names.generated[label] {
			missing = append(missing, label)
		}
		return "", nil
	}
	for _, k := range slices.Sorted(maps.Keys(names.Root)) {
		v, err := get("", k, secrets.RootKey(k))
		if err != nil {
			return Secrets{}, nil, nil, err
		}
		if v != "" {
			sec.Root[k] = v
		}
	}
	for _, svc := range slices.Sorted(maps.Keys(names.Services)) {
		for _, k := range slices.Sorted(maps.Keys(names.Services[svc])) {
			v, err := get(svc, k, secrets.ServiceKey(svc, k))
			if err != nil {
				return Secrets{}, nil, nil, err
			}
			if v == "" {
				continue
			}
			if sec.Services[svc] == nil {
				sec.Services[svc] = map[string]string{}
			}
			sec.Services[svc][k] = v
		}
	}
	sort.Strings(missing)
	return sec, missing, warnings, nil
}

// migrateLegacy infers exposure.yaml for services still routed by the old
// per-layer conf.d* files: they are copied into dst for routing.Migrate to
// read, then dropped again.
func migrateLegacy(oldDir, dst string) error {
	dirs, err := filepath.Glob(filepath.Join(oldDir, "caddy", "conf.d*"))
	if err != nil || len(dirs) == 0 {
		return err
	}
	var copied []string
	defer func() {
		for _, d := range copied {
			_ = os.RemoveAll(d)
		}
	}()
	for _, d := range dirs {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			continue
		}
		to := filepath.Join(dst, "caddy", filepath.Base(d))
		copied = append(copied, to)
		if _, err := copyTree(d, to); err != nil {
			return err
		}
	}
	if _, err := routing.Migrate(dst, layers.Legacy(dst, nil, nil)); err != nil {
		return fmt.Errorf("inferring exposure from the legacy Caddy files: %w", err)
	}
	return nil
}

// copyBinds copies what svc's compose file bind-mounts from its own dir.
func copyBinds(oldDir, dst, svc string, names Names) ([]string, error) {
	src := filepath.Join(oldDir, "services", svc)
	if _, err := os.Stat(filepath.Join(src, "docker-compose.yml")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	env, err := composeEnv(dst, svc, names)
	if err != nil {
		return nil, err
	}
	binds, err := swarm.BindSources(src, env)
	if err != nil {
		return nil, fmt.Errorf("%s: reading its bind mounts: %w", svc, err)
	}
	var changed []string
	for _, b := range binds {
		rel := b.Rel
		if !b.ReadOnly {
			// Translate refuses it: the deploy would fail in the agent's log
			// only. The data is still copied, so nothing is lost.
			changed = append(changed, fmt.Sprintf("%s: ./%s is a writable bind; cluster mode cannot deploy it — "+
				"move the data to a named volume", svc, rel))
		}
		if rel == "." {
			continue // the service dir itself: its files are copied above
		}
		from := filepath.Join(src, filepath.FromSlash(rel))
		fi, err := os.Lstat(from)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			changed = append(changed, fmt.Sprintf("%s: bind-mounted ./%s does not exist; nothing copied", svc, rel))
			continue
		case err != nil:
			return nil, err
		case fi.Mode()&os.ModeSymlink != 0:
			changed = append(changed, fmt.Sprintf("%s: bind-mounted ./%s is a symlink; not copied", svc, rel))
			continue
		}
		to := filepath.Join(dst, "services", svc, filepath.FromSlash(rel))
		if fi.IsDir() {
			skipped, err := copyTree(from, to)
			if err != nil {
				return nil, err
			}
			for _, s := range skipped {
				changed = append(changed, fmt.Sprintf("%s: ./%s/%s is not a regular file; not copied", svc, rel, s))
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			changed = append(changed, fmt.Sprintf("%s: bind-mounted ./%s is not a regular file; not copied", svc, rel))
			continue
		}
		if err := copyFile(from, to, fi.Mode()); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// placeholder stands in for secrets when the compose file is only read for
// its bind mounts: a `${X:?}` must not fail the load.
const placeholder = "homelab-import-placeholder"

// composeEnv is the service's interpolation env without any secret value.
func composeEnv(root, svc string, names Names) (map[string]string, error) {
	env, err := config.BuildEnv(config.RootConfigFile(root, ""), root, svc, nil)
	if err != nil {
		return nil, err
	}
	fill := func(k string) {
		if env[k] == "" {
			env[k] = placeholder
		}
	}
	for k := range names.Root {
		fill(k)
	}
	for k := range names.Services[svc] {
		fill(k)
	}
	cfg, err := config.Load(config.ServiceConfigFile(root, svc))
	if err != nil || cfg == nil {
		return env, err
	}
	if dbs, err := cfg.ServiceDatabases(); err == nil {
		for i := range dbs {
			d := &dbs[i]
			for _, target := range d.Env {
				if target != "" {
					fill(target)
				}
			}
		}
	}
	return env, nil
}

// tcpOnlyRawPorts rewrites the raw (listen) port declarations of a service
// config that carry both protocols — explicitly, or by saying none — to tcp
// only: caddy-l4 would otherwise bind a udp listener nobody uses. HTTP ports
// are left alone. The file is only re-encoded when something changed.
func tcpOnlyRawPorts(svc, path string) ([]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the tree being built
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: parsing config.yaml: %w", svc, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	top := doc.Content[0]
	var ports *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "ports" {
			ports = top.Content[i+1]
		}
	}
	if ports == nil {
		return nil, nil
	}
	var changed []string
	switch ports.Kind {
	case yaml.SequenceNode:
		for _, item := range ports.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			_, e, err := config.ParsePortString(item.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", svc, err)
			}
			if e.Listen != 0 && !strings.Contains(item.Value, "/") {
				changed = append(changed, fmt.Sprintf("%s: port %s is tcp only now", svc, item.Value))
				item.Value = strings.TrimSpace(item.Value) + "/tcp"
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(ports.Content); i += 2 {
			key, val := ports.Content[i], ports.Content[i+1]
			var e config.PortEntry
			if err := val.Decode(&e); err != nil {
				return nil, fmt.Errorf("%s: port %s: %w", svc, key.Value, err)
			}
			pl := e.ProtocolList()
			if e.Listen == 0 || val.Kind != yaml.MappingNode || !slices.Contains(pl, "tcp") || !slices.Contains(pl, "udp") {
				continue
			}
			setTCPOnly(val)
			changed = append(changed, fmt.Sprintf("%s: port %s (listen %d) is tcp only now", svc, key.Value, e.Listen))
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return changed, os.WriteFile(path, out.Bytes(), fileMask)
}

// setTCPOnly makes a legacy port mapping say `protocols: [tcp]` and drops
// its single-protocol `protocol:` field.
func setTCPOnly(m *yaml.Node) {
	var content []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case "protocol", "protocols":
			continue
		}
		content = append(content, m.Content[i], m.Content[i+1])
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle,
		Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "tcp"}}}
	content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "protocols"}, seq)
	m.Content = content
}

// copyIfExists copies a regular file; an absent one is skipped, a symlink or
// other non-regular file refused.
func copyIfExists(src, dst string) error {
	fi, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	return copyFile(src, dst, fi.Mode())
}

// copyFile copies src to dst, mode 0600 unless src's was more restrictive.
func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // a file of the dir being imported
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	perm := mode.Perm() & fileMask
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // the tree being built
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm) // umask-proof
}

// copyTree copies the regular files and directories under src (symlinks are
// not followed) and returns the relative paths it skipped.
func copyTree(src, dst string) ([]string, error) {
	var skipped []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, dirMode)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(p, to, fi.Mode())
		default:
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		}
	})
	return skipped, err
}

// Tar writes the tree at dir as a tar stream with every entry under prefix/:
// owned by root, files 0600 and dirs 0700 unless the tree's were more
// restrictive.
func Tar(w io.Writer, dir, prefix string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name = prefix + "/" + filepath.ToSlash(rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		h := &tar.Header{Name: name, ModTime: fi.ModTime(), Format: tar.FormatPAX}
		switch {
		case d.IsDir():
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, name+"/", int64(fi.Mode().Perm()&dirMode)
			return tw.WriteHeader(h)
		case d.Type().IsRegular():
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, int64(fi.Mode().Perm()&fileMask), fi.Size()
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			f, err := os.Open(p) //nolint:gosec // the tree being built
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.CopyN(tw, f, fi.Size())
			return err
		default:
			return fmt.Errorf("%s: not a regular file or directory", p)
		}
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// stagingRE is the name of the dir under the agent's root that a tree is
// copied into before agent-seed --import moves it into the state dir.
var stagingRE = regexp.MustCompile(`^import-[0-9a-f]{16}$`)

// StagingName is a fresh staging dir name: a stale one from an interrupted
// import is never merged into.
func StagingName(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return "import-" + hex.EncodeToString(b), nil
}

// maxSeed bounds agent-seed --import's stdin.
const maxSeed = 1 << 20

// ApplySeed is `homelab agent-seed --import <staging>`, in the agent
// container: the tree CopyToContainer put at <root>/<staging> becomes the
// state dir, and the secrets on in (Secrets JSON) go into the vault. Every
// name must be one AllowedNames gives for the staged tree; any other refuses
// the whole import before anything is stored. A state dir that already has
// services is refused too. The staging dir is removed on every exit path.
// Returns the stored names (RootKey / ServiceKey form), sorted.
func ApplySeed(root, staging string, in io.Reader) ([]string, error) {
	if !stagingRE.MatchString(staging) {
		return nil, fmt.Errorf("agent-seed --import: %q is not a staging dir name", staging)
	}
	dir := filepath.Join(root, staging)
	defer func() { _ = os.RemoveAll(dir) }()
	p := agentstate.At(root)
	if has, err := hasServices(p.StateDir); err != nil {
		return nil, err
	} else if has {
		return nil, errors.New("agent-seed --import: the agent's state dir already has services — " +
			"an import only goes into a new cluster. To start over, remove the agent and its state: " +
			"`docker rm -f homelab-agent && docker volume rm homelab-agent` — data volumes are untouched")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("agent-seed --import: no staged tree at %s", dir)
	}

	b, err := io.ReadAll(io.LimitReader(in, maxSeed+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSeed {
		return nil, errors.New("agent-seed --import: input too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var sec Secrets
	if err := dec.Decode(&sec); err != nil {
		return nil, fmt.Errorf("agent-seed --import: stdin must be {\"root\":{…},\"services\":{…}} of strings: %w", err)
	}
	names, err := AllowedNames(dir)
	if err != nil {
		return nil, fmt.Errorf("agent-seed --import: reading the staged config: %w", err)
	}
	type item struct{ ns, k, v string }
	var items []item
	for k, v := range sec.Root {
		if !names.Root[k] {
			return nil, fmt.Errorf("agent-seed --import: refusing root secret %q: the imported config does not declare it", k)
		}
		items = append(items, item{"", k, v})
	}
	for svc, m := range sec.Services {
		if err := config.ValidServiceName(svc); err != nil {
			return nil, fmt.Errorf("agent-seed --import: %w", err)
		}
		allowed, ok := names.Services[svc]
		if !ok {
			return nil, fmt.Errorf("agent-seed --import: refusing secrets of %q: not an imported service", svc)
		}
		for k, v := range m {
			if !allowed[k] {
				return nil, fmt.Errorf("agent-seed --import: refusing %s secret %q: its config does not declare it", svc, k)
			}
			items = append(items, item{svc, k, v})
		}
	}
	label := func(it item) string {
		if it.ns == "" {
			return secrets.RootKey(it.k)
		}
		return secrets.ServiceKey(it.ns, it.k)
	}
	for _, it := range items {
		if it.v == "" {
			return nil, fmt.Errorf("agent-seed --import: %s is empty", label(it))
		}
	}
	sort.Slice(items, func(i, j int) bool { return label(items[i]) < label(items[j]) })

	vault, err := secrets.OpenVault(p.VaultFile, p.KeyFile)
	if err != nil {
		return nil, err
	}
	stored := make([]string, 0, len(items))
	for _, it := range items {
		if err := vault.Set(it.ns, it.k, it.v); err != nil {
			return nil, fmt.Errorf("agent-seed --import: storing %s: %w", label(it), err)
		}
		stored = append(stored, label(it))
	}
	if err := moveInto(dir, p.StateDir); err != nil {
		return nil, fmt.Errorf("agent-seed --import: moving the tree into %s: %w", p.StateDir, err)
	}
	return stored, nil
}

// hasServices reports whether the state dir has any service dir.
func hasServices(stateDir string) (bool, error) {
	des, err := os.ReadDir(filepath.Join(stateDir, "services"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, de := range des {
		if de.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// moveInto moves every file under src to the same place under dst, creating
// directories as needed: the agent may already have written its own files
// (Caddy's, tsnet's) into the state dir.
func moveInto(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(to, fi.Mode().Perm()|0o700)
		}
		return os.Rename(p, to) //nolint:gosec // G122: the staging dir is the agent's own, written by CopyToContainer as root
	})
}
