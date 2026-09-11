package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

func TestParseOwners(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  []model.Owner
		isErr bool
	}{
		{"empty", "", nil, false},
		{"bare login defaults to github.com", "jbain", []model.Owner{{Host: "github.com", Login: "jbain"}}, false},
		{"host and login", "github.com/jbain", []model.Owner{{Host: "github.com", Login: "jbain"}}, false},
		{
			"multiple, whitespace tolerated",
			" github.com/jbain , github.com/some-org ",
			[]model.Owner{{Host: "github.com", Login: "jbain"}, {Host: "github.com", Login: "some-org"}},
			false,
		},
		{
			"duplicates collapse, first-seen order preserved",
			"github.com/jbain,jbain,github.com/other",
			[]model.Owner{{Host: "github.com", Login: "jbain"}, {Host: "github.com", Login: "other"}},
			false,
		},
		{"empty entries are skipped", "jbain,,", []model.Owner{{Host: "github.com", Login: "jbain"}}, false},
		{"surrounding slashes trimmed", "/github.com/jbain/", []model.Owner{{Host: "github.com", Login: "jbain"}}, false},
		{"too many segments", "github.com/org/team", nil, true},
		{"missing login", "github.com/", nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOwners(tc.in)
			if tc.isErr {
				if err == nil {
					t.Fatalf("ParseOwners(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOwners(%q) returned %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseOwners(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseOwners(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	root := t.TempDir()
	t.Setenv("REPOMAN_ROOT", root)

	cfg, err := Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(root)
	if cfg.Root != resolved {
		t.Errorf("Root = %q, want %q", cfg.Root, resolved)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.FetchInterval != DefaultFetchInterval {
		t.Errorf("FetchInterval = %v, want %v", cfg.FetchInterval, DefaultFetchInterval)
	}
	if cfg.AuthEnabled() {
		t.Error("auth must be off when no passphrase is set")
	}
}

func TestLoadFlagsBeatEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("REPOMAN_ROOT", root)
	t.Setenv("REPOMAN_ADDR", "from-env:1")
	t.Setenv("REPOMAN_SCAN_INTERVAL", "5m")

	cfg, err := Load([]string{"-addr", "from-flag:2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "from-flag:2" {
		t.Errorf("Addr = %q, want the flag value", cfg.Addr)
	}
	if cfg.ScanInterval != 5*time.Minute {
		t.Errorf("ScanInterval = %v, want the env value 5m", cfg.ScanInterval)
	}
}

func TestLoadPassphraseIsEnvironmentOnly(t *testing.T) {
	// A passphrase passed as a flag would be visible in the process table, so
	// there deliberately is no flag for it.
	root := t.TempDir()
	t.Setenv("REPOMAN_ROOT", root)
	t.Setenv("REPOMAN_PASSPHRASE", "hunter2")

	cfg, err := Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AuthEnabled() || cfg.Passphrase != "hunter2" {
		t.Errorf("Passphrase = %q, AuthEnabled = %v", cfg.Passphrase, cfg.AuthEnabled())
	}
	if _, err := Load([]string{"-passphrase", "x"}, os.NewFile(0, os.DevNull)); err == nil {
		t.Error("a -passphrase flag must not exist")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"missing root", map[string]string{"REPOMAN_ROOT": filepath.Join(root, "nope")}, nil},
		{"root is a file", map[string]string{"REPOMAN_ROOT": file}, nil},
		{"empty addr", map[string]string{"REPOMAN_ROOT": root}, []string{"-addr", ""}},
		{"zero scan interval", map[string]string{"REPOMAN_ROOT": root}, []string{"-scan-interval", "0"}},
		{"negative fetch interval", map[string]string{"REPOMAN_ROOT": root}, []string{"-fetch-interval", "-1s"}},
		{"zero fetch concurrency", map[string]string{"REPOMAN_ROOT": root}, []string{"-fetch-concurrency", "0"}},
		{"zero max depth", map[string]string{"REPOMAN_ROOT": root}, []string{"-max-depth", "0"}},
		{"bad owner", map[string]string{"REPOMAN_ROOT": root}, []string{"-owners", "a/b/c"}},
		{"stray positional argument", map[string]string{"REPOMAN_ROOT": root}, []string{"extra"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if cfg, err := Load(tc.args, os.NewFile(0, os.DevNull)); err == nil {
				t.Fatalf("Load succeeded with %+v", cfg)
			}
		})
	}
}

func TestZeroFetchIntervalIsAllowed(t *testing.T) {
	// Zero is the documented way to disable background fetching entirely.
	root := t.TempDir()
	t.Setenv("REPOMAN_ROOT", root)
	cfg, err := Load([]string{"-fetch-interval", "0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FetchInterval != 0 {
		t.Errorf("FetchInterval = %v, want 0", cfg.FetchInterval)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
	if got, want := expandHome("~/git"), filepath.Join(home, "git"); got != want {
		t.Errorf("expandHome(~/git) = %q, want %q", got, want)
	}
	// A tilde that is not a home reference must be left alone.
	if got := expandHome("~user/git"); got != "~user/git" {
		t.Errorf("expandHome(~user/git) = %q, want it untouched", got)
	}
}
