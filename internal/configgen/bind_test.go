package configgen_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/configgen/goldentest"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/cf"
	"github.com/groot/homelab/internal/network/tailscale"
)

// In cluster mode Caddy receives tailnet traffic through the in-process
// network hl-ts, so a tailnet site binds to it — before anything else in the
// block — while a Cloudflare site (reached over the overlay) binds nothing.
func TestRender_TailnetSiteHasBind(t *testing.T) {
	root := t.TempDir()
	goldentest.Install(t, root, "single")
	e, err := configgen.Resolve(root, "single", "single", nil)
	require.NoError(t, err)

	blocks, err := configgen.Render(tailscale.New(root, nil, nil), e)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Contains(t, blocks[0].Content, " {\n\tbind "+network.TailnetBind+"\n\timport wildcard_tls\n")

	blocks, err = configgen.Render(cf.New(root, nil, nil), e)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.NotContains(t, blocks[0].Content, "bind")
}

func TestRenderL4_ListensOnTailnet(t *testing.T) {
	e := configgen.Exposure{Service: "gitea", Ports: []configgen.PortSelection{
		{Name: "ssh", Port: 2222, Listen: 22, Protocols: []string{"tcp"}},
		{Name: "dns", Port: 53, Listen: 53, Protocols: []string{"tcp", "udp"}},
	}}
	got := configgen.RenderL4(e)
	assert.Contains(t, got, "\n"+network.TailnetBind+":22 {\n\troute {\n\t\tproxy gitea:2222\n")
	assert.Contains(t, got, "\n"+network.TailnetBind+":53 {\n\troute {\n\t\tproxy gitea:53\n")
	assert.Contains(t, got, "\nudp/"+network.TailnetBind+":53 {\n\troute {\n\t\tproxy udp/gitea:53\n")
	assert.False(t, strings.Contains(got, "\n:22"), got)
}
