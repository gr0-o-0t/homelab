// Package backup snapshots and restores the state a homelab service owns:
// its named Docker volumes, its databases on the shared instances, and the
// config files that describe it.
//
// Everything that touches Docker goes through Executor so the planning and
// path-building logic is testable without a daemon.
package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/config"
)

// Executor runs external commands. Satisfied by *run.Commander.
type Executor interface {
	Run(name string, args ...string) error
	RunTo(w io.Writer, name string, args ...string) error
	RunFrom(r io.Reader, name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

// helperImage is used to read and write volume contents. Volumes are only
// reachable from inside a container, so a tar step needs a throwaway one.
// Pinned to a major so backups do not silently change format with :latest.
const helperImage = "alpine:3"

// DatabaseTarget is one dumpable database on a shared instance.
type DatabaseTarget struct {
	Type      config.DBType
	Database  string
	User      string
	Container string // shared container to exec into
}

// DumpFile returns this database's filename inside a backup directory.
func (d DatabaseTarget) DumpFile(svc string) string {
	ext := "sql"
	if d.Type == config.DBPostgres {
		ext = "dump" // pg_dump custom format, restored with pg_restore
	}
	return fmt.Sprintf("%s-%s.%s", svc, d.Database, ext)
}

// Plan is everything that makes up one service's state.
type Plan struct {
	Service     string
	Volumes     []string // docker volume names, as they exist on the host
	Databases   []DatabaseTarget
	ConfigFiles []string // filenames under services/<svc>/
	// SkippedRedis records Redis dependencies that were intentionally not
	// dumped, so the caller can say so rather than appear to have covered them.
	SkippedRedis int
}

// Empty reports whether there is nothing to back up.
func (p Plan) Empty() bool {
	return len(p.Volumes) == 0 && len(p.Databases) == 0 && len(p.ConfigFiles) == 0
}

type composeVolumes struct {
	Volumes map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"volumes"`
}

// PlanFor works out what a service's backup consists of.
//
// Only volumes with an explicit `name:` are included: without one Docker derives
// the name from the compose project at runtime, so it cannot be resolved
// reliably from the file alone. The catalog test
// TestCatalogServices_VolumesAreExplicitlyNamed keeps that from happening.
//
// Redis is deliberately never dumped. In this catalog it is a cache and a job
// queue, never a system of record, and a stale restored queue is worse than an
// empty one.
func PlanFor(configDir, svcName string) (Plan, error) {
	p := Plan{Service: svcName}
	svcDir := filepath.Join(configDir, "services", svcName)

	composeData, err := os.ReadFile(filepath.Join(svcDir, "docker-compose.yml")) // #nosec G304 -- svcDir is <config>/services/<name>
	if err != nil {
		return p, fmt.Errorf("reading compose file for %s: %w", svcName, err)
	}
	var cv composeVolumes
	if err := yaml.Unmarshal(composeData, &cv); err != nil {
		return p, fmt.Errorf("parsing compose file for %s: %w", svcName, err)
	}
	for _, v := range cv.Volumes {
		if v.Name != "" {
			p.Volumes = append(p.Volumes, v.Name)
		}
	}
	sort.Strings(p.Volumes)

	svcCfg, err := config.Load(config.ServiceConfigFile(configDir, svcName))
	if err != nil {
		return p, fmt.Errorf("reading config for %s: %w", svcName, err)
	}
	if svcCfg != nil && svcCfg.Databases.Kind != 0 {
		dbs, err := svcCfg.ServiceDatabases()
		if err != nil {
			return p, fmt.Errorf("reading database declarations for %s: %w", svcName, err)
		}
		for i := range dbs {
			d := &dbs[i]
			switch d.Type {
			case config.DBPostgres, config.DBMariaDB:
				p.Databases = append(p.Databases, DatabaseTarget{
					Type:      d.Type,
					Database:  d.Database,
					User:      d.User,
					Container: config.SharedDBContainer(d.Type),
				})
			case config.DBRedis:
				p.SkippedRedis++
			}
		}
	}
	sort.Slice(p.Databases, func(i, j int) bool {
		return p.Databases[i].Database < p.Databases[j].Database
	})

	entries, err := os.ReadDir(svcDir)
	if err != nil {
		return p, fmt.Errorf("listing %s: %w", svcDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			p.ConfigFiles = append(p.ConfigFiles, e.Name())
		}
	}
	sort.Strings(p.ConfigFiles)

	return p, nil
}
