package db

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/config"
)

// ── Generated secrets ─────────────────────────────────────────────────────────

// EnsureGeneratedSecrets mints every secret svcName's config.yaml marks with
// `generate:` that the keyring does not hold yet, and returns the names it
// created. Existing values are never replaced: the shared redis password and
// Garage's rpc_secret are baked into running containers and data on disk.
func (p *Provisioner) EnsureGeneratedSecrets(svcName string) ([]string, error) {
	cfg, err := config.Load(config.ServiceConfigFile(p.ConfigDir, svcName))
	if err != nil || cfg == nil || p.SM == nil {
		return nil, err
	}
	var created []string
	for key, e := range cfg.Secrets {
		if e.Generate == "" {
			continue
		}
		existing, err := p.SM.Get(svcName, key)
		if err != nil {
			return created, fmt.Errorf("reading %s from keyring: %w", key, err)
		}
		if existing != "" {
			continue
		}
		val, err := generateSecret(e.Generate)
		if err != nil {
			return created, fmt.Errorf("%s/%s: %w", svcName, key, err)
		}
		if err := p.SM.Set(svcName, key, val); err != nil {
			return created, fmt.Errorf("storing %s in keyring: %w", key, err)
		}
		created = append(created, key)
	}
	return created, nil
}

func generateSecret(kind string) (string, error) {
	switch kind {
	case "password":
		return generatePassword(32), nil
	case "hex32":
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		return hex.EncodeToString(buf), nil
	default:
		return "", fmt.Errorf("unknown secret generator %q (want password or hex32)", kind)
	}
}

// ── Redis ─────────────────────────────────────────────────────────────────────

// provisionRedis gives (svc, decl.Name) its own database number on the shared
// redis, and checks the running instance accepts the shared password the
// service is about to be handed.
//
// The check matters because the password lives in two places — the keyring,
// and the arguments homelab-redis was created with. A container created before
// the password existed (or resumed with `docker start`) runs without auth, and
// every client then fails on AUTH with an error that names neither cause.
func (p *Provisioner) provisionRedis(svcName string, decl config.ServiceDBDecl) error {
	n, fresh, err := config.AllocateRedisDB(p.ConfigDir, svcName, decl.Name)
	if err != nil {
		return err
	}
	password := ""
	if p.SM != nil {
		if password, err = p.SM.Get(config.SharedDBName(config.DBRedis), config.RedisPasswordKey); err != nil {
			return fmt.Errorf("reading shared redis password: %w", err)
		}
	}
	if password == "" {
		return fmt.Errorf("no shared redis password in the keyring — run `homelab up redis` to generate it")
	}

	// Over stdin, not argv: the command line of `docker exec` is visible to
	// every user on the host.
	script := fmt.Sprintf("AUTH %s\nSELECT %d\n", password, n)
	if fresh {
		// A number freed by a deleted service may still hold its keys.
		script += "FLUSHDB\n"
	}
	out, err := p.RC.OutputFrom(strings.NewReader(script),
		"docker", "exec", "-i", p.containerName(config.DBRedis), "redis-cli")
	if err != nil {
		return fmt.Errorf("checking shared redis: %w", err)
	}
	reply := strings.Fields(string(out))
	ok := len(reply) > 0
	for _, r := range reply {
		ok = ok && r == "OK"
	}
	if !ok {
		return fmt.Errorf("homelab-redis rejected the shared password (%s)\n"+
			"  It was probably created before REDIS_PASSWORD existed: recreate it with `homelab up redis`",
			firstLine(out))
	}
	return nil
}

// ── Garage (s3) ───────────────────────────────────────────────────────────────

// garage runs the garage CLI inside the shared container. Output, not Run:
// the CLI logs connection chatter to stderr on every call.
func (p *Provisioner) garage(args ...string) ([]byte, error) {
	return p.RC.Output("docker", append([]string{"exec", p.containerName(config.DBS3), "/garage"}, args...)...)
}

var layoutVersionRe = regexp.MustCompile(`Current cluster layout version:\s*(\d+)`)

