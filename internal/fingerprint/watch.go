package fingerprint

import (
	"slices"

	"github.com/go-task/task/v3/taskfile/ast"
)

// WatchEventAttribution explains how a single filesystem watch event relates
// to a task. Building it is side-effect free: it expands source globs the same
// way fingerprinting does but never runs status commands nor stores
// fingerprints, so it cannot change whether or when the task actually runs.
type WatchEventAttribution struct {
	// Method is the fingerprint method that would be used for the task:
	// "checksum", "timestamp" or "none".
	Method string
	// Sources is the number of source globs declared by the task.
	Sources int
	// Status is the number of status commands declared by the task. The
	// commands themselves are only evaluated later by the status checker
	// during the rerun.
	Status int
	// MatchesSource reports whether the event path is part of the expanded
	// sources of the task.
	MatchesSource bool
}

// AttributeWatchEvent expands the task sources with the same glob and
// gitignore rules used for fingerprinting and reports whether eventPath (in
// slash-separated form) matches one of them. The final up-to-date decision
// still belongs to [Fingerprinter.UpToDate], which combines the sources
// fingerprint with the status commands.
func (f *Fingerprinter) AttributeWatchEvent(t *ast.Task, eventPath string) (*WatchEventAttribution, error) {
	files, err := Globs(t.Dir, t.Sources, t.ShouldUseGitignore())
	if err != nil {
		return nil, err
	}

	return &WatchEventAttribution{
		Method:        f.resolveMethod(t),
		Sources:       len(t.Sources),
		Status:        len(t.Status),
		MatchesSource: slices.Contains(files, eventPath),
	}, nil
}
