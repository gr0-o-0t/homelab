package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"

	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
)

// shipLeader runs argv on the leader agent with stdin, returning its exit
// code; stdout is captured, stderr goes to the terminal. No TTY, so a
// `docker save` stream passes through intact. A var so tests can stand in
// for the agent.
var shipLeader = func(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
	c, err := remote.LoadClient(Version, remote.KeyringSecret)
	if err != nil {
		return 1, err
	}
	return c.Run(ctx, remote.RunOptions{Argv: argv, Stdin: stdin, Stdout: stdout, Stderr: os.Stderr})
}

// forwardDeploy is `homelab up` / `update` on a PC: a locally built image
// (compose pull_policy: never) is never pulled, so the leader can only have
// it if a PC ships it. Each local image the services deploy is sent once,
// before the command is forwarded: for up only when the leader lacks it (up
// never takes a rebuilt image, R53), for update when the leader's differs
// from this PC's. An image this PC does not have (or no Docker here) is left
// to the leader, which says which PC to run the command from.
func forwardDeploy(argv []string) (int, error) {
	resolveMu.Lock()
	c, _, err := rootCmd.Find(argv)
	resolveMu.Unlock()
	if err != nil {
		return 1, err
	}
	update := c == serviceUpdateCmd
	all, _ := c.Flags().GetBool("all")
	group, _ := c.Flags().GetString("group")
	if !all && group == "" && len(c.Flags().Args()) == 0 {
		return forwardRun(argv) // no target: the command itself refuses
	}
	ask := append(rootFlagsOf(argv), "images")
	ask = append(ask, c.Flags().Args()...)
	if all {
		ask = append(ask, "-a")
	}
	if group != "" {
		ask = append(ask, "--group", group)
	}
	ask = append(ask, "--json")

	ctx := cmdContext()
	infos, err := leaderImages(ctx, ask)
	if err != nil {
		// The command itself still runs: it reports a bad service and goes on
		// with the others, and an older agent without `images --json` must
		// not block every up/update.
		fmt.Fprintf(os.Stderr, "warning: could not check local images (%s); not shipping\n", err)
		return forwardRun(argv)
	}
	warned := map[string]bool{}
	for _, info := range infos {
		if info.Local && !warned[info.Ref] {
			warned[info.Ref] = true
			warnIfQualifiedLocal(os.Stderr, info.Ref)
		}
	}
	if err := shipLocalImages(ctx, rootFlagsOf(argv), infos, update); err != nil {
		return 1, err
	}
	return forwardRun(argv)
}

// leaderImages is the leader's `images --json` answer to ask.
func leaderImages(ctx context.Context, ask []string) ([]imageInfo, error) {
	var out bytes.Buffer
	code, err := shipLeader(ctx, ask, nil, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d", code)
	}
	if err != nil {
		return nil, err
	}
	var infos []imageInfo
	if err := json.Unmarshal(out.Bytes(), &infos); err != nil {
		return nil, fmt.Errorf("reading the leader's answer: %w", err)
	}
	return infos, nil
}

// shipLocalImages ships each distinct local ref in infos that this PC has
// and the leader needs (see forwardDeploy).
func shipLocalImages(ctx context.Context, rootFlags []string, infos []imageInfo, update bool) error {
	var pc imageSaver // connected on the first local image
	defer func() {
		if c, ok := pc.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	seen := map[string]bool{}
	for _, info := range infos {
		if !info.Local || seen[info.Ref] {
			continue
		}
		seen[info.Ref] = true
		if pc == nil {
			var err error
			if pc, err = localImages(); err != nil {
				return nil // no Docker on this PC: it has no image to ship
			}
		}
		img, _, err := pc.ImageInspectWithRaw(ctx, info.Ref)
		switch {
		case errdefs.IsNotFound(err), client.IsErrConnectionFailed(err):
			continue // not built here, or no Docker running on this PC
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: inspecting %s on this PC: %s; not shipping it\n", info.Ref, err)
			continue
		}
		id := swarm.ImageIdentity(img)
		if info.Identity == id || (!update && info.Identity != "") {
			continue
		}
		fmt.Printf("shipping %s (%s) to the leader…\n", info.Ref, units.HumanSize(float64(img.Size)))
		if err := shipImage(ctx, pc, rootFlags, info.Ref, id); err != nil {
			return err
		}
	}
	return nil
}

// shipImage streams `docker save ref` from this PC into `images load` on the
// leader and checks the leader now has the identity id.
func shipImage(ctx context.Context, pc imageSaver, rootFlags []string, ref, id string) error {
	rc, err := pc.ImageSave(ctx, []string{ref})
	if err != nil {
		return fmt.Errorf("saving image %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	code, err := shipLeader(ctx, append(rootFlags, "images", "load", ref), rc, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d", code)
	}
	if err != nil {
		return fmt.Errorf("shipping image %s to the leader: %w", ref, err)
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var got struct{ Ref, Identity string }
		if json.Unmarshal(sc.Bytes(), &got) != nil || got.Ref != ref {
			continue
		}
		if got.Identity != id {
			return fmt.Errorf("image %s on the leader is %s after the load, not this PC's %s", ref, got.Identity, id)
		}
		return nil
	}
	return fmt.Errorf("shipping image %s to the leader: the load did not report it", ref)
}
