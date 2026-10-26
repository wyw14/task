package task

import (
	"context"

	"mvdan.cc/sh/v3/interp"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/results"
	"github.com/go-task/task/v3/taskfile/ast"
)

// Re-exported result types so the result summary can be consumed without
// importing an internal package.
type (
	// TaskResult is the outcome of one task in a run.
	TaskResult = results.TaskResult
	// DepSummary is the state of one of a task's dependencies.
	DepSummary = results.DepSummary
	// ResultReport is a snapshot of every task result in a run.
	ResultReport = results.Report
	// ResultStatus is the status of a task in a run.
	ResultStatus = results.Status
	// ResultCacheSource is the cache a task result was served from.
	ResultCacheSource = results.CacheSource
)

// Re-exported statuses.
const (
	ResultStatusRunning   = results.StatusRunning
	ResultStatusSkipped   = results.StatusSkipped
	ResultStatusUpToDate  = results.StatusUpToDate
	ResultStatusSucceeded = results.StatusSucceeded
	ResultStatusFailed    = results.StatusFailed
	ResultStatusCanceled  = results.StatusCanceled
)

// Re-exported cache sources.
const (
	ResultCacheNone      = results.CacheSourceNone
	ResultCacheChecksum  = results.CacheSourceChecksum
	ResultCacheTimestamp = results.CacheSourceTimestamp
	ResultCacheStatus    = results.CacheSourceStatus
)

// exitCodeCanceled follows the shell convention for a process stopped by
// SIGINT (128 + signal 2).
const exitCodeCanceled = 130

// TaskResults returns a snapshot (sorted by name) of the results of every
// task in the last run. Reading the results is a data operation and never
// writes to the log.
func (e *Executor) TaskResults() []TaskResult {
	return e.results.Report().Tasks
}

// TaskResult returns the current or terminal result of the named task.
func (e *Executor) TaskResult(name string) (TaskResult, bool) {
	return e.results.Get(name)
}

// ResultReport returns the full result report of the last run, including the
// Taskfile origin and every task result.
func (e *Executor) ResultReport() ResultReport {
	return e.results.Report()
}

// PrintTaskResults writes the opt-in run result summary using the logger. It
// does not share any output path with regular task logging, so it cannot
// change the existing log format.
func (e *Executor) PrintTaskResults() {
	results.PrintReport(e.Logger, e.results.Report())
}

// recordTaskResult opens and immediately closes an attempt for a task that
// could not even start running (for example its Taskfile failed to compile).
// It guarantees such failures also leave a complete terminal record.
func (e *Executor) recordTaskResult(name string, ctx context.Context, err error) {
	attempt := e.results.Begin(name)
	status, exitCode := classifyTaskOutcome(err, ctx)
	e.results.Finish(attempt, status, exitCode, err)
}

// classifyTaskOutcome derives the terminal status and exit code of an attempt
// from its returned error. A nil error means the task succeeded.
func classifyTaskOutcome(err error, ctx context.Context) (results.Status, int) {
	if err == nil {
		return results.StatusSucceeded, 0
	}

	// A declared timeout is the command's own outcome, even though it
	// cancels its context.
	if _, ok := errors.AsType[*errors.TaskTimeoutError](err); ok {
		return results.StatusFailed, errors.TimeoutExitCode
	}

	// Aborted by the user at a prompt.
	var canceledByUser *errors.TaskCancelledByUserError
	if errors.As(err, &canceledByUser) {
		return results.StatusCanceled, errors.CodeTaskCancelled
	}
	var canceledWithoutTerminal *errors.TaskCancelledNoTerminalError
	if errors.As(err, &canceledWithoutTerminal) {
		return results.StatusCanceled, errors.CodeTaskCancelled
	}

	// Canceled through its context (signal, fail-fast sibling or parent).
	// This precedes the exit-status check because killing the running command
	// may report its signal exit status (or a generic failure on Windows)
	// instead of context.Canceled.
	if errors.Is(err, context.Canceled) ||
		ctx != nil && (ctx.Err() == context.Canceled || errors.Is(context.Cause(ctx), context.Canceled)) {
		return results.StatusCanceled, exitCodeCanceled
	}

	// The concrete exit status of the failed command.
	if exitCode, ok := errors.AsType[interp.ExitStatus](err); ok {
		return results.StatusFailed, int(exitCode)
	}

	// Any other known Task error keeps its dedicated code.
	var taskErr errors.TaskError
	if errors.As(err, &taskErr) {
		return results.StatusFailed, taskErr.Code()
	}

	return results.StatusFailed, 1
}

// cacheSourceOf reports which cache an up-to-date task was served from.
func cacheSourceOf(e *Executor, t *ast.Task) results.CacheSource {
	switch {
	case len(t.Sources) > 0:
		switch e.fingerprinter().Kind(t) {
		case "timestamp":
			return results.CacheSourceTimestamp
		case "none":
			return results.CacheSourceNone
		default:
			return results.CacheSourceChecksum
		}
	case len(t.Status) > 0:
		return results.CacheSourceStatus
	default:
		return results.CacheSourceNone
	}
}

// dependencyNames returns the canonical names of a task's declared
// dependencies, deduplicated in declaration order.
func dependencyNames(t *ast.Task) []string {
	seen := make(map[string]struct{}, len(t.Deps))
	names := make([]string, 0, len(t.Deps))
	for _, dep := range t.Deps {
		if dep == nil {
			continue
		}
		if _, ok := seen[dep.Task]; ok {
			continue
		}
		seen[dep.Task] = struct{}{}
		names = append(names, dep.Task)
	}
	return names
}
