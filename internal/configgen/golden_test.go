package configgen_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/configgen/goldentest"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/i2p"
	"github.com/groot/homelab/internal/network/tailscale"
)

// The Host-routed layers render the same files they did before rendering was
// shared with tor and ygg; see goldentest. tor and ygg check theirs in their
// own packages, which can fake the daemon.
func TestGolden_HostLayers(t *testing.T) {
	for _, svc := range goldentest.Services {
		root := t.TempDir()
		goldentest.Install(t, root, svc)
		for _, l := range []network.NetworkLayer{
			tailscale.New(root, nil, nil), cf.New(root, nil, nil), i2p.New(root, nil, nil),
		} {
			e, err := configgen.Resolve(root, svc, svc, nil)
			require.NoError(t, err)
			blocks, err := configgen.Render(l, e)
			require.NoError(t, err)
			for _, b := range blocks {
				require.NoError(t, configgen.WriteFile(root, l.ConfDir(), svc, b.PortName, b.Content))
			}
			goldentest.Check(t, root, svc, l.ConfDir())
		}
	}
}
