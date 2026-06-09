package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/config"
)

// withProfiles prepends --profile <name> for each extension that is
// enabled in the root config, activating optional Docker Compose services.
func withProfiles(cfgDir string, args ...string) []string {
	cfgFile := config.RootConfigFile(cfgDir, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)

	var profiles []string
	if cfg != nil {
		for _, ext := range cfg.Extensions {
			profiles = append(profiles, "--profile", config.ExtensionProfile(ext))
		}
	}
	if len(profiles) == 0 {
		return args
	}
	return append(profiles, args...)
}

// activeExtNotes returns user-facing notes about which extensions will start.
func activeExtNotes(cfgDir string) []string {
	cfgFile := config.RootConfigFile(cfgDir, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)

	var notes []string
	if cfg != nil {
		for _, ext := range cfg.Extensions {
			notes = append(notes, fmt.Sprintf("%s enabled — starting %s",
				config.ExtensionLabel(ext), ext))
		}
	}
	return notes
}
