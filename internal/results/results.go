// Package results collects the outcome of the tasks executed during a single
// Task run. It is a plain data sink: recording results never writes anything
// to the log, so enabling it cannot change the existing log format.
package results

import (
	"sort"
	"sync"
)

type (
	// Status is the terminal (or current) state of a task execution.
	Status string

	// CacheSource describes why a task did not have to run its commands.
	CacheSource string

	// DepSummary is the state of one of a task's declared dependencies at
	// the time the task itself finished.
	DepSummary struct {
		// Name is the canonical (namespaced) name of the dependency.
		Name string
		// Status is the terminal status the dependency reached.
		Status Status
	}

	// TaskResult is the outcome of one task in a run. A task that is invoked
	// more than once has a single record whose Attempts/Retries fields count
	// every invocation.
	TaskResult struct {
		// Name is the display name of the task (label aware).
		Name string
		// Status is the current or terminal status.
		Status Status
		// ExitCode is the exit code of the failing command, 0 otherwise.
		ExitCode int
		// Attempts is how many times the task was entered during the run.
		Attempts int
		// Retries is the number of invocations beyond the first one, i.e.
		// Attempts minus one.
		Retries int
		// CacheSource is the cache the task was served from when its commands
		// did not have to run, CacheSourceNone otherwise.
		CacheSource CacheSource
		// Deps summarizes the declared dependencies of the task.
		Deps []DepSummary
		// Error holds the error message of the last attempt, if it failed.
		Error string
	}

	// Origin identifies the Taskfile node the run was loaded from.
	Origin struct {
		// Source is the node kind: file, http, git, stdin or cache.
		Source string
		// Location is the (redacted) location of the node.
		Location string
		// Checksum is the checksum the node was verified against, if any.
		Checksum string
	}

	// Report is an immutable snapshot of a run's results.
	Report struct {
		// Origin is the Taskfile the run was loaded from.
		Origin Origin
		// Tasks are the task results, sorted by name.
		Tasks []TaskResult
	}
)

const (
	// StatusRunning means the task has been entered and has not finished yet.
	StatusRunning Status = "running"
	// StatusSkipped means the task was not run because of a platform or if
	// condition.
	StatusSkipped Status = "skipped"
	// StatusUpToDate means the task was served from its fingerprint/status
	// cache.
	StatusUpToDate Status = "up-to-date"
	// StatusSucceeded means the task ran (or had nothing to do) successfully.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the task (or one of its commands/dependencies)
	// returned an error.
	StatusFailed Status = "failed"
	// StatusCanceled means the task did not finish because its context was
	// canceled or the user aborted it.
	StatusCanceled Status = "canceled"
)

const (
	// CacheSourceNone means the task ran its commands (or was skipped), so no
	// result cache was used.
	CacheSourceNone CacheSource = "none"
	// CacheSourceChecksum means the task was skipped because its sources'
	// checksum matched the stored one.
	CacheSourceChecksum CacheSource = "checksum"
	// CacheSourceTimestamp means the task was skipped because its sources
	// were older than its generated files.
	CacheSourceTimestamp CacheSource = "timestamp"
	// CacheSourceStatus means the task was skipped because its status
	// commands reported it as up to date.
	CacheSourceStatus CacheSource = "status"
)

// Attempt tracks a single RunTask invocation while it is in flight. Its
// setter methods are only called by the owning goroutine before Finish.
type Attempt struct {
	name     string
	display  string
	override Status      // set when the task did not actually run
	cache    CacheSource // cache source for this attempt
	deps     []string    // canonical dependency names in declaration order
}

// SetDisplay overrides the displayed task name (e.g. with its label).
func (a *Attempt) SetDisplay(name string) {
	if name != "" {
		a.display = name
	}
}

// SetDeps records the declared dependency names before they run, so they are
// summarized even if the task fails or is canceled while they run.
func (a *Attempt) SetDeps(names []string) {
	a.deps = names
}

