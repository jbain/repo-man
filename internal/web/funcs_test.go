package web

import "testing"

func TestErrSummaryDropsTheGitCommandPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"git failure", "git status --porcelain=v2 --branch -z: fatal: not a git repository", "fatal: not a git repository"},
		{"not a git error", "permission denied", "permission denied"},
		{"no message after the command", "git status", "git status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errSummary(tt.in); got != tt.want {
				t.Errorf("errSummary(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
