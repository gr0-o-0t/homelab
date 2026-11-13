package diagnostics

import (
	"os"
	"strings"
	"testing"
)

// The agent runs in a container where the host's /etc/docker/daemon.json is
// never mounted, so no check may read it: it warned on every run.
func TestNoDaemonConfigCheck(t *testing.T) {
	src, err := os.ReadFile("check.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"daemon.json", "checkLogRotation", "log rotation"} {
		if strings.Contains(string(src), s) {
			t.Errorf("check.go still mentions %q", s)
		}
	}
}
