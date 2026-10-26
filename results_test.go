package task_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
)

const resultsTestDir = "testdata/results"

func newResultsExecutor(t *testing.T, opts ...task.ExecutorOption) *task.Executor {
	t.Helper()
	var out bytes.Buffer
	allOpts := append([]task.ExecutorOption{
		task.WithDir(resultsTestDir),
		task.WithStdout(&out),
		task.WithStderr(&out),
		task.WithSilent(true),
	}, opts...)
	e := task.NewExecutor(allOpts...)
	require.NoError(t, e.Setup())
	return e
}

func requireTaskResult(t *testing.T, e *task.Executor, name string) task.TaskResult {
	t.Helper()
	result, ok := e.TaskResult(name)
	require.Truef(t, ok, "missing result for task %q", name)
	return result
}

func requireDepStatus(t *testing.T, result task.TaskResult, name string) task.ResultStatus {
	t.Helper()
	for _, dep := range result.Deps {
		if dep.Name == name {
			return dep.Status
		}
	}
	t.Fatalf("dependency %q missing from result of %q: %+v", name, result.Name, result.Deps)
	return ""
}

// removeIfExists deletes the given file if it is present, ignoring a missing
// file so tests start from a clean slate on first run.
func removeIfExists(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		require.NoError(t, err)
	}
}

func TestTaskResultsParallelDeps(t *testing.T) {
	t.Parallel()

	e := newResultsExecutor(t)
	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "default"}))

	for _, name := range []string{"default", "dep-a", "dep-b", "dep-c"} {
		result := requireTaskResult(t, e, name)
		assert.Equalf(t, task.ResultStatusSucceeded, result.Status, "task %s", name)
		assert.Equalf(t, 0, result.ExitCode, "task %s", name)
		assert.Equalf(t, 1, result.Attempts, "task %s", name)
		assert.Equalf(t, 0, result.Retries, "task %s", name)
		assert.Equalf(t, task.ResultCacheNone, result.CacheSource, "task %s", name)
		assert.Emptyf(t, result.Error, "task %s", name)
	}

	// Dependencies run in parallel and their terminal states must be
	// summarized on the parent.
	parent := requireTaskResult(t, e, "default")
	assert.Equal(t, []task.DepSummary{
		{Name: "dep-a", Status: task.ResultStatusSucceeded},
		{Name: "dep-b", Status: task.ResultStatusSucceeded},
	}, parent.Deps)

	depB := requireTaskResult(t, e, "dep-b")
	assert.Equal(t, []task.DepSummary{
		{Name: "dep-c", Status: task.ResultStatusSucceeded},
	}, depB.Deps)

	// The report carries the Taskfile node origin and sorted tasks.
	report := e.ResultReport()
	assert.Equal(t, "file", report.Origin.Source)
	assert.Contains(t, report.Origin.Location, "Taskfile.yml")
	for i := 1; i < len(report.Tasks); i++ {
		assert.LessOrEqual(t, report.Tasks[i-1].Name, report.Tasks[i].Name)
	}
}

func TestTaskResultsCommandFailure(t *testing.T) {
	t.Parallel()

	t.Run("failed command records status and exit code", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.Error(t, e.Run(t.Context(), &task.Call{Task: "fail"}))

		result := requireTaskResult(t, e, "fail")
		assert.Equal(t, task.ResultStatusFailed, result.Status)
		assert.Equal(t, 7, result.ExitCode)
		assert.Equal(t, 1, result.Attempts)
		assert.Equal(t, 0, result.Retries)
		assert.Equal(t, task.ResultCacheNone, result.CacheSource)
		assert.NotEmpty(t, result.Error)
	})

	t.Run("failed dependency fails parent with complete terminal state", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.Error(t, e.Run(t.Context(), &task.Call{Task: "fail-parent"}))

		dep := requireTaskResult(t, e, "fail")
		assert.Equal(t, task.ResultStatusFailed, dep.Status)
		assert.Equal(t, 7, dep.ExitCode)

		parent := requireTaskResult(t, e, "fail-parent")
		assert.Equal(t, task.ResultStatusFailed, parent.Status)
		assert.Equal(t, 7, parent.ExitCode)
		assert.Equal(t, task.ResultStatusFailed, requireDepStatus(t, parent, "fail"))
	})

	t.Run("ignored command error keeps success", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.NoError(t, e.Run(t.Context(), &task.Call{Task: "ignored"}))

		result := requireTaskResult(t, e, "ignored")
		assert.Equal(t, task.ResultStatusSucceeded, result.Status)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("unmet if condition is skipped", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.NoError(t, e.Run(t.Context(), &task.Call{Task: "if-skipped"}))

		result := requireTaskResult(t, e, "if-skipped")
		assert.Equal(t, task.ResultStatusSkipped, result.Status)
		assert.Equal(t, 0, result.ExitCode)
	})
}

