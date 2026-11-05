package swarm

import (
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

// WithEnv makes the container read its environment from a Swarm secret: the
// command becomes the homelab shim (from the shared read-only homelab-shim
// volume at /hl) wrapping the original argv.
//
// Precedence: the spec's own Command/Args (compose entrypoint/command) win;
// origEntrypoint/origCmd (the image's) fill what compose left unset. As in
// Docker, a compose entrypoint discards the image's default command.
func WithEnv(spec *swarm.ServiceSpec, secretName string, origEntrypoint, origCmd []string) {
	cs := spec.TaskTemplate.ContainerSpec
	entry, cmd := cs.Command, cs.Args
	if len(entry) == 0 {
		entry = origEntrypoint
		if len(cmd) == 0 {
			cmd = origCmd
		}
	}
	argv := []string{"/hl/homelab", "envshim", "--"}
	argv = append(argv, entry...)
	argv = append(argv, cmd...)
	cs.Command, cs.Args = argv, nil
	cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "homelab-shim", Target: "/hl", ReadOnly: true})
	cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
		SecretName: secretName,
		File:       &swarm.SecretReferenceFileTarget{Name: "homelab-env", UID: "0", GID: "0", Mode: 0o444},
	})
}
