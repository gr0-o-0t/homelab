package configgen

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLayerDisplayURL_Private(t *testing.T) {
	env := map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}
	url := LayerDisplayURL("private", "gitea", env)
	assert.Equal(t, "https://gitea.home.example.com", url)
}

func TestLayerDisplayURL_PrivateFallback(t *testing.T) {
	env := map[string]string{} // no HOME_SUBDOMAIN or DOMAIN
	url := LayerDisplayURL("private", "gitea", env)
	assert.Equal(t, "https://gitea.{HOME_SUBDOMAIN}.{DOMAIN}", url)
}

func TestLayerDisplayURL_CF(t *testing.T) {
	env := map[string]string{"DOMAIN": "example.com"}
	url := LayerDisplayURL("cf", "gitea", env)
	assert.Equal(t, "https://gitea.example.com", url)
}

func TestLayerDisplayURL_Tor(t *testing.T) {
	url := LayerDisplayURL("tor", "gitea", nil)
	assert.Equal(t, "http://gitea.onion (via Caddy)", url)
}

func TestLayerDisplayURL_I2P(t *testing.T) {
	url := LayerDisplayURL("i2p", "gitea", nil)
	assert.Equal(t, "http://gitea.i2p (via Caddy)", url)
}

func TestLayerDisplayURL_Ygg(t *testing.T) {
	url := LayerDisplayURL("ygg", "gitea", nil)
	assert.Equal(t, "http://gitea.ygg", url)
}

func TestLayerDisplayURL_IPFS(t *testing.T) {
	url := LayerDisplayURL("ipfs", "gitea", nil)
	assert.Equal(t, "ipfs://gitea", url)
}

func TestLayerDisplayURL_Unknown(t *testing.T) {
	url := LayerDisplayURL("unknown", "gitea", nil)
	assert.Equal(t, "", url)
}

func TestLayerDisplayURL_EmptyDisplayName(t *testing.T) {
	env := map[string]string{"HOME_SUBDOMAIN": "home", "DOMAIN": "example.com"}
	url := LayerDisplayURL("private", "", env)
	// When both env vars are set, they are resolved even with an empty display name.
	assert.Equal(t, "https://.home.example.com", url)
}

func TestLayerDisplayURL_CFWithoutDomain(t *testing.T) {
	env := map[string]string{} // no DOMAIN
	url := LayerDisplayURL("cf", "gitea", env)
	assert.Equal(t, "https://gitea.{DOMAIN}", url)
}
