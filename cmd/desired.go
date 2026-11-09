package cmd

import (
	"fmt"
	"os"

	"github.com/groot/homelab/internal/desired"
)

// recordService notes what a lifecycle command asked of a service in
// state/desired.yaml, before it runs: the agent brings the cluster back to it
// after a reboot. A missing file starts empty — nothing else is recorded as
// running. Failures only warn: the state file never blocks a lifecycle
// command.
func recordService(root, name string, st desired.State) {
	d, _, err := desired.LoadDesired(root)
	if err == nil {
		d.SetService(name, st)
		err = d.Save(root)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: desired state: %v\n", err)
	}
}
