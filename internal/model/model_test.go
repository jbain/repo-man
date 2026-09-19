package model

import "testing"

func TestRemoteWebURL(t *testing.T) {
	tests := []struct {
		name   string
		remote Remote
		want   string
	}{
		{
			// The case this exists for: an ssh remote is not something a
			// browser can open, but its coordinates are known.
			name:   "ssh scp-style",
			remote: Remote{URL: "git@github.com:jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
			want:   "https://github.com/jbain/repo-man",
		},
		{
			name:   "ssh url form",
			remote: Remote{URL: "ssh://git@github.com/jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
			want:   "https://github.com/jbain/repo-man",
		},
		{
			name:   "https is kept as-is",
			remote: Remote{URL: "https://github.com/jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
			want:   "https://github.com/jbain/repo-man.git",
		},
		{
			// A self-hosted forge with no TLS must not be linked over https.
			name:   "plain http is kept as-is",
			remote: Remote{URL: "http://git.lan/team/thing.git", Host: "git.lan", Owner: "team", Name: "thing"},
			want:   "http://git.lan/team/thing.git",
		},
		{
			name:   "unparseable and not http",
			remote: Remote{URL: "/srv/mirrors/thing.git"},
			want:   "",
		},
		{
			name:   "no remote at all",
			remote: Remote{},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.remote.WebURL(); got != tt.want {
				t.Errorf("WebURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGhostWebURL(t *testing.T) {
	g := Ghost{Host: "github.com", Owner: "jbain", Name: "side-project", CloneURL: "https://github.com/jbain/side-project.git"}
	if got, want := g.WebURL(), "https://github.com/jbain/side-project.git"; got != want {
		t.Errorf("WebURL() = %q, want %q", got, want)
	}

	// A provider listing that hands back an ssh clone URL still has to produce
	// a link, since a ghost row has nothing else to offer.
	ssh := Ghost{Host: "github.com", Owner: "jbain", Name: "side-project", CloneURL: "git@github.com:jbain/side-project.git"}
	if got, want := ssh.WebURL(), "https://github.com/jbain/side-project"; got != want {
		t.Errorf("WebURL() = %q, want %q", got, want)
	}
}

func TestSortInterleavesGhostsWithCheckouts(t *testing.T) {
	n := &Node{Children: []*Node{
		{Name: "zeta", Kind: KindRepo},
		{Name: "beta", Kind: KindGhost},
		{Name: "tools", Kind: KindDir},
		{Name: "alpha", Kind: KindRepo},
	}}
	n.Sort()

	want := []string{"tools", "alpha", "beta", "zeta"}
	for i, w := range want {
		if n.Children[i].Name != w {
			got := make([]string, len(n.Children))
			for j, c := range n.Children {
				got[j] = c.Name
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
