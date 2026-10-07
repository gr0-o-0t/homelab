package ygg

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/configgen/goldentest"
)

// The mesh blocks are what ygg wrote itself before rendering
// moved to configgen; see goldentest.
func TestGolden(t *testing.T) {
	for _, svc := range goldentest.Services {
		root := t.TempDir()
		goldentest.Install(t, root, svc)
		l := newForTest(root, noopRestart)
		e, err := configgen.Resolve(root, svc, svc, nil)
		require.NoError(t, err)
		require.NoError(t, l.Configure(svc, svc, e.Ports))
		blocks, err := configgen.Render(l, e)
		require.NoError(t, err)
		for _, b := range blocks {
			require.NoError(t, configgen.WriteFile(root, l.ConfDir(), svc, b.PortName, b.Content))
		}
		goldentest.Check(t, root, svc, l.ConfDir())
	}
}
