// Package agentstate defines the agent's on-disk layout under /var/lib/homelab.
// StateDir is laid out like the classic config dir and is passed as
// --config-dir to the existing packages.
package agentstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/secrets"
)

// Root is the agent's state volume mount point.
const Root = "/var/lib/homelab"

const clusterSecretVar = "CLUSTER_SECRET"

// Paths are the agent's state locations.
type Paths struct {
	StateDir    string
	VaultFile   string
	KeyFile     string
	ClusterFile string
}

// At resolves the layout under root.
func At(root string) Paths {
	return Paths{
		StateDir:    filepath.Join(root, "state"),
		VaultFile:   filepath.Join(root, "vault.bin"),
		KeyFile:     filepath.Join(root, "vault.key"),
		ClusterFile: filepath.Join(root, "cluster.yaml"),
	}
}

// Cluster is the swarm membership info. Secret (hex, 32 bytes) lives in the
// vault under root:CLUSTER_SECRET, never in cluster.yaml.
type Cluster struct {
	Secret     string `yaml:"-"`
	LeaderAddr string `yaml:"leader_addr"`
	TSHostname string `yaml:"ts_hostname"`
}

// LoadCluster reads cluster.yaml and the secret from sm. With neither present
// it returns a zero Cluster and nil; a secret without cluster.yaml is an error.
func LoadCluster(p Paths, sm *secrets.Manager) (Cluster, error) {
	secret, err := sm.Get("", clusterSecretVar)
	if err != nil {
		return Cluster{}, err
	}
	var c Cluster
	b, err := os.ReadFile(p.ClusterFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if secret != "" {
			return Cluster{}, fmt.Errorf("cluster secret exists but %s is missing", p.ClusterFile)
		}
		return Cluster{}, nil
	case err != nil:
		return Cluster{}, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Cluster{}, fmt.Errorf("parse %s: %w", p.ClusterFile, err)
	}
	c.Secret = secret
	return c, nil
}

// SaveCluster writes cluster.yaml (0600, no secret) and stores the secret in sm.
func SaveCluster(p Paths, sm *secrets.Manager, c Cluster) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.ClusterFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p.ClusterFile, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p.ClusterFile, 0o600); err != nil {
		return err
	}
	return sm.Set("", clusterSecretVar, c.Secret)
}
