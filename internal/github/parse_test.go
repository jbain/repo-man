package github

import (
	"strings"
	"testing"
	"time"
)

func TestParseRepoList(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		login string
		json  string
		want  []struct {
			name          string
			description   string
			private       bool
			fork          bool
			archived      bool
			defaultBranch string
			cloneURL      string
			updatedAt     string // RFC3339, "" means zero time
		}
		wantErr bool
	}{
		{
			name:  "typical mixed set",
			host:  "github.com",
			login: "jbain",
			json: `[
				{"name":"aoe-sandbox","description":"Agent of Empires sandbox docker images","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-09-08T05:38:04Z","url":"https://github.com/jbain/aoe-sandbox","defaultBranchRef":{"name":"main"}},
				{"name":"logseq-selfhost","description":"self-hosted logseq","isPrivate":false,"isFork":true,"isArchived":false,"updatedAt":"2026-09-07T09:16:18Z","url":"https://github.com/jbain/logseq-selfhost","defaultBranchRef":{"name":"master"}},
				{"name":"homelab","description":"","isPrivate":true,"isFork":false,"isArchived":false,"updatedAt":"2026-08-26T17:50:50Z","url":"https://github.com/jbain/homelab","defaultBranchRef":{"name":"main"}}
			]`,
			want: []struct {
				name          string
				description   string
				private       bool
				fork          bool
				archived      bool
				defaultBranch string
				cloneURL      string
				updatedAt     string
			}{
				// sorted case-insensitively by name: aoe-sandbox, homelab, logseq-selfhost
				{"aoe-sandbox", "Agent of Empires sandbox docker images", false, false, false, "main", "https://github.com/jbain/aoe-sandbox.git", "2026-09-08T05:38:04Z"},
				{"homelab", "", true, false, false, "main", "https://github.com/jbain/homelab.git", "2026-08-26T17:50:50Z"},
				{"logseq-selfhost", "self-hosted logseq", false, true, false, "master", "https://github.com/jbain/logseq-selfhost.git", "2026-09-07T09:16:18Z"},
			},
		},
		{
			name:  "null description",
			host:  "github.com",
			login: "jbain",
			json:  `[{"name":"foo","description":null,"isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-01-01T00:00:00Z","url":"x","defaultBranchRef":{"name":"main"}}]`,
			want: []struct {
				name          string
				description   string
				private       bool
				fork          bool
				archived      bool
				defaultBranch string
				cloneURL      string
				updatedAt     string
			}{
				{"foo", "", false, false, false, "main", "https://github.com/jbain/foo.git", "2026-01-01T00:00:00Z"},
			},
		},
		{
			name:  "null defaultBranchRef (empty repository)",
			host:  "github.com",
			login: "jbain",
			json:  `[{"name":"empty-repo","description":"","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-01-01T00:00:00Z","url":"x","defaultBranchRef":null}]`,
			want: []struct {
				name          string
				description   string
				private       bool
				fork          bool
				archived      bool
				defaultBranch string
				cloneURL      string
				updatedAt     string
			}{
				{"empty-repo", "", false, false, false, "", "https://github.com/jbain/empty-repo.git", "2026-01-01T00:00:00Z"},
			},
		},
		{
			name:  "archived repo",
			host:  "github.com",
			login: "acme",
			json:  `[{"name":"old","description":"","isPrivate":false,"isFork":false,"isArchived":true,"updatedAt":"2020-01-01T00:00:00Z","url":"x","defaultBranchRef":{"name":"main"}}]`,
			want: []struct {
				name          string
				description   string
				private       bool
				fork          bool
				archived      bool
				defaultBranch string
				cloneURL      string
				updatedAt     string
			}{
				{"old", "", false, false, true, "main", "https://github.com/acme/old.git", "2020-01-01T00:00:00Z"},
			},
		},
		{
			name:  "empty array",
			host:  "github.com",
			login: "jbain",
			json:  `[]`,
			want: []struct {
				name          string
				description   string
				private       bool
				fork          bool
				archived      bool
				defaultBranch string
				cloneURL      string
				updatedAt     string
			}{},
		},
		{
			name:    "malformed JSON",
			host:    "github.com",
			login:   "jbain",
			json:    `{not valid json`,
			wantErr: true,
		},
		{
			name:    "JSON object instead of array",
			host:    "github.com",
			login:   "jbain",
			json:    `{"name":"foo"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRepoList(tt.host, tt.login, []byte(tt.json))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRepoList() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRepoList() unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseRepoList() returned %d ghosts, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Host != tt.host {
					t.Errorf("ghost[%d].Host = %q, want %q", i, g.Host, tt.host)
				}
				if g.Owner != tt.login {
					t.Errorf("ghost[%d].Owner = %q, want %q", i, g.Owner, tt.login)
				}
				if g.Name != w.name {
					t.Errorf("ghost[%d].Name = %q, want %q", i, g.Name, w.name)
				}
				if g.Description != w.description {
					t.Errorf("ghost[%d].Description = %q, want %q", i, g.Description, w.description)
				}
				if g.Private != w.private {
					t.Errorf("ghost[%d].Private = %v, want %v", i, g.Private, w.private)
				}
				if g.Fork != w.fork {
					t.Errorf("ghost[%d].Fork = %v, want %v", i, g.Fork, w.fork)
				}
				if g.Archived != w.archived {
					t.Errorf("ghost[%d].Archived = %v, want %v", i, g.Archived, w.archived)
				}
				if g.DefaultBranch != w.defaultBranch {
					t.Errorf("ghost[%d].DefaultBranch = %q, want %q", i, g.DefaultBranch, w.defaultBranch)
				}
				if g.CloneURL != w.cloneURL {
					t.Errorf("ghost[%d].CloneURL = %q, want %q", i, g.CloneURL, w.cloneURL)
				}
				wantTime, perr := time.Parse(time.RFC3339, w.updatedAt)
				if perr != nil {
					t.Fatalf("bad test fixture time %q: %v", w.updatedAt, perr)
				}
				if !g.UpdatedAt.Equal(wantTime) {
					t.Errorf("ghost[%d].UpdatedAt = %v, want %v", i, g.UpdatedAt, wantTime)
				}
			}
		})
	}
}

func TestParseRepoListSortsCaseInsensitively(t *testing.T) {
	data := `[
		{"name":"Zebra","description":"","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-01-01T00:00:00Z","url":"x","defaultBranchRef":{"name":"main"}},
		{"name":"apple","description":"","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-01-01T00:00:00Z","url":"x","defaultBranchRef":{"name":"main"}},
		{"name":"Banana","description":"","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-01-01T00:00:00Z","url":"x","defaultBranchRef":{"name":"main"}}
	]`
	got, err := parseRepoList("github.com", "jbain", []byte(data))
	if err != nil {
		t.Fatalf("parseRepoList() error: %v", err)
	}
	var names []string
	for _, g := range got {
		names = append(names, g.Name)
	}
	want := "apple,Banana,Zebra"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("sorted names = %q, want %q", got, want)
	}
}

func TestCloneURL(t *testing.T) {
	tests := []struct {
		host, login, name, want string
	}{
		{"github.com", "jbain", "repo-man", "https://github.com/jbain/repo-man.git"},
		{"github.example.com", "acme-org", "internal-tool", "https://github.example.com/acme-org/internal-tool.git"},
		{"github.com", "jbain", "dots.in.name", "https://github.com/jbain/dots.in.name.git"},
	}
	for _, tt := range tests {
		if got := cloneURL(tt.host, tt.login, tt.name); got != tt.want {
			t.Errorf("cloneURL(%q, %q, %q) = %q, want %q", tt.host, tt.login, tt.name, got, tt.want)
		}
	}
}

func TestValidateLogin(t *testing.T) {
	tests := []struct {
		login   string
		wantErr bool
	}{
		{"jbain", false},
		{"acme-org", false},
		{"a", false},
		{"", true},
		{"foo/bar", true},
		{"-x", true},
		{"--help", true},
	}
	for _, tt := range tests {
		err := validateLogin(tt.login)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateLogin(%q) error = %v, wantErr %v", tt.login, err, tt.wantErr)
		}
	}
}

func TestValidateHost(t *testing.T) {
	tests := []struct {
		host    string
		wantErr bool
	}{
		{"github.com", false},
		{"github.example.com", false},
		{"", true},
		{"github.com/evil", true},
		{"-x", true},
	}
	for _, tt := range tests {
		err := validateHost(tt.host)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateHost(%q) error = %v, wantErr %v", tt.host, err, tt.wantErr)
		}
	}
}
