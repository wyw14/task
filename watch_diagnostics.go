package task

import (
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/go-task/task/v3/internal/fsnotifyext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

// Watch event attribution diagnostic lines. They all share the
// "task: watch:" prefix so the output can be filtered.
//
// Every method is a no-op unless [Executor.WatchDiagnostics] is explicitly
// enabled and only performs read-only work (glob expansion and logging).
// Therefore they can never influence the debounce window, the cancellation of
// a running task or the decision to rerun.
func (e *Executor) logWatchEvent(event fsnotifyext.Event) {
	if !e.WatchDiagnostics {
		return
	}
	path := watchRelativePath(e.Dir, event.Name)
	switch {
	case event.Merged > 0:
		e.Logger.Errf(logger.Cyan,
			"task: watch: event %s %q: merged with %d duplicate event(s) during debounce\n",
			event.Op.String(), path, event.Merged)
	default:
		e.Logger.Errf(logger.Cyan,
			"task: watch: event %s %q: not merged with any other event\n",
			event.Op.String(), path)
	}
}

// logWatchIgnored attributes an event that was filtered out because its path
// is inside an ignored directory (.task, .git, ...).
func (e *Executor) logWatchIgnored(taskName string, event fsnotify.Event) {
	if !e.WatchDiagnostics {
		return
	}
	e.Logger.Errf(logger.Cyan,
		"task: watch: task %q: event %s %q filtered by ignored directory; no rerun\n",
		taskName, event.Op.String(), watchRelativePath(e.Dir, event.Name))
}

// logWatchAttribution explains, for one watched task, whether an event reaches
// its sources and therefore reruns it. isRemove and matchesCombinedSources are
// the exact values used by the rerun decision in watchTasks, so the diagnostic
// can only describe that decision, never change it.
func (e *Executor) logWatchAttribution(
	c *Call,
	t *ast.Task,
	baseDir string,
	event fsnotify.Event,
	isRemove bool,
	matchesCombinedSources bool,
) {
	if !e.WatchDiagnostics {
		return
	}
	path := watchRelativePath(e.Dir, event.Name)

	// Expand globs against the resolved task directory on a shallow copy of
	// the raw task: this stays read-only and never invokes the compiler,
	// executes status commands nor runs the task.
	attributionTask := *t
	attributionTask.Dir = baseDir
	attr, err := e.fingerprinter().AttributeWatchEvent(&attributionTask, filepath.ToSlash(event.Name))
	if err != nil {
		e.Logger.Errf(logger.Red, "task: watch: task %q: failed to attribute event %q: %v\n", c.Task, path, err)
		return
	}

	const ctxFmt = "(fingerprint=%s, sources=%d, status=%d)"
	switch {
	case isRemove:
		e.Logger.Errf(logger.Cyan,
			"task: watch: task %q: REMOVE event %q bypasses the sources check "+ctxFmt+"\n",
			c.Task, path, attr.Method, attr.Sources, attr.Status)
	case attr.MatchesSource:
		e.Logger.Errf(logger.Cyan,
			"task: watch: task %q: %q matches task sources "+ctxFmt+"\n",
			c.Task, path, attr.Method, attr.Sources, attr.Status)
	case matchesCombinedSources:
		e.Logger.Errf(logger.Cyan,
			"task: watch: task %q: %q is outside this task's sources but matches the combined sources of watched tasks "+ctxFmt+"\n",
			c.Task, path, attr.Method, attr.Sources, attr.Status)
	default:
		e.Logger.Errf(logger.Cyan,
			"task: watch: task %q: %q is not in sources of any watched task; no rerun "+ctxFmt+"\n",
			c.Task, path, attr.Method, attr.Sources, attr.Status)
	}
}

// logWatchRerun marks the exact point where a task run is about to be
// triggered by a watch event.
func (e *Executor) logWatchRerun(taskName string) {
	if !e.WatchDiagnostics {
		return
	}
	e.Logger.Errf(logger.Cyan, "task: watch: task %q: rerun triggered\n", taskName)
}

// watchRelativePath renders event paths relative to the Taskfile directory so
// diagnostic lines stay comparable across machines.
func watchRelativePath(baseDir, name string) string {
	rel, err := filepath.Rel(baseDir, name)
	if err != nil {
		return filepath.ToSlash(name)
	}
	return filepath.ToSlash(rel)
}
