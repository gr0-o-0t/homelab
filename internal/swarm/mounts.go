package swarm

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/api/types/mount"
)

// configMode is the mode bind-mounted service files get as Swarm configs,
// which are read-only whatever the compose file said.
const configMode = 0o444

// translateMounts turns a container's volumes into Swarm mounts. Binds of
// paths under the service directory become FileConfigs (they must reach
// whichever node runs the task); other binds stay host binds. It also returns
// the named volumes used.
func translateMounts(stack, dir, specName string, proj *types.Project, svc types.ServiceConfig,
) ([]mount.Mount, []FileConfig, []string, error) {
	var (
		mounts []mount.Mount
		cfgs   []FileConfig
		vols   []string
	)
	for _, v := range svc.Volumes {
		switch v.Type {
		case types.VolumeTypeBind:
			if rel, ok := under(dir, v.Source); ok {
				if !v.ReadOnly { // a config is read-only; don't silently drop writes
					return nil, nil, nil, unsupported(stack, svc.Name, "writable bind ./"+rel)
				}
				cfgs = append(cfgs, FileConfig{Service: specName, Target: v.Target, Source: rel, Mode: configMode})
				continue
			}
			mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
		case types.VolumeTypeVolume:
			m := mount.Mount{Type: mount.TypeVolume, Target: v.Target, ReadOnly: v.ReadOnly}
			if v.Source != "" { // empty: anonymous volume
				m.Source = volumeName(stack, v.Source, proj.Volumes[v.Source])
				vols = append(vols, m.Source)
			}
			if v.Volume != nil && v.Volume.NoCopy {
				m.VolumeOptions = &mount.VolumeOptions{NoCopy: true}
			}
			mounts = append(mounts, m)
		case types.VolumeTypeTmpfs:
			m := mount.Mount{Type: mount.TypeTmpfs, Target: v.Target}
			if v.Tmpfs != nil && v.Tmpfs.Size != 0 {
				m.TmpfsOptions = &mount.TmpfsOptions{SizeBytes: int64(v.Tmpfs.Size)}
			}
			mounts = append(mounts, m)
		default:
			return nil, nil, nil, unsupported(stack, svc.Name, "volume type "+v.Type)
		}
	}
	for _, t := range svc.Tmpfs { // "path[:options]"; options are dropped
		target, _, _ := strings.Cut(t, ":")
		mounts = append(mounts, mount.Mount{Type: mount.TypeTmpfs, Target: target})
	}
	return mounts, cfgs, vols, nil
}

// Bind is a bind mount of a path inside the service directory.
type Bind struct {
	Rel      string // slash-separated, relative to the service dir; "." is the dir itself
	ReadOnly bool   // false when any container mounts it writable
}

// BindSources lists what svcDir's docker-compose.yml bind-mounts from inside
// svcDir — the files and directories Translate turns into Swarm configs, or
// refuses when writable — sorted by path, one entry per path (writable when
// any mount of it is). env interpolates the compose file as Translate's would.
func BindSources(svcDir string, env map[string]string) ([]Bind, error) {
	abs, err := filepath.Abs(svcDir)
	if err != nil {
		return nil, err
	}
	proj, err := loadCompose(abs, filepath.Base(abs), env)
	if err != nil {
		return nil, err
	}
	ro := map[string]bool{}
	for key := range proj.Services {
		svc := proj.Services[key]
		for _, v := range svc.Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			rel, ok := under(abs, v.Source)
			if !ok {
				continue
			}
			if prev, seen := ro[rel]; seen {
				ro[rel] = prev && v.ReadOnly
			} else {
				ro[rel] = v.ReadOnly
			}
		}
	}
	out := make([]Bind, 0, len(ro))
	for _, rel := range slices.Sorted(maps.Keys(ro)) {
		out = append(out, Bind{Rel: rel, ReadOnly: ro[rel]})
	}
	return out, nil
}

// volumeName is the volume's compose name: when set (keeps gitea_data etc.),
// else <svc>_<key>, which is also what compose itself would have used.
func volumeName(stack, key string, vc types.VolumeConfig) string {
	if vc.Name != "" {
		return vc.Name
	}
	return stack + "_" + key
}

// under reports whether path lies inside dir, and its slash-separated path
// relative to dir.
func under(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
