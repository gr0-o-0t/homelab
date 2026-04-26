package config

import (
	"os"
	"path/filepath"
)

// DefaultConfigDir returns the platform-appropriate homelab config directory:
//
//	${XDG_CONFIG_HOME}/homelab   when XDG_CONFIG_HOME is set
//	${HOME}/.config/homelab      otherwise
func DefaultConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "homelab")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "homelab")
}
