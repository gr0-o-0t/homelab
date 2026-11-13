package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/release"
)

// releaseAPI is the repository's API base; a var so tests point it at
// httptest. releaseTransport is the base transport of both HTTP clients
// (tests trust their own CA through it).
var (
	releaseAPI       = release.DefaultAPI
	releaseTransport = func() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }
)

// apiTimeout bounds one GitHub API call (a small JSON document), the body
// included; downloadDeadline bounds a whole download. Vars for tests.
var (
	apiTimeout       = 30 * time.Second
	downloadDeadline = 15 * time.Minute
)

// releaseClient is the client for the release API (Latest, ByTag).
func releaseClient() *http.Client {
	return &http.Client{Timeout: apiTimeout, Transport: releaseTransport()}
}

// downloadClient is the client for a release's assets. It has no overall
// Timeout — that would cover reading the body, and a ~60 MB archive on a
// slow link takes minutes — but each step that can hang is bounded: dial,
// TLS handshake, response headers; the caller's context (downloadDeadline)
// bounds the rest.
func downloadClient() *http.Client {
	t := releaseTransport()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: t}
}

// updateLeaderVersion asks the leader agent its version; a var for tests.
var updateLeaderVersion = func(ctx context.Context) (string, error) {
	c, err := upgradeClient()
	if err != nil {
		return "", err
	}
	id, err := upgradeAsk(ctx, c)
	if err != nil {
		return "", err
	}
	return id.Version, nil
}

// updateRunsLocally reports whether c is `homelab update` with no target:
// the release check runs on this PC (it is the CLI's own version that is
// compared, and the PC may have no agent), so it is not forwarded.
func updateRunsLocally(c *cobra.Command) bool {
	return c == serviceUpdateCmd && bareUpdate(c)
}

// bareUpdate: no service, -a or --group on c.
func bareUpdate(c *cobra.Command) bool {
	return c.Flags().NArg() == 0 &&
		!c.Flags().Changed("all") && !c.Flags().Changed("group")
}

// runUpdateCheck reports this CLI's and the leader's versions and whether a
// newer release exists. An unreachable leader is reported and skipped; only
// an unreachable GitHub is an error.
func runUpdateCheck(ctx context.Context, w io.Writer) error {
	release.UserAgent = "homelab/" + Version
	var agents string
	if v, err := updateLeaderVersion(ctx); err != nil {
		agents = fmt.Sprintf("leader: unreachable (%s)", err)
	} else {
		agents = "agents: leader " + v
	}
	_, _ = fmt.Fprintf(w, "homelab CLI %s (this PC), %s\n", Version, agents)

	rel, err := release.Latest(ctx, releaseClient(), releaseAPI)
	if err != nil {
		return fmt.Errorf("checking for a newer release: %w", err)
	}
	switch reason, dev := release.DevBuild(Version); {
	case dev:
		_, _ = fmt.Fprintf(w, "this CLI is a development build (%s); the latest release is %s\n", reason, rel.Tag)
	case release.Newer(Version, rel.Tag):
		_, _ = fmt.Fprintf(w, "homelab %s is available: run homelab upgrade\n%s\n", rel.Tag, releaseURL(rel.Tag))
	default:
		_, _ = fmt.Fprintf(w, "homelab is up to date (%s)\n", Version)
	}
	return nil
}

func releaseURL(tag string) string {
	return "https://github.com/gr0-o-0t/homelab/releases/tag/" + tag
}
