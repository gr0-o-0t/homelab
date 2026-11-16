package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPassedSummary(t *testing.T) {
	for _, tc := range []struct {
		warns int
		want  string
	}{
		{0, "All checks passed."},
		{1, "Passed with 1 warning."},
		{2, "Passed with 2 warnings."},
	} {
		if got := passedSummary(tc.warns); got != tc.want {
			t.Errorf("passedSummary(%d) = %q, want %q", tc.warns, got, tc.want)
		}
	}
}

func TestAllServicesSummary(t *testing.T) {
	for warns, want := range map[int]string{0: "All services healthy.", 1: "Passed with 1 warning.", 3: "Passed with 3 warnings."} {
		if got := allServicesSummary(warns); got != want {
			t.Errorf("allServicesSummary(%d) = %q, want %q", warns, got, want)
		}
	}
}

// `doctor --all` exits non-zero when a service fails its checks, so scripts
// and front ends can tell.
func TestDoctorAll_FailureIsAnError(t *testing.T) {
	origAll, origDir := doctorAllFlag, rootFlags.configDir
	t.Cleanup(func() { doctorAllFlag, rootFlags.configDir = origAll, origDir })
	rootFlags.configDir = t.TempDir()
	// A service folder without docker-compose.yml fails its config checks.
	if err := os.MkdirAll(filepath.Join(rootFlags.configDir, "services", "broken"), 0o750); err != nil {
		t.Fatal(err)
	}
	doctorAllFlag = true
	if err := runDoctor(doctorCmd, nil); err == nil {
		t.Fatal("doctor --all returned nil with a failing service")
	}
}

// Plain `doctor` exits non-zero when a check fails: in an empty config dir
// the agent's Caddy socket is missing.
func TestDoctor_FailureIsAnError(t *testing.T) {
	origDir := rootFlags.configDir
	t.Cleanup(func() { rootFlags.configDir = origDir })
	rootFlags.configDir = t.TempDir()
	if err := runDoctor(doctorCmd, nil); err == nil {
		t.Fatal("doctor returned nil with failing checks")
	}
}
