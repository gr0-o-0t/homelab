package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A read-only declaration points at another service's database: that
// service's own backup dumps it, so the consumer's must not.
func TestBackupPlan_SkipsReadOnly(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "grafana", twoVolCompose, `
databases:
  - postgres:
      database: grafana
      user: grafana
  - postgres:
      database: taash
      user: taash_ro
      readonly: true
`)
	p, err := PlanFor(root, "grafana")
	require.NoError(t, err)
	require.Len(t, p.Databases, 1)
	assert.Equal(t, "grafana", p.Databases[0].Database)
}
