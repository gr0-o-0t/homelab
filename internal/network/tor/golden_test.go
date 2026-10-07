package tor

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/configgen/goldentest"
)

// The onion blocks are byte-for-byte what tor wrote itself before rendering
// moved to configgen; see goldentest.
func TestGolden(t *testing.T) {
	for _, svc := range goldentest.Services {
		root := t.TempDir()
		goldentest.Install(t, root, svc)
		l := newForTest(root, noopReload)
		e, err := configgen.Resolve(root, svc, svc, nil)
		require.NoError(t, err)
		require.NoError(t, l.Configure(svc, svc, e.Ports))
		blocks, err := configgen.Render(l, e)
		require.NoError(t, err)
		goldentest.Check(t, svc, l.LegacyConfDir(), blocks)
	}
}
