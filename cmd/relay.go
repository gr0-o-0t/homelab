package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
)

// exec and logs act on a container, which runs on whichever node Swarm put
// it on. On the leader, a task on another node is handed to that node's
// agent (homelab-<host>.<tailnet>:7443, same cluster secret), which runs the
// same command against its own engine; worker agents accept only exec/logs.

// relayRun runs argv on another agent; a var so tests stand in for it.
var relayRun = runOnAgent

// relayCluster is this agent's cluster settings: the secret and tailnet
// name the relay needs. A var for tests.
var relayCluster = func() (agentstate.Cluster, error) {
	sm, err := openSecrets()
	if err != nil {
		return agentstate.Cluster{}, err
	}
	return agentstate.LoadCluster(agentstate.At(agentstate.Root), sm)
}

// locateTask is bootstrap.LocateTask on this engine; without Docker, local.
func locateTask(svc string) (string, bool, error) {
	api, err := newSwarmAPI()
	if err != nil {
		return "", true, nil
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	return bootstrap.LocateTask(cmdContext(), api, svc)
}

// relayToTaskNode forwards this invocation to the agent of the node running
// svc's task. handled is false when the task runs here (or nowhere): the
// caller then runs the command itself. Once handled, err is errForwarded
// with the remote exit code, or the reason it could not be forwarded.
func relayToTaskNode(svc string) (handled bool, err error) {
	host, local, err := locateTask(svc)
	if err != nil {
		return true, fmt.Errorf("finding the node running %s: %w", svc, err)
	}
	if local {
		return false, nil
	}
	cl, err := relayCluster()
	if err != nil {
		return true, fmt.Errorf("reading the cluster settings: %w", err)
	}
	if cl.Secret == "" || cl.Tailnet == "" {
		return true, errors.New("the cluster secret or tailnet name is missing from the agent's state — rerun `homelab init`")
	}
	argv := invokedArgs
	if argv == nil {
		argv = os.Args[1:]
	}
	c := &remote.Client{Endpoint: bootstrap.NodeEndpoint(host, cl.Tailnet), Secret: cl.Secret, Version: Version}
	code, err := relayRun(c, forwardableArgv(argv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s on %s: %s\n", svc, host, err)
		if code == 0 {
			code = 1
		}
	}
	return true, errForwarded{code: code}
}

// forwardableArgv drops the root flags the agent gave this process
// (--config-dir, --config, --no-record), which another agent refuses. Only
// the flags before the command name are looked at: after it, they are the
// command's (`homelab exec svc grep --config x`).
func forwardableArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return append(out, argv[i:]...)
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		switch name {
		case "config-dir", "config":
			if !hasValue {
				i++ // and its value
			}
		case "no-record":
		default:
			out = append(out, a)
		}
	}
	return out
}
