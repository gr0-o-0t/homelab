package cmd

import "testing"

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
