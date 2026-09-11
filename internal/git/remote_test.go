package git

import (
	"testing"

	"github.com/jbain/repo-man/internal/model"
)

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want model.Remote
	}{
		{
			name: "https with .git suffix",
			raw:  "https://github.com/jbain/repo-man.git",
			want: model.Remote{URL: "https://github.com/jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "https without .git suffix",
			raw:  "https://github.com/jbain/repo-man",
			want: model.Remote{URL: "https://github.com/jbain/repo-man", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "scp-style",
			raw:  "git@github.com:jbain/repo-man.git",
			want: model.Remote{URL: "git@github.com:jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "ssh scheme",
			raw:  "ssh://git@github.com/jbain/repo-man",
			want: model.Remote{URL: "ssh://git@github.com/jbain/repo-man", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "ssh scheme with port",
			raw:  "ssh://git@github.com:2222/jbain/repo-man.git",
			want: model.Remote{URL: "ssh://git@github.com:2222/jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "https with port",
			raw:  "https://git.example.com:8443/jbain/repo-man.git",
			want: model.Remote{URL: "https://git.example.com:8443/jbain/repo-man.git", Host: "git.example.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "https with userinfo",
			raw:  "https://oauth2:token@gitlab.com/jbain/repo-man.git",
			want: model.Remote{URL: "https://oauth2:token@gitlab.com/jbain/repo-man.git", Host: "gitlab.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "scp-style with userinfo-like prefix",
			raw:  "deploy@git.example.com:jbain/repo-man.git",
			want: model.Remote{URL: "deploy@git.example.com:jbain/repo-man.git", Host: "git.example.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "deep path subgroup",
			raw:  "https://gitlab.com/group/subgroup/name.git",
			want: model.Remote{URL: "https://gitlab.com/group/subgroup/name.git", Host: "gitlab.com", Owner: "group/subgroup", Name: "name"},
		},
		{
			name: "host is lowercased",
			raw:  "https://GitHub.com/jbain/repo-man.git",
			want: model.Remote{URL: "https://GitHub.com/jbain/repo-man.git", Host: "github.com", Owner: "jbain", Name: "repo-man"},
		},
		{
			name: "unrecognizable form keeps URL only",
			raw:  "some nonsense that is not a url",
			want: model.Remote{URL: "some nonsense that is not a url"},
		},
		{
			name: "empty input",
			raw:  "",
			want: model.Remote{URL: ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRemoteURL(tc.raw)
			if got != tc.want {
				t.Errorf("ParseRemoteURL(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidateCloneURL_Accept(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"https", "https://github.com/jbain/repo-man.git", "https://github.com/jbain/repo-man.git"},
		{"https no .git", "https://github.com/jbain/repo-man", "https://github.com/jbain/repo-man"},
		{"http", "http://git.example.com/jbain/repo-man.git", "http://git.example.com/jbain/repo-man.git"},
		{"ssh scheme", "ssh://git@github.com/jbain/repo-man", "ssh://git@github.com/jbain/repo-man"},
		{"scp-style", "git@github.com:jbain/repo-man.git", "git@github.com:jbain/repo-man.git"},
		{"bare owner/name shorthand", "jbain/repo-man", "https://github.com/jbain/repo-man"},
		{"host/owner/name shorthand", "gitlab.com/jbain/repo-man", "https://gitlab.com/jbain/repo-man"},
		{"trims whitespace", "  jbain/repo-man  ", "https://github.com/jbain/repo-man"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCloneURL(tc.raw)
			if err != nil {
				t.Fatalf("ValidateCloneURL(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ValidateCloneURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidateCloneURL_Reject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"leading dash", "--upload-pack=touch /tmp/pwned"},
		{"leading dash shorthand-looking", "-oProxyCommand=id"},
		{"ext transport", "ext::sh -c id"},
		{"ext transport embedded", "https://ext::sh -c id"},
		{"file scheme", "file:///etc/passwd"},
		{"file scheme uppercase", "FILE:///etc/passwd"},
		{"bare absolute path", "/home/user/project"},
		{"relative path", "./relative/path"},
		{"parent relative path", "../escape"},
		{"home-relative path", "~/project"},
		{"windows path", `C:\Users\me\project`},
		{"contains NUL", "jbain/repo-man\x00.git"},
		{"contains newline", "jbain/repo-man\ngit fetch"},
		{"contains embedded carriage return", "jbain/repo\r\nman"},
		{"nonsense", "not a url at all!!"},
		{"too many shorthand segments", "a/b/c/d"},
		{"three segments no dotted host", "owner/mid/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCloneURL(tc.raw)
			if err == nil {
				t.Fatalf("ValidateCloneURL(%q) = %q, want error", tc.raw, got)
			}
		})
	}
}