func TestTaskResultsCacheHit(t *testing.T) {
	t.Parallel()

	t.Run("checksum cache", func(t *testing.T) {
		t.Parallel()

		generated := filepath.Join(resultsTestDir, "generated.txt")
		removeIfExists(t, generated)
		t.Cleanup(func() { _ = os.Remove(generated) })

		// Both executors share the fingerprint directory.
		tempDir := task.TempDir{Remote: t.TempDir(), Fingerprint: t.TempDir()}

		first := newResultsExecutor(t, task.WithTempDir(tempDir))
		require.NoError(t, first.Run(t.Context(), &task.Call{Task: "cached"}))

		firstResult := requireTaskResult(t, first, "cached")
		assert.Equal(t, task.ResultStatusSucceeded, firstResult.Status)
		assert.Equal(t, task.ResultCacheNone, firstResult.CacheSource)
		_, statErr := os.Stat(generated)
		require.NoError(t, statErr)

		second := newResultsExecutor(t, task.WithTempDir(tempDir))
		require.NoError(t, second.Run(t.Context(), &task.Call{Task: "cached"}))

		secondResult := requireTaskResult(t, second, "cached")
		assert.Equal(t, task.ResultStatusUpToDate, secondResult.Status)
		assert.Equal(t, task.ResultCacheChecksum, secondResult.CacheSource)
		assert.Equal(t, 0, secondResult.ExitCode)
		assert.Equal(t, 1, secondResult.Attempts)
	})

	t.Run("status cache", func(t *testing.T) {
		t.Parallel()

		marker := filepath.Join(resultsTestDir, "marker.txt")
		removeIfExists(t, marker)
		t.Cleanup(func() { _ = os.Remove(marker) })

		first := newResultsExecutor(t)
		require.NoError(t, first.Run(t.Context(), &task.Call{Task: "status-hit"}))
		firstResult := requireTaskResult(t, first, "status-hit")
		assert.Equal(t, task.ResultStatusSucceeded, firstResult.Status)
		assert.Equal(t, task.ResultCacheNone, firstResult.CacheSource)

		second := newResultsExecutor(t)
		require.NoError(t, second.Run(t.Context(), &task.Call{Task: "status-hit"}))
		secondResult := requireTaskResult(t, second, "status-hit")
		assert.Equal(t, task.ResultStatusUpToDate, secondResult.Status)
		assert.Equal(t, task.ResultCacheStatus, secondResult.CacheSource)
	})
}

func TestTaskResultsRepeatedExecution(t *testing.T) {
	t.Parallel()

	t.Run("two sequential calls count attempts and retries", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.NoError(t, e.Run(t.Context(),
			&task.Call{Task: "repeated"},
			&task.Call{Task: "repeated"},
		))

		result := requireTaskResult(t, e, "repeated")
		assert.Equal(t, task.ResultStatusSucceeded, result.Status)
		assert.Equal(t, 2, result.Attempts)
		assert.Equal(t, 1, result.Retries)
	})

	t.Run("shared dependency runs for each parallel parent", func(t *testing.T) {
		t.Parallel()

		e := newResultsExecutor(t)
		require.NoError(t, e.Run(t.Context(), &task.Call{Task: "shared-parent"}))

		result := requireTaskResult(t, e, "shared")
		assert.Equal(t, task.ResultStatusSucceeded, result.Status)
		assert.Equal(t, 2, result.Attempts)
		assert.Equal(t, 1, result.Retries)
	})

	t.Run("run once executes a single time despite repeated invocations", func(t *testing.T) {
		t.Parallel()

		countFile := filepath.Join(resultsTestDir, "once-count.txt")
		removeIfExists(t, countFile)
		t.Cleanup(func() { _ = os.Remove(countFile) })

		e := newResultsExecutor(t)
		require.NoError(t, e.Run(t.Context(), &task.Call{Task: "once-parent"}))

		data, err := os.ReadFile(countFile)
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(strings.TrimSpace(string(data)), "called"),
			"shared-once must execute exactly once, got: %q", string(data))

		result := requireTaskResult(t, e, "shared-once")
		assert.Equal(t, task.ResultStatusSucceeded, result.Status)
		assert.Equal(t, 2, result.Attempts)
		assert.Equal(t, 1, result.Retries)
	})
}

func TestTaskResultsCanceledWritesTerminalState(t *testing.T) {
	t.Parallel()

	e := newResultsExecutor(t)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	require.Error(t, e.Run(ctx, &task.Call{Task: "slow"}))

	result := requireTaskResult(t, e, "slow")
	assert.NotEqual(t, task.ResultStatusRunning, result.Status)
	assert.Equal(t, task.ResultStatusCanceled, result.Status)
	assert.Equal(t, 130, result.ExitCode)
	assert.NotEmpty(t, result.Error)
}

func TestTaskResultsQueryAndPrintDoNotChangeLogFormat(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	e := task.NewExecutor(
		task.WithDir(resultsTestDir),
		task.WithStdout(&out),
		task.WithStderr(&out),
		task.WithSilent(true),
	)
	require.NoError(t, e.Setup())
	require.NoError(t, e.Run(t.Context(), &task.Call{Task: "dep-a"}))

	// Querying results writes nothing and leaves the run output untouched.
	outputBefore := out.String()
	assert.NotContains(t, outputBefore, "Run result summary")
	_ = e.TaskResults()
	_, _ = e.TaskResult("dep-a")
	_ = e.ResultReport()
	assert.Equal(t, outputBefore, out.String())

	// Printing is explicit, additive and clearly separated from task logs.
	e.PrintTaskResults()
	outputAfter := out.String()
	assert.Contains(t, outputAfter, "Run result summary:")
	assert.Contains(t, outputAfter, "dep-a")
	assert.Contains(t, outputAfter, "succeeded")
	assert.Contains(t, outputAfter, "taskfile:")
}