// ensureGarageLayout gives a new single-node cluster its layout. Garage
// accepts no writes until a layout assigning storage to at least one node has
// been applied; version 0 means none ever has. Re-running after a partial
// first attempt is safe: assign only stages, and apply --version 1 commits
// whatever is staged.
func (p *Provisioner) ensureGarageLayout() error {
	out, err := p.garage("layout", "show")
	if err != nil {
		return fmt.Errorf("reading garage layout: %w", err)
	}
	m := layoutVersionRe.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("unrecognised `garage layout show` output")
	}
	if v, _ := strconv.Atoi(string(m[1])); v > 0 {
		return nil
	}

	out, err = p.garage("node", "id", "-q")
	if err != nil {
		return fmt.Errorf("reading garage node id: %w", err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), "@")
	if id == "" {
		return fmt.Errorf("garage returned an empty node id")
	}
	// Capacity is only a relative weight between nodes; with one node any
	// value works, and it is not a quota.
	if _, err := p.garage("layout", "assign", "-z", "dc1", "-c", "1T", id); err != nil {
		return fmt.Errorf("assigning garage layout: %w", err)
	}
	if _, err := p.garage("layout", "apply", "--version", "1"); err != nil {
		return fmt.Errorf("applying garage layout: %w", err)
	}
	return nil
}

var (
	garageKeyIDRe  = regexp.MustCompile(`(?m)^Key ID:\s*(\S+)`)
	garageSecretRe = regexp.MustCompile(`(?m)^Secret key:\s*(\S+)`)
)

// provisionGarage creates the service's bucket and access key, and grants
// that key read/write/owner on that bucket only. The key's id and secret are
// kept in the service's keyring namespace; a key that Garage no longer knows
// (a wiped metadata volume) is replaced rather than handed out.
func (p *Provisioner) provisionGarage(svcName string, decl config.ServiceDBDecl) error {
	if err := p.ensureGarageLayout(); err != nil {
		return err
	}

	bucket := decl.Bucket
	if bucket == "" {
		bucket = svcName
	}
	if _, err := p.garage("bucket", "info", bucket); err != nil {
		if _, err := p.garage("bucket", "create", bucket); err != nil {
			return fmt.Errorf("creating bucket %s: %w", bucket, err)
		}
	}

	if p.SM == nil {
		return fmt.Errorf("a keyring is required to store the garage key for %s", svcName)
	}
	keyID, err := p.SM.Get(svcName, config.GarageAccessKeyIDKey)
	if err != nil {
		return fmt.Errorf("reading garage key id: %w", err)
	}
	secret, err := p.SM.Get(svcName, config.GarageSecretKeyKey)
	if err != nil {
		return fmt.Errorf("reading garage secret: %w", err)
	}
	known := keyID != "" && secret != ""
	if known {
		_, err := p.garage("key", "info", keyID)
		known = err == nil
	}
	if !known {
		// `key create` prints the secret once, on stdout. `key import` would
		// let us choose it, but only by putting it in argv.
		out, err := p.garage("key", "create", svcName)
		if err != nil {
			return fmt.Errorf("creating garage key %s: %w", svcName, err)
		}
		id, sec := garageKeyIDRe.FindSubmatch(out), garageSecretRe.FindSubmatch(out)
		if id == nil || sec == nil {
			return fmt.Errorf("unrecognised `garage key create` output")
		}
		keyID, secret = string(id[1]), string(sec[1])
		if err := p.SM.Set(svcName, config.GarageAccessKeyIDKey, keyID); err != nil {
			return fmt.Errorf("storing garage key id: %w", err)
		}
		if err := p.SM.Set(svcName, config.GarageSecretKeyKey, secret); err != nil {
			return fmt.Errorf("storing garage secret: %w", err)
		}
	}

	if _, err := p.garage("bucket", "allow", "--read", "--write", "--owner", bucket, "--key", keyID); err != nil {
		return fmt.Errorf("granting %s access to bucket %s: %w", svcName, bucket, err)
	}
	return nil
}

// deprovisionGarage deletes the service's access key. Its bucket and objects
// stay, as a postgres database outlives its dropped role: deleting a service is
// not deleting its data.
func (p *Provisioner) deprovisionGarage(svcName string) error {
	if p.SM == nil {
		return nil
	}
	keyID, err := p.SM.Get(svcName, config.GarageAccessKeyIDKey)
	if err != nil {
		return fmt.Errorf("reading garage key id: %w", err)
	}
	if keyID == "" {
		return nil
	}
	if _, err := p.garage("key", "info", keyID); err == nil {
		if _, err := p.garage("key", "delete", "--yes", keyID); err != nil {
			return fmt.Errorf("deleting garage key %s: %w", keyID, err)
		}
	}
	_ = p.SM.Delete(svcName, config.GarageAccessKeyIDKey)
	_ = p.SM.Delete(svcName, config.GarageSecretKeyKey)
	return nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}
