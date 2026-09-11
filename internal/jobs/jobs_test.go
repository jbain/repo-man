package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRegistry returns a registry whose jobs never outlive the test.
func newRegistry(t *testing.T, opts Options) *Registry {
	t.Helper()
	if opts.Base == nil {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		opts.Base = ctx
	}
	r := New(opts)
	t.Cleanup(r.Wait)
	return r
}

func TestStartRunsTheFunctionAndRecordsSuccess(t *testing.T) {
	done := make(chan struct{})
	r := newRegistry(t, Options{OnDone: func() { close(done) }})

	job, err := r.Start("init", "a/b", "make a/b", func(ctx context.Context, w *TailWriter) error {
		w.Write([]byte("working\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateRunning || !job.Running() {
		t.Fatalf("new job = %+v, want running", job)
	}

	<-done
	final, ok := r.Get(job.ID)
	if !ok {
		t.Fatal("job missing")
	}
	if final.State != StateDone || final.Err != "" {
		t.Fatalf("final = %+v, want a clean done", final)
	}
	if final.Output != "working" {
		t.Errorf("output = %q, want %q", final.Output, "working")
	}
	if final.Finished.IsZero() || final.Finished.Before(final.Started) {
		t.Errorf("finished = %v, started = %v", final.Finished, final.Started)
	}
}

func TestStartRecordsFailure(t *testing.T) {
	done := make(chan struct{})
	r := newRegistry(t, Options{OnDone: func() { close(done) }})

	job, _ := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		w.Write([]byte("fatal: nope\n"))
		return errors.New("exit status 128")
	})
	<-done

	final, _ := r.Get(job.ID)
	if final.State != StateFailed {
		t.Fatalf("state = %q, want %q", final.State, StateFailed)
	}
	if final.Err != "exit status 128" {
		t.Errorf("err = %q", final.Err)
	}
	if !strings.Contains(final.Output, "fatal: nope") {
		t.Errorf("output = %q, want it to retain the failure detail", final.Output)
	}
}

func TestStartRecoversFromAPanicAndRecordsFailure(t *testing.T) {
	done := make(chan struct{}, 2)
	r := newRegistry(t, Options{OnDone: func() { done <- struct{}{} }})

	job, err := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		w.Write([]byte("about to blow up\n"))
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done

	final, ok := r.Get(job.ID)
	if !ok {
		t.Fatal("job missing")
	}
	if final.State != StateFailed {
		t.Fatalf("state = %q, want %q", final.State, StateFailed)
	}
	if !strings.Contains(final.Err, "panic") || !strings.Contains(final.Err, "boom") {
		t.Errorf("err = %q, want it to mention the panic value", final.Err)
	}
	if final.Finished.IsZero() {
		t.Error("a recovered panic must still finalize the job record")
	}

	// The registry itself, and the process, must survive: a second job for a
	// different target still runs normally.
	job2, err := r.Start("clone", "c/d", "", func(ctx context.Context, w *TailWriter) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-done
	waitFinished(t, r, job2.ID)
}

func TestStartRejectsASecondJobForTheSameTarget(t *testing.T) {
	r := newRegistry(t, Options{})
	release := make(chan struct{})
	started := make(chan struct{})

	if _, err := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	if _, err := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("second job for the same target = %v, want ErrBusy", err)
	}
	// A different target is unaffected.
	if _, err := r.Start("clone", "other", "", func(ctx context.Context, w *TailWriter) error { return nil }); err != nil {
		t.Fatalf("job for a different target = %v, want it to start", err)
	}
	close(release)
}

func TestActive(t *testing.T) {
	r := newRegistry(t, Options{})
	release := make(chan struct{})
	started := make(chan struct{})
	r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		close(started)
		<-release
		return nil
	})
	<-started

	if _, ok := r.Active("a/b"); !ok {
		t.Error("Active should find the in-flight job")
	}
	if _, ok := r.Active("nothing"); ok {
		t.Error("Active should not report a job for an unrelated target")
	}
	close(release)
	r.Wait()
	if _, ok := r.Active("a/b"); ok {
		t.Error("Active should not report a finished job")
	}
}

func TestOutputVisibleWhileStillRunning(t *testing.T) {
	// The UI shows clone progress before the job ends, so a running job's
	// buffer must be readable.
	r := newRegistry(t, Options{})
	wrote := make(chan struct{})
	release := make(chan struct{})
	job, _ := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		w.Write([]byte("receiving objects: 42%"))
		close(wrote)
		<-release
		return nil
	})
	<-wrote

	live, _ := r.Get(job.ID)
	if !strings.Contains(live.Output, "42%") {
		t.Errorf("running job output = %q, want the in-progress text", live.Output)
	}
	close(release)
}

