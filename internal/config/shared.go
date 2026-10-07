package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Shared-service credentials and state that are not per-database passwords.
//
// Every dependency a service declares under `databases:` is served by one
// shared instance — postgres, mariadb, redis, garage — and isolated inside it:
// a database and role per service on the SQL engines, a database number per
// service on redis, a bucket and access key per service on garage.

const (
	// RedisPasswordKey is the shared redis password, stored in the keyring
	// under the redis service's own namespace. Every consumer authenticates
	// with it; isolation between them is the per-service database number.
	RedisPasswordKey = "REDIS_PASSWORD"

	// RedisDBCount is the `--databases` value the shared redis runs with. The
	// allocator hands out 1..RedisDBCount-1: database 0 is what a client that
	// cannot select a database falls back to, so it is never given to anyone.
	RedisDBCount = 64

	// GarageAccessKeyIDKey and GarageSecretKeyKey hold a service's Garage
	// access key, in that service's keyring namespace.
	GarageAccessKeyIDKey = "GARAGE_ACCESS_KEY_ID"
	GarageSecretKeyKey   = "GARAGE_SECRET_ACCESS_KEY"

	// S3Region is the region the shared Garage answers for (garage.toml
	// s3_region). Garage rejects a SigV4 scope naming any other region.
	S3Region = "garage"

	// redisDBFile records which service owns which redis database number.
	redisDBFile = "redis-dbs.yaml"
)

// RedisDBKey names one redis allocation: the service, plus the declaration's
// instance name when a service holds more than one database.
func RedisDBKey(svc, name string) string {
	if name == "" {
		return svc
	}
	return svc + "/" + name
}

// RedisDBFile returns the path of the redis allocation table.
func RedisDBFile(configDir string) string {
	return filepath.Join(configDir, redisDBFile)
}

// LoadRedisDBs reads the allocation table. A missing file is an empty table.
func LoadRedisDBs(configDir string) (map[string]int, error) {
	data, err := os.ReadFile(RedisDBFile(configDir)) // #nosec G304 -- config dir path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int{}, nil
		}
		return nil, fmt.Errorf("reading redis allocations: %w", err)
	}
	m := map[string]int{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", RedisDBFile(configDir), err)
	}
	return m, nil
}

func saveRedisDBs(configDir string, m map[string]int) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	header := "# Managed by homelab: the shared redis database number each service owns.\n" +
		"# A number stays reserved until its service is deleted. Do not hand-edit\n" +
		"# while services are running.\n"
	path := RedisDBFile(configDir)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), data...), 0o600); err != nil {
		return fmt.Errorf("writing redis allocations: %w", err)
	}
	return os.Rename(tmp, path)
}

// AllocateRedisDB returns the database number owned by (svc, name), assigning
// the lowest free one on first use. fresh reports a new assignment, so the
// caller can clear whatever a previous owner of that number left behind.
//
// When every number is taken, entries whose service directory no longer exists
// (deleted by hand, or by a homelab that predates release-on-delete) are
// reclaimed before giving up — a number is never taken from an installed
// service.
func AllocateRedisDB(configDir, svc, name string) (n int, fresh bool, err error) {
	m, err := LoadRedisDBs(configDir)
	if err != nil {
		return 0, false, err
	}
	key := RedisDBKey(svc, name)
	if n, ok := m[key]; ok {
		return n, false, nil
	}

	n = lowestFreeRedisDB(m)
	if n == 0 {
		var stale []string
		for k := range m {
			owner, _, _ := strings.Cut(k, "/")
			if _, err := os.Stat(filepath.Join(configDir, "services", owner)); os.IsNotExist(err) {
				stale = append(stale, k)
			}
		}
		for _, k := range stale {
			delete(m, k)
		}
		n = lowestFreeRedisDB(m)
	}
	if n == 0 {
		return 0, false, fmt.Errorf("all %d shared redis databases are allocated (see %s)",
			RedisDBCount-1, RedisDBFile(configDir))
	}
	m[key] = n
	if err := saveRedisDBs(configDir, m); err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func lowestFreeRedisDB(m map[string]int) int {
	used := make(map[int]bool, len(m))
	for _, n := range m {
		used[n] = true
	}
	for n := 1; n < RedisDBCount; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

// ReleaseRedisDB frees the number held by (svc, name). Releasing an
// unallocated key is a no-op.
func ReleaseRedisDB(configDir, svc, name string) error {
	m, err := LoadRedisDBs(configDir)
	if err != nil {
		return err
	}
	key := RedisDBKey(svc, name)
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return saveRedisDBs(configDir, m)
}
