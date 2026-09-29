package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Job states.
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// ErrBusy is returned when another long-running operation is in progress.
var ErrBusy = errors.New("another operation is still running")

const maxJobLog = 4 << 20

// Job is a long-running operation (scan, plan, apply) whose output is
// streamed to the UI.
type Job struct {
	ID         string
	Kind       string
	Cancelable bool

	mu       sync.Mutex
	state    string
	started  time.Time
	finished time.Time
	err      string
	log      []byte
	dropped  int
	cancel   context.CancelFunc
	result   any
}

// JobView is a snapshot of a job for the UI.
type JobView struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	Cancelable bool      `json:"cancelable"`
	Log        string    `json:"log"`
	NextOffset int       `json:"next_offset"`
	Result     any       `json:"result,omitempty"`
}

// Write appends to the job log, keeping at most maxJobLog bytes.
func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, p...)
	if over := len(j.log) - maxJobLog; over > 0 {
		j.log = j.log[over:]
		j.dropped += over
	}
	return len(p), nil
}

// Printf writes a formatted line to the job log.
func (j *Job) Printf(format string, args ...any) {
	fmt.Fprintf(j, format+"\n", args...)
}

// View returns the job state and the log from the given offset.
func (j *Job) View(offset int) JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := offset - j.dropped
	if start < 0 {
		start = 0
	}
	if start > len(j.log) {
		start = len(j.log)
	}
	return JobView{
		ID: j.ID, Kind: j.Kind, State: j.state, StartedAt: j.started, FinishedAt: j.finished,
		Error: j.err, Cancelable: j.Cancelable && j.state == JobRunning,
		Log: string(j.log[start:]), NextOffset: j.dropped + len(j.log), Result: j.result,
	}
}

// State returns the current job state.
func (j *Job) State() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// Jobs runs one operation at a time.
type Jobs struct {
	mu     sync.Mutex
	base   context.Context
	jobs   map[string]*Job
	order  []string
	active *Job
	wg     sync.WaitGroup
}

// NewJobs creates a job runner whose jobs are cancelled when base is.
func NewJobs(base context.Context) *Jobs {
	return &Jobs{base: base, jobs: map[string]*Job{}}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start runs fn in the background. Only one job may run at a time.
func (js *Jobs) Start(kind string, cancelable bool, fn func(ctx context.Context, job *Job) (any, error)) (*Job, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.active != nil && js.active.State() == JobRunning {
		return nil, fmt.Errorf("%w (%s)", ErrBusy, js.active.Kind)
	}
	ctx, cancel := context.WithCancel(js.base)
	job := &Job{ID: newID(), Kind: kind, Cancelable: cancelable, state: JobRunning, started: time.Now().UTC(), cancel: cancel}
	js.jobs[job.ID] = job
	js.order = append(js.order, job.ID)
	if len(js.order) > 20 {
		delete(js.jobs, js.order[0])
		js.order = js.order[1:]
	}
	js.active = job
	js.wg.Add(1)
	go func() {
		defer js.wg.Done()
		defer cancel()
		result, err := safeRun(ctx, job, fn)
		job.mu.Lock()
		defer job.mu.Unlock()
		job.finished = time.Now().UTC()
		job.result = result
		switch {
		case err != nil && ctx.Err() != nil:
			job.state, job.err = JobCancelled, "cancelled"
		case err != nil:
			job.state, job.err = JobFailed, err.Error()
		default:
			job.state = JobSucceeded
		}
	}()
	return job, nil
}

func safeRun(ctx context.Context, job *Job, fn func(ctx context.Context, job *Job) (any, error)) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return fn(ctx, job)
}

// Get returns a job by ID.
func (js *Jobs) Get(id string) (*Job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.jobs[id]
	return j, ok
}

// Latest returns the most recent job.
func (js *Jobs) Latest() *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.active
}

// Cancel requests cancellation of a cancelable running job.
func (js *Jobs) Cancel(id string) error {
	j, ok := js.Get(id)
	if !ok {
		return errors.New("unknown job")
	}
	if !j.Cancelable {
		return errors.New("this operation cannot be cancelled safely")
	}
	j.cancel()
	return nil
}

// Wait blocks until all jobs have finished.
func (js *Jobs) Wait() { js.wg.Wait() }

// discard is an io.Writer for callers without a job.
var discard io.Writer = io.Discard
