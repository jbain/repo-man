package actions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jbain/repo-man/internal/jobs"
	"github.com/jbain/repo-man/internal/safepath"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// newRoot returns a symlink-resolved temp root, matching how config.Load
// canonicalizes the real one.
func newRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInitRepoCreatesARepository(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "github.com", "jbain"), 0o755); err != nil {
		t.Fatal(err)
	}

	var changed int
	svc := New(root, nil, func() { changed++ }, nil)

	rel, err := svc.InitRepo(context.Background(), "github.com/jbain", "new-thing", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/jbain/new-thing"; rel != want {
		t.Errorf("rel = %q, want %q", rel, want)
	}
	if fi, err := os.Stat(filepath.Join(root, rel, ".git")); err != nil || !fi.IsDir() {
		t.Errorf(".git = (%v, %v), want a directory", fi, err)
	}
	if changed != 1 {
		t.Errorf("onChange called %d times, want 1", changed)
	}
}

func TestInitRepoAtTheRoot(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	svc := New(root, nil, nil, nil)

	rel, err := svc.InitRepo(context.Background(), "", "scratch", "")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "scratch" {
		t.Errorf("rel = %q, want %q", rel, "scratch")
	}
}

func TestInitRepoRejections(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(root, nil, nil, nil)

	tests := []struct {
		name     string
		parent   string
		repoName string
		wantIs   error
	}{
		{"existing directory", "", "taken", ErrExists},
		{"missing parent", "nope", "x", os.ErrNotExist},
		{"parent escapes the root", "../..", "x", nil},
		{"name with a separator", "", "a/b", nil},
		{"name that is a parent reference", "", "..", nil},
		{"name that would become a git flag", "", "--bare", nil},
		{"empty name", "", "", nil},
		{"dot-prefixed name is invisible to the scanner", "", ".hidden", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.InitRepo(context.Background(), tc.parent, tc.repoName, "")
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want one wrapping %v", err, tc.wantIs)
			}
		})
	}
}