// MarkSkipped records that the task did not run because of a platform or if
// condition.
func (a *Attempt) MarkSkipped() {
	a.override = StatusSkipped
}

// MarkCacheHit records that the task was served from the given cache source.
func (a *Attempt) MarkCacheHit(source CacheSource) {
	a.override = StatusUpToDate
	if source == "" {
		source = CacheSourceNone
	}
	a.cache = source
}

// Recorder is the concurrency-safe store of task results for one run. The
// zero value is not usable; use NewRecorder.
type Recorder struct {
	mu     sync.Mutex
	origin Origin
	order  []string
	tasks  map[string]*TaskResult
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		tasks: make(map[string]*TaskResult),
	}
}

// Reset empties the recorder and records the Taskfile origin of the new run.
func (r *Recorder) Reset(origin Origin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.origin = origin
	r.order = nil
	r.tasks = make(map[string]*TaskResult)
}

// SetOrigin records where the Taskfile was loaded from.
func (r *Recorder) SetOrigin(origin Origin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.origin = origin
}

// Begin registers the start of an invocation of the named task and returns an
// Attempt the caller marks and finishes. It is safe for concurrent use: a
// task invoked in parallel shares a single record and every Begin increments
// Attempts.
func (r *Recorder) Begin(name string) *Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, ok := r.tasks[name]
	if !ok {
		result = &TaskResult{
			Name:        name,
			Status:      StatusRunning,
			CacheSource: CacheSourceNone,
		}
		r.tasks[name] = result
		r.order = append(r.order, name)
	}
	result.Attempts++
	// A new invocation is running until Finish writes its terminal state.
	result.Status = StatusRunning
	result.CacheSource = CacheSourceNone

	return &Attempt{
		name:    name,
		display: name,
		cache:   CacheSourceNone,
	}
}

// Finish writes the terminal state of an attempt. status and exitCode are the
// values derived from the attempt's error; a status override set on the
// attempt (skipped/cache hit) takes precedence and forces a zero exit code.
// It must be called exactly once per Begin so that canceled and failed runs
// leave a complete terminal record.
func (r *Recorder) Finish(a *Attempt, status Status, exitCode int, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, ok := r.tasks[a.name]
	if !ok {
		// Defensive: Begin always creates the record, but never drop a
		// terminal state.
		result = &TaskResult{Name: a.display, Attempts: 1}
		r.tasks[a.name] = result
		r.order = append(r.order, a.name)
	}

	if a.display != "" {
		result.Name = a.display
	}
	if a.override != "" {
		result.Status = a.override
		result.ExitCode = 0
	} else {
		result.Status = status
		result.ExitCode = exitCode
	}
	result.CacheSource = a.cache
	result.Retries = max(result.Attempts-1, 0)

	deps := make([]DepSummary, 0, len(a.deps))
	for _, name := range a.deps {
		depStatus := StatusRunning
		if dep, ok := r.tasks[name]; ok {
			depStatus = dep.Status
		}
		deps = append(deps, DepSummary{Name: name, Status: depStatus})
	}
	result.Deps = deps

	if cause != nil {
		result.Error = cause.Error()
	} else {
		result.Error = ""
	}
}

// Get returns a snapshot of the named task's result.
func (r *Recorder) Get(name string) (TaskResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, ok := r.tasks[name]
	if !ok {
		return TaskResult{}, false
	}
	return clone(result), true
}

// Report returns a snapshot of every recorded task, sorted by name.
func (r *Recorder) Report() Report {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.order))
	seen := make(map[string]bool, len(r.order))
	for _, name := range r.order {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)

	tasks := make([]TaskResult, 0, len(names))
	for _, name := range names {
		tasks = append(tasks, clone(r.tasks[name]))
	}

	return Report{Origin: r.origin, Tasks: tasks}
}

func clone(r *TaskResult) TaskResult {
	cp := *r
	if r.Deps != nil {
		cp.Deps = append([]DepSummary(nil), r.Deps...)
	}
	return cp
}
