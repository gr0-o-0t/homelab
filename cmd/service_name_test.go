package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// add, doctor <svc> and cf route add|rm refuse a name that is not a service
// name before it reaches a path or a DNS record.
func TestServiceNameIsValidated(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "1")
	t.Cleanup(resetFlags)
	for _, run := range map[string]func([]string) error{
		"add":          func(a []string) error { return runServiceAdd(nil, a) },
		"doctor":       func(a []string) error { return runDoctor(doctorCmd, a) },
		"cf route add": func(a []string) error { return tunnelRouteAddCmd.RunE(tunnelRouteAddCmd, a) },
		"cf route rm":  func(a []string) error { return tunnelRouteRmCmd.RunE(tunnelRouteRmCmd, a) },
	} {
		for _, svc := range []string{"../core", "a/b", "..", "Foo"} {
			err := run([]string{svc})
			if assert.Error(t, err, svc) {
				assert.Contains(t, err.Error(), "invalid service name", svc)
			}
		}
	}
}
