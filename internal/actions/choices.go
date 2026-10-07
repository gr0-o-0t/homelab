package actions

import (
	"sort"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/config"
)

// Choices returns the options of a Choice input: its static Choices, or its
// dynamic Source resolved against the config dir root. Backups are listed from
// the default backup directory, newest first.
func Choices(root string, in Input) ([]string, error) {
	switch in.Source {
	case SourceGroups:
		cfg, err := config.Load(config.RootConfigFile(root, ""))
		if err != nil || cfg == nil {
			return nil, err
		}
		var out []string
		for g := range cfg.Groups {
			out = append(out, g)
		}
		sort.Strings(out)
		return out, nil
	case SourceBackups:
		list, err := backup.List(backup.DefaultDir(root))
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Dir)
		}
		return out, nil
	}
	return in.Choices, nil
}
