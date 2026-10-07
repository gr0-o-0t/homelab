package layers_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/network/layers"
)

// The registry is the only list of layers, so its order is the display order
// everywhere, and each layer must own a distinct conf dir and flag.
func TestStatic_OrderAndIdentity(t *testing.T) {
	var names []string
	dirs, flags := map[string]bool{}, map[string]bool{}
	for _, l := range layers.Static() {
		names = append(names, l.Name())
		assert.False(t, dirs[l.LegacyConfDir()], "%s: conf dir %s is shared", l.Name(), l.LegacyConfDir())
		dirs[l.LegacyConfDir()] = true
		if l.Flag() != "" {
			assert.False(t, flags[l.Flag()], "%s: flag --%s is shared", l.Name(), l.Flag())
			flags[l.Flag()] = true
		}
	}
	assert.Equal(t, []string{"ts", "cf", "tor", "i2p", "ygg"}, names)
	assert.Len(t, flags, len(names)-1, "only the private layer has no flag")
}
