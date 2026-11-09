package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/remote"
)

// exec and logs act on a container, which runs on whichever node Swarm put
// it on. Any agent runs them against its own engine by container ID (the
// running task container of <svc>_<svc>, or of the stack's only service).
// On the leader, a task on another node is handed to that node's agent
// (homelab-<host>.<tailnet>:7443, same cluster secret, dialled through the
// leader's tailnet node); worker agents accept only exec/logs and have no
// service files, so they skip validateService.

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

// dockerCLI runs `docker args…` on this process's terminal; a var for tests.
// A non-zero exit becomes errForwarded with that code, as docker exec's own
// exit status is the command's.
var dockerCLI = func(args []string) error {
	c := exec.Command("docker", args...) //nolint:gosec // fixed binary; args built here
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return errForwarded{code: ee.ExitCode()}
	}
	return err
}

// onTaskContainer runs fn with the ID of svc's task container on this node
// (want: -c, see bootstrap.PickService),
// relaying the whole invocation to another agent first when, on the leader,
// the task runs elsewhere.
func onTaskContainer(root, svc, want string, fn func(id string) error) error {
	if err := validName(svc); err != nil {
		return err
	}
	api, err := newSwarmAPI()
	if err != nil {
		if verr := validateService(root, svc); verr != nil {
			return verr
		}
		return fmt.Errorf("connecting to docker: %w", err)
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	ctx := cmdContext()
	info, err := api.Info(ctx)
	if err != nil {
		if verr := validateService(root, svc); verr != nil {
			return verr
		}
		return fmt.Errorf("asking Docker for this node's swarm role: %w", err)
	}
	inSwarm := info.Swarm.LocalNodeState == dswarm.LocalNodeStateActive
	worker := inSwarm && !info.Swarm.ControlAvailable
	if !worker {
		if err := validateService(root, svc); err != nil {
			return err
		}
	}
	if inSwarm && !worker {
		host, local, err := bootstrap.LocateTask(ctx, api, svc, want)
		if err != nil {
			return fmt.Errorf("finding the node running %s: %w", svc, err)
		}
		if !local {
			return relayTo(host, svc)
		}
	}
	id, err := bootstrap.TaskContainer(ctx, api, svc, want)
	if err != nil {
		return err
	}
	return fn(id)
}

// relayTo forwards this invocation to the agent on host and returns
// errForwarded with its exit code, or why it could not be forwarded.
func relayTo(host, svc string) error {
	cl, err := relayCluster()
	if err != nil {
		return fmt.Errorf("reading the cluster settings: %w", err)
	}
	if cl.Secret == "" || cl.Tailnet == "" {
		return errors.New("the cluster secret or tailnet name is missing from the agent's state — rerun `homelab init`")
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
	return errForwarded{code: code}
}

// dockerExec is docker's verb (and ours).
const dockerExec = "exec"

// dockerExecArgs is `docker exec` for today's exec flags: stdin always
// attached, a TTY when there is one (or -t) unless -T.
func dockerExecArgs(id string, tty bool, cmdArgs []string) []string {
	a := []string{dockerExec, "-i"}
	if tty && !execFlags.noTTY {
		a = append(a, "-t")
	}
	if execFlags.user != "" {
		a = append(a, "--user", execFlags.user)
	}
	if execFlags.workdir != "" {
		a = append(a, "--workdir", execFlags.workdir)
	}
	for _, e := range execFlags.env {
		a = append(a, "--env", e)
	}
	a = append(a, id)
	return append(a, cmdArgs...)
}

// forwardableArgv drops the root flags the agent gave this process
// (--config-dir, --config), which another agent refuses. Only
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
		default:
			out = append(out, a)
		}
	}
	return out
}