func TestOutputIsTruncatedToTheTail(t *testing.T) {
	r := newRegistry(t, Options{})
	done := make(chan struct{})
	job, _ := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
		defer close(done)
		w.Write([]byte(strings.Repeat("x", maxOutput)))
		w.Write([]byte("THE-END"))
		return nil
	})
	<-done
	r.Wait()

	final, _ := r.Get(job.ID)
	if len(final.Output) > maxOutput {
		t.Errorf("output length = %d, want at most %d", len(final.Output), maxOutput)
	}
	if !strings.HasSuffix(final.Output, "THE-END") {
		t.Error("truncation must keep the tail, which is where failures are explained")
	}
}

func TestInPlaceProgressCollapsesToItsFinalState(t *testing.T) {
	// git rewrites a progress line with \r. Keeping every revision would bury
	// the message that explains a failure under hundreds of percentages.
	tests := []struct {
		name  string
		write string
		want  string
	}{
		{
			"a progress line keeps only what it settled on",
			"Counting objects:   7% (16/225)        \rCounting objects: 100% (225/225), done.        \n",
			"Counting objects: 100% (225/225), done.",
		},
		{
			"each line collapses independently",
			"Counting: 1%\rCounting: done.\nResolving: 1%\rResolving: done.\n",
			"Counting: done.\nResolving: done.",
		},
		{
			"a failure after progress survives",
			"Receiving objects: 50%\rReceiving objects: 100%\nfatal: the remote end hung up\n",
			"Receiving objects: 100%\nfatal: the remote end hung up",
		},
		{
			"plain output is untouched",
			"line one\nline two\n",
			"line one\nline two",
		},
		{
			"blank lines are dropped",
			"a\n\n\nb\n",
			"a\nb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry(t, Options{})
			done := make(chan struct{})
			job, _ := r.Start("clone", "a/b", "", func(ctx context.Context, w *TailWriter) error {
				defer close(done)
				w.Write([]byte(tc.write))
				return nil
			})
			<-done
			r.Wait()

			final, _ := r.Get(job.ID)
			if strings.Contains(final.Output, "\r") {
				t.Errorf("output = %q, still contains a carriage return", final.Output)
			}
			if final.Output != tc.want {
				t.Errorf("output = %q, want %q", final.Output, tc.want)
			}
		})
	}
}

func TestBaseContextCancellationReachesRunningJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New(Options{Base: ctx})

	observed := make(chan error, 1)
	r.Start("clone", "a/b", "", func(jobCtx context.Context, w *TailWriter) error {
		<-jobCtx.Done()
		observed <- jobCtx.Err()
		return jobCtx.Err()
	})

	cancel()
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("job context error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the base context did not reach the running job")
	}
	r.Wait()
}

func TestListIsNewestFirst(t *testing.T) {
	r := newRegistry(t, Options{})
	for _, target := range []string{"first", "second", "third"} {
		r.Start("init", target, "", func(ctx context.Context, w *TailWriter) error { return nil })
		time.Sleep(2 * time.Millisecond) // distinguish the Started timestamps
	}
	r.Wait()

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("len(List()) = %d, want 3", len(list))
	}
	if list[0].Target != "third" || list[2].Target != "first" {
		t.Errorf("order = %q, %q, %q; want newest first", list[0].Target, list[1].Target, list[2].Target)
	}
}

func TestEvictionDropsOldFinishedJobsButNeverRunningOnes(t *testing.T) {
	r := newRegistry(t, Options{MaxJobs: 3})

	release := make(chan struct{})
	started := make(chan struct{})
	keep, _ := r.Start("clone", "long-running", "", func(ctx context.Context, w *TailWriter) error {
		close(started)
		<-release
		return nil
	})
	<-started

	for i := range 10 {
		job, err := r.Start("init", fmt.Sprintf("job-%d", i), "", func(ctx context.Context, w *TailWriter) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		// Each must reach a finished state before the next Start, otherwise
		// there is nothing eligible for eviction and the test proves nothing.
		waitFinished(t, r, job.ID)
	}

	if _, ok := r.Get(keep.ID); !ok {
		t.Error("the running job was evicted; the UI would lose its only handle on in-flight work")
	}
	if n := len(r.List()); n > 3 {
		t.Errorf("retained %d jobs, want at most MaxJobs (3)", n)
	}
	close(release)
}

// waitFinished blocks until the job leaves the running state. A job that has
// already been evicted counts as finished.
func waitFinished(t *testing.T, r *Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if j, ok := r.Get(id); !ok || !j.Running() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished", id)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunExpiresFinishedJobs(t *testing.T) {
	r := newRegistry(t, Options{TTL: 40 * time.Millisecond})
	job, _ := r.Start("init", "a/b", "", func(ctx context.Context, w *TailWriter) error { return nil })
	r.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		if _, ok := r.Get(job.ID); !ok {
			return // expired as expected
		}
		select {
		case <-deadline:
			t.Fatal("finished job was never expired")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	r := newRegistry(t, Options{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := string(rune('a' + i))
			job, err := r.Start("init", target, "", func(ctx context.Context, w *TailWriter) error {
				w.Write([]byte("x"))
				return nil
			})
			if err != nil {
				return
			}
			for range 20 {
				r.Get(job.ID)
				r.List()
				r.Active(target)
			}
		}()
	}
	wg.Wait()
}
