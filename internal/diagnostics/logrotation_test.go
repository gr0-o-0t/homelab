package diagnostics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Unbounded container logs are a silent host-killer: the default json-file
// driver never rotates, so one crash-looping container fills the disk and takes
// every service down with it.
func TestCheckLogRotation(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "daemon.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name   string
		body   string
		status Status
		want   string
	}{
		{
			name:   "max-size set",
			body:   `{"log-driver":"json-file","log-opts":{"max-size":"10m","max-file":"3"}}`,
			status: StatusPass,
			want:   "10m",
		},
		{
			name:   "journald rotates by default",
			body:   `{"log-driver":"journald"}`,
			status: StatusPass,
			want:   "journald",
		},
		{
			name:   "driver set but no cap",
			body:   `{"log-driver":"json-file"}`,
			status: StatusWarn,
			want:   "no log cap",
		},
		{
			name:   "empty config",
			body:   `{}`,
			status: StatusWarn,
			want:   "no log cap",
		},
		{
			name:   "malformed json",
			body:   `{"log-driver":`,
			status: StatusWarn,
			want:   "not valid JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLogRotation(write(t, tc.body))
			if got.Status != tc.status {
				t.Errorf("status = %v, want %v (message: %s)", got.Status, tc.status, got.Message)
			}
			if !strings.Contains(got.Message, tc.want) {
				t.Errorf("message %q should mention %q", got.Message, tc.want)
			}
		})
	}

	// A missing daemon.json is the default on a fresh host, and the default is
	// uncapped — so absence must warn, not pass.
	t.Run("missing file warns", func(t *testing.T) {
		got := checkLogRotation(filepath.Join(t.TempDir(), "absent.json"))
		if got.Status != StatusWarn {
			t.Errorf("status = %v, want warn", got.Status)
		}
		if !strings.Contains(got.Message, "max-size") {
			t.Errorf("message should include the fix, got %q", got.Message)
		}
	})
}
