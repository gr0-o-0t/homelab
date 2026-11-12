package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

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
	var out bytes.Buffer
	code, err := shipLeader(ctx, ask, nil, &out)
	if err == nil && code != 0 {
		err = fmt.Errorf("asking the leader for the services' images: exit status %d", code)
	}
	if err != nil {
		return code, err
	}
	var infos []imageInfo
	if err := json.Unmarshal(out.Bytes(), &infos); err != nil {
		return 1, fmt.Errorf("reading the leader's images: %w", err)
	}

	var pc imageSaver
	seen := map[string]bool{}
	for _, info := range infos {
		if !info.Local || seen[info.Ref] {
			continue
		}
		seen[info.Ref] = true
		if pc == nil {
			if pc, err = localImages(); err != nil {
				break // no Docker on this PC: it has no image to ship
			}
		}
		img, _, err := pc.ImageInspectWithRaw(ctx, info.Ref)
		if err != nil {
			continue // not built here (or no reachable engine)
		}
		id := swarm.ImageIdentity(img)
		if info.Identity == id || (!update && info.Identity != "") {
			continue
		}
		fmt.Printf("shipping %s (%s) to the leader…\n", info.Ref, units.HumanSize(float64(img.Size)))
		if err := shipImage(ctx, pc, rootFlagsOf(argv), info.Ref, id); err != nil {
			return 1, err
		}
	}
	return forwardRun(argv)
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
