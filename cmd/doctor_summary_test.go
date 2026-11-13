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
