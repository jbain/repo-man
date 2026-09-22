// Package jobs tracks the service's only long-running operations: cloning a
// repository and initializing a new one.
//
// A clone of a large repository takes minutes, which is far too long to hold
// an HTTP request open, so the handler starts a job and the browser polls it.
// Job records are in-memory and expire; nothing is written to disk. Losing
// them on restart is acceptable — the clone either landed on disk or it did
// not, and the next scan reports the truth either way.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is a job's lifecycle position.
type State string

const (
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Job is an immutable snapshot of one operation. Callers always receive a copy.
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`   // "clone" or "init"
	Target   string    `json:"target"` // root-relative path being created
	Label    string    `json:"label"`  // human-readable description
	State    State     `json:"state"`
	Err      string    `json:"err,omitempty"`
	Output   string    `json:"output,omitempty"` // tail of the command's output
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
}

// Running reports whether the job is still in flight.
func (j Job) Running() bool { return j.State == StateRunning }

// ErrBusy is returned when a job is already running for the same target.
var ErrBusy = errors.New("an operation is already running for this path")

// maxOutput caps the retained output per job. Clone progress is verbose and
// almost entirely uninteresting; the tail is what explains a failure.
const maxOutput = 16 << 10

// Registry runs and remembers jobs.
type Registry struct {
	base    context.Context
	ttl     time.Duration
	maxJobs int
	onDone  func()

	mu   sync.RWMutex
	jobs map[string]*entry
	wg   sync.WaitGroup
}

type entry struct {
	mu  sync.Mutex
	job Job
	buf *tailBuffer
}

// Options configures a Registry.
type Options struct {
	// Base is the context every job inherits, so shutdown cancels in-flight
	// work. Job lifetime must not be tied to the HTTP request that started it.
	Base context.Context
	// TTL is how long a finished job stays readable. Zero uses 30 minutes.
	TTL time.Duration
	// MaxJobs caps retained records. Zero uses 100.
	MaxJobs int
	// OnDone, if set, is called after each job finishes — used to kick off an
	// immediate rescan so a fresh clone shows up without waiting for the timer.
	OnDone func()
}

// New returns a Registry.
func New(opts Options) *Registry {
	r := &Registry{
		base:    opts.Base,
		ttl:     opts.TTL,
		maxJobs: opts.MaxJobs,
		onDone:  opts.OnDone,
		jobs:    make(map[string]*entry),
	}
	if r.base == nil {
		r.base = context.Background()
	}
	if r.ttl <= 0 {
		r.ttl = 30 * time.Minute
	}
	if r.maxJobs <= 0 {
		r.maxJobs = 100
	}
	return r
}

// Start launches fn in a goroutine and returns the job record. fn writes its
// progress to the supplied writer. It fails with ErrBusy if another job for
// the same target is still running. A panic inside fn is recovered and
// recorded as a failed job, the same as an ordinary error return, rather than
// taking down the process.
func (r *Registry) Start(kind, target, label string, fn func(context.Context, *TailWriter) error) (Job, error) {
	r.mu.Lock()
	for _, e := range r.jobs {
		e.mu.Lock()
		busy := e.job.Running() && e.job.Target == target
		e.mu.Unlock()
		if busy {
			r.mu.Unlock()
			return Job{}, ErrBusy
		}
	}
	e := &entry{
		job: Job{
			ID:      newID(),
			Kind:    kind,
			Target:  target,
			Label:   label,
			State:   StateRunning,
			Started: time.Now(),
		},
		buf: &tailBuffer{limit: maxOutput},
	}
	r.jobs[e.job.ID] = e
	r.evictLocked()
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := runJob(r.base, fn, &TailWriter{buf: e.buf})

		e.mu.Lock()
		e.job.Finished = time.Now()
		e.job.Output = e.buf.String()
		if err != nil {
			e.job.State = StateFailed
			e.job.Err = err.Error()
		} else {
			e.job.State = StateDone
		}
		e.mu.Unlock()

		if r.onDone != nil {
			r.onDone()
		}
	}()

	return r.snapshot(e), nil
}

// runJob calls fn and recovers a panic from it, turning it into an error
// indistinguishable in kind from any other job failure. This is a
// single-process, single-tenant service: an unrecovered panic in fn (in
// practice git.Clone or git.Init, parsing subprocess output) would otherwise
// crash the whole server, taking every other in-flight job and live session
// down with it, instead of marking just this one job StateFailed.
func runJob(ctx context.Context, fn func(context.Context, *TailWriter) error, w *TailWriter) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(ctx, w)
}

// Get returns a job by ID.
func (r *Registry) Get(id string) (Job, bool) {
	r.mu.RLock()
	e, ok := r.jobs[id]
	r.mu.RUnlock()
	if !ok {
		return Job{}, false
	}
	return r.snapshot(e), true
}

// List returns every retained job, newest first.
func (r *Registry) List() []Job {
	r.mu.RLock()
	out := make([]Job, 0, len(r.jobs))
	for _, e := range r.jobs {
		out = append(out, r.snapshot(e))
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Active returns the in-flight job for a target, if any.
func (r *Registry) Active(target string) (Job, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.jobs {
		j := r.snapshot(e)
		if j.Running() && j.Target == target {
			return j, true
		}
	}
	return Job{}, false
}

// Wait blocks until all in-flight jobs finish. Used on shutdown so a clone is
// not abandoned half-written without at least being cancelled first.
func (r *Registry) Wait() { r.wg.Wait() }

// Run expires finished jobs until ctx is done.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(r.ttl / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			cutoff := time.Now().Add(-r.ttl)
			for id, e := range r.jobs {
				e.mu.Lock()
				expired := !e.job.Running() && e.job.Finished.Before(cutoff)
				e.mu.Unlock()
				if expired {
					delete(r.jobs, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *Registry) snapshot(e *entry) Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.job
	if j.Running() {
		// A running job's output lives in the buffer; copy the tail so the UI
		// can show progress before the job finishes.
		j.Output = e.buf.String()
	}
	return j
}

// evictLocked drops the oldest finished jobs once the map is over budget. It
// never evicts a running job: that record is the only handle the UI has on
// work still in flight.
func (r *Registry) evictLocked() {
	if len(r.jobs) <= r.maxJobs {
		return
	}
	type aged struct {
		id string
		at time.Time
	}
	var finished []aged
	for id, e := range r.jobs {
		e.mu.Lock()
		if !e.job.Running() {
			finished = append(finished, aged{id, e.job.Started})
		}
		e.mu.Unlock()
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].at.Before(finished[j].at) })
	for _, f := range finished {
		if len(r.jobs) <= r.maxJobs {
			return
		}
		delete(r.jobs, f.id)
	}
}

// TailWriter is the io.Writer handed to a job's function. It keeps only the
// last maxOutput bytes.
type TailWriter struct{ buf *tailBuffer }

func (w *TailWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	b     []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.limit; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

// String returns the retained output with git's in-place progress collapsed.
//
// git renders progress by rewriting a line with carriage returns:
// "Counting objects: 7%\rCounting objects: 8%\r...\rCounting objects: 100%, done."
// all arrives as a single line. Treating each \r as a line break — the obvious
// reading — turns one line of progress into hundreds, which then bury the
// message that actually explains a failure. Applying the real terminal
// semantic instead (a \r discards what was on the line before it) leaves only
// the text each line finally settled on.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	lines := strings.Split(string(t.b), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = line[i+1:]
		}
		if line = strings.TrimRight(line, " \t"); line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; a job ID is not
		// a security boundary here anyway (it is unguessable padding on an
		// already-authenticated endpoint), so fall back rather than panic.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