func TestInitRepoLeavesAnExistingDirectoryAlone(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	existing := filepath.Join(root, "taken")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(existing, "important.txt")
	if err := os.WriteFile(payload, []byte("do not clobber"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := New(root, nil, nil, nil)
	if _, err := svc.InitRepo(context.Background(), "", "taken", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if b, err := os.ReadFile(payload); err != nil || string(b) != "do not clobber" {
		t.Errorf("existing content = (%q, %v), want it untouched", b, err)
	}
	if _, err := os.Stat(filepath.Join(existing, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a repository must not have been initialized in the existing directory")
	}
}

// TestInitRepoRejectsAConcurrentCallForTheSameDestination exercises the guard
// deterministically: with a claim already held (simulating a first InitRepo
// call still in flight), a second call for the same destination must be
// rejected immediately rather than racing git.Init against the first, and
// must succeed again once the claim is released.
func TestInitRepoRejectsAConcurrentCallForTheSameDestination(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	svc := New(root, nil, nil, nil)

	abs, _, err := safepath.Resolve(root, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.beginInit(abs) {
		t.Fatal("beginInit should succeed when nothing else holds the claim")
	}

	if _, err := svc.InitRepo(context.Background(), "", "concurrent", ""); !errors.Is(err, jobs.ErrBusy) {
		t.Fatalf("InitRepo while another call holds the claim = %v, want ErrBusy", err)
	}
	if _, err := os.Stat(abs); !errors.Is(err, os.ErrNotExist) {
		t.Error("a rejected concurrent InitRepo must not have touched the filesystem")
	}

	svc.endInit(abs)

	if rel, err := svc.InitRepo(context.Background(), "", "concurrent", ""); err != nil || rel != "concurrent" {
		t.Fatalf("InitRepo after the claim was released = (%q, %v), want it to succeed", rel, err)
	}
}

// TestInitRepoConcurrentCallsRaceToExactlyOneWinner fires real concurrent
// InitRepo calls at the same destination and asserts exactly one succeeds.
// Before the concurrency guard, both could pass the "does it already exist"
// check before either had created anything and both would call git.Init
// against the same directory.
func TestInitRepoConcurrentCallsRaceToExactlyOneWinner(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	svc := New(root, nil, nil, nil)

	const n = 8
	start := make(chan struct{})
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = svc.InitRepo(context.Background(), "", "race-target", "")
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, jobs.ErrBusy) && !errors.Is(err, ErrExists) {
			t.Errorf("loser error = %v, want ErrBusy or ErrExists", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 of %d concurrent InitRepo calls to win", successes, n)
	}
	if _, err := os.Stat(filepath.Join(root, "race-target", ".git")); err != nil {
		t.Errorf(".git missing after the race: %v", err)
	}
}

func TestDestFor(t *testing.T) {
	tests := []struct {
		url   string
		want  string
		isErr bool
	}{
		{url: "https://github.com/jbain/repo-man.git", want: "github.com/jbain/repo-man"},
		{url: "https://github.com/jbain/repo-man", want: "github.com/jbain/repo-man"},
		{url: "git@github.com:jbain/repo-man.git", want: "github.com/jbain/repo-man"},
		{url: "ssh://git@github.com/jbain/repo-man", want: "github.com/jbain/repo-man"},
		{url: "https://gitlab.com/group/subgroup/thing.git", want: "gitlab.com/group/subgroup/thing"},
		{url: "not a url", isErr: true},
		{url: "", isErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			got, err := DestFor(tc.url)
			if tc.isErr {
				if err == nil {
					t.Fatalf("DestFor(%q) = %q, want an error", tc.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DestFor(%q) returned %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("DestFor(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestCloneValidatesBeforeStartingAnyWork(t *testing.T) {
	root := newRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "github.com", "jbain", "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := jobs.New(jobs.Options{Base: context.Background()})
	svc := New(root, reg, nil, nil)

	tests := []struct {
		name   string
		req    CloneRequest
		wantIs error
	}{
		{"empty url", CloneRequest{}, nil},
		{"url that is a git option", CloneRequest{URL: "--upload-pack=touch /tmp/pwn"}, nil},
		{"ext transport executes commands", CloneRequest{URL: "ext::sh -c 'touch /tmp/pwn'"}, nil},
		{"destination escapes the root", CloneRequest{URL: "https://github.com/a/b", Dest: "../../etc/evil"}, safepath.ErrOutsideRoot},
		{"destination already exists", CloneRequest{URL: "https://github.com/jbain/taken"}, ErrExists},
		{"refuses to clone into the root itself", CloneRequest{URL: "https://github.com/a/b", Dest: "/"}, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, err := svc.Clone(tc.req)
			if err == nil {
				t.Fatalf("Clone succeeded with job %+v", job)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want one wrapping %v", err, tc.wantIs)
			}
			if len(reg.List()) != 0 {
				t.Fatal("a rejected clone must not have started a job")
			}
		})
	}
}

// TestCloneStartsAJobAndReportsFailure exercises the full path from an
// accepted request through to a finished job record.
//
// It deliberately cannot assert a *successful* clone: ValidateCloneURL rejects
// local paths and file:// URLs, so there is no way to stand up an origin the
// test could reach without real network. Cloning from a working remote is
// covered by the git package's own tests, which call git.Clone directly. What
// matters here is the orchestration — that a valid request starts a job, that
// the job completes, and that a transport failure is reported rather than
// swallowed. 127.0.0.1:1 refuses instantly, so this needs no network and does
// not depend on DNS.
func TestCloneStartsAJobAndReportsFailure(t *testing.T) {
	requireGit(t)
	root := newRoot(t)

	done := make(chan struct{}, 1)
	reg := jobs.New(jobs.Options{Base: context.Background(), OnDone: func() { done <- struct{}{} }})
	svc := New(root, reg, nil, nil)

	job, err := svc.Clone(CloneRequest{URL: "https://127.0.0.1:1/jbain/thing.git", Dest: "local/mirror"})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.StateRunning || job.Target != "local/mirror" || job.Kind != "clone" {
		t.Fatalf("job = %+v, want a running clone targeting local/mirror", job)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("clone job never finished")
	}

	final, ok := reg.Get(job.ID)
	if !ok {
		t.Fatal("job disappeared from the registry")
	}
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %q, want %q", final.State, jobs.StateFailed)
	}
	if final.Err == "" {
		t.Error("a failed job must carry an error message for the UI to show")
	}
}

func TestCloneDerivesTheConventionalDestination(t *testing.T) {
	requireGit(t)
	root := newRoot(t)
	done := make(chan struct{}, 1)
	reg := jobs.New(jobs.Options{Base: context.Background(), OnDone: func() { done <- struct{}{} }})
	svc := New(root, reg, nil, nil)

	job, err := svc.Clone(CloneRequest{URL: "https://127.0.0.1:1/jbain/thing.git"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.1/jbain/thing"; job.Target != want {
		t.Errorf("target = %q, want %q derived from the URL", job.Target, want)
	}

	// The job only had to be *started* for the assertion above, but it is
	// still writing into the temp root; returning here leaves t.TempDir's
	// cleanup racing a live git process, which fails the test intermittently
	// with "directory not empty".
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("clone job never finished")
	}
}

func TestCloneWithoutARegistryFails(t *testing.T) {
	root := newRoot(t)
	svc := New(root, nil, nil, nil)
	if _, err := svc.Clone(CloneRequest{URL: "https://github.com/a/b"}); err == nil {
		t.Fatal("expected an error when no job registry is configured")
	}
}
