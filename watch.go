package task

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/fingerprint"
	"github.com/go-task/task/v3/internal/fsnotifyext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/slicesext"
	"github.com/go-task/task/v3/taskfile/ast"
)

const defaultWaitTime = 100 * time.Millisecond

// watchTasks start watching the given tasks. It returns when the parent
// context is cancelled: the CLI passes a context that is only done when the
// process exits, while tests cancel it to stop watching.
func (e *Executor) watchTasks(parent context.Context, calls ...*Call) error {
	tasks := make([]string, len(calls))
	for i, c := range calls {
		tasks[i] = c.Task
	}

	e.Logger.Errf(logger.Green, "task: Started watching for tasks: %s\n", strings.Join(tasks, ", "))

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	for _, c := range calls {
		go func() {
			err := e.RunTask(ctx, c)
			if err == nil {
				e.Logger.Errf(logger.Green, "task: task \"%s\" finished running\n", c.Task)
			} else if !isContextError(err) {
				e.Logger.Errf(logger.Red, "%v\n", err)
			}
		}()
	}

	var waitTime time.Duration
	switch {
	case e.Interval != 0:
		waitTime = e.Interval
	case e.Taskfile.Interval != 0:
		waitTime = e.Taskfile.Interval
	default:
		waitTime = defaultWaitTime
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		cancel()
		return err
	}
	defer w.Close()

	deduper := fsnotifyext.NewDeduper(w, waitTime)
	eventsChan := deduper.GetChan()

	closeOnInterrupt(w)

	go func() {
		for {
			select {
			case event, ok := <-eventsChan:
				if !ok {
					cancel()
					return
				}
				e.Logger.VerboseErrf(logger.Magenta, "task: received watch event: %v\n", event.Event)
				e.logWatchEvent(event)

				cancel()
				// Derive the run context from parent so each event cancels
				// the previous run, while shutdown (process exit in the CLI,
				// test cleanup otherwise) cancels the in-flight run too.
				ctx, cancel = context.WithCancel(parent)

				e.Compiler.ResetCache()

				for _, c := range calls {
					go func() {
						if ShouldIgnore(event.Name) {
							e.Logger.VerboseErrf(logger.Magenta, "task: event skipped for being an ignored dir: %s\n", event.Name)
							e.logWatchIgnored(c.Task, event.Event)
							return
						}
						t, err := e.GetTask(c)
						if err != nil {
							e.Logger.Errf(logger.Red, "%v\n", err)
							return
						}
						baseDir := filepathext.SmartJoin(e.Dir, t.Dir)
						files, err := e.collectSources(calls)
						if err != nil {
							e.Logger.Errf(logger.Red, "%v\n", err)
							return
						}

						isRemove := event.Has(fsnotify.Remove)
						matchesSources := slices.Contains(files, filepath.ToSlash(event.Name))
						if !isRemove && !matchesSources {
							relPath, _ := filepath.Rel(baseDir, event.Name)
							e.Logger.VerboseErrf(logger.Magenta, "task: skipped for file not in sources: %s\n", relPath)
						}
						e.logWatchAttribution(c, t, baseDir, event.Event, isRemove, matchesSources)
						if !isRemove && !matchesSources {
							return
						}
						e.logWatchRerun(c.Task)
						err = e.RunTask(ctx, c)
						if err == nil {
							e.Logger.Errf(logger.Green, "task: task \"%s\" finished running\n", c.Task)
						} else if !isContextError(err) {
							e.Logger.Errf(logger.Red, "%v\n", err)
						}
					}()
				}
			case err, ok := <-w.Errors:
				switch {
				case !ok:
					cancel()
					return
				default:
					e.Logger.Errf(logger.Red, "%v\n", err)
				}
			}
		}
	}()

	e.watchedDirs = xsync.NewMap[string, bool]()

	go func() {
		// NOTE(@andreynering): New files can be created in directories
		// that were previously empty, so we need to check for new dirs
		// from time to time.
		for {
			if err := e.registerWatchedDirs(w, calls...); err != nil {
				e.Logger.Errf(logger.Red, "%v\n", err)
			}
			select {
			case <-parent.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	<-parent.Done()
	return nil
}

func isContextError(err error) bool {
	if taskRunErr, ok := err.(*errors.TaskRunError); ok {
		err = taskRunErr.Err
	}

	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func closeOnInterrupt(w *fsnotify.Watcher) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		w.Close()
		os.Exit(0)
	}()
}

func (e *Executor) registerWatchedDirs(w *fsnotify.Watcher, calls ...*Call) error {
	files, err := e.collectSources(calls)
	if err != nil {
		return err
	}
	for _, f := range files {
		d := filepath.Dir(f)
		if isSet, ok := e.watchedDirs.Load(d); ok && isSet {
			continue
		}
		if ShouldIgnore(d) {
			continue
		}
		if err := w.Add(d); err != nil {
			return err
		}
		e.watchedDirs.Store(d, true)
		relPath, _ := filepath.Rel(e.Dir, d)
		e.Logger.VerboseOutf(logger.Green, "task: watching new dir: %v\n", relPath)
	}
	return nil
}

var ignorePaths = []string{
	"/.task",
	"/.git",
	"/.hg",
	"/node_modules",
}

func ShouldIgnore(path string) bool {
	// Normalize separators so events reported with native separators
	// (e.g. backslashes on Windows) are filtered consistently.
	path = filepath.ToSlash(path)
	for _, p := range ignorePaths {
		if strings.Contains(path, fmt.Sprintf("%s/", p)) || strings.HasSuffix(path, p) {
			return true
		}
	}
	return false
}

func (e *Executor) collectSources(calls []*Call) ([]string, error) {
	var sources []string

	err := e.traverse(calls, func(task *ast.Task) error {
		files, err := fingerprint.Globs(task.Dir, task.Sources, task.ShouldUseGitignore())
		if err != nil {
			return err
		}
		sources = append(sources, files...)
		return nil
	})

	return slicesext.UniqueJoin(sources), err
}

type traverseFunc func(*ast.Task) error

func (e *Executor) traverse(calls []*Call, yield traverseFunc) error {
	for _, c := range calls {
		task, err := e.CompiledTask(c)
		if err != nil {
			return err
		}
		for _, dep := range task.Deps {
			if dep.Task != "" {
				if err := e.traverse([]*Call{{Task: dep.Task, Vars: dep.Vars}}, yield); err != nil {
					return err
				}
			}
		}
		for _, cmd := range task.Cmds {
			if cmd.Task != "" {
				if err := e.traverse([]*Call{{Task: cmd.Task, Vars: cmd.Vars}}, yield); err != nil {
					return err
				}
			}
		}
		if err := yield(task); err != nil {
			return err
		}
	}
	return nil
}
