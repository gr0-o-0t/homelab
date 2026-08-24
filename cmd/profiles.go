package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/config"
)

// withProfiles prepends --profile <name> for each extension that is
// enabled in the root config, activating optional Docker Compose services.
func withProfiles(cfgDir string, args ...string) []string {
	var flags []string
	for _, profile := range activeProfiles(cfgDir) {
		flags = append(flags, "--profile", profile)
	}
	if len(flags) == 0 {
		return args
	}
	return append(flags, args...)
}

// activeProfiles returns the compose profile names the enabled extensions map
// to. Callers that need the bare names rather than --profile flags (the port
// preflight, which must know which containers actually start) use this.
func activeProfiles(cfgDir string) []string {
	cfgFile := config.RootConfigFile(cfgDir, rootFlags.configFile)
	cfg, _ := config.Load(cfgFile)
	if cfg == nil {
		return nil
	}
	var profiles []string
	for _, ext := range cfg.Extensions {
		layer, ok := extRegistry().Get(config.ResolveExtension(ext))
		if !ok {
			continue
		}
		if profile := layer.Profile(); profile != "" {
			profiles = append(profiles, profile)
		}
	}
	return profiles
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
