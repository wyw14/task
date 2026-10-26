package results

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorderCountsAttemptsAndRetries(t *testing.T) {
	t.Parallel()

	r := NewRecorder()

	first := r.Begin("build")
	r.Finish(first, StatusFailed, 2, errors.New("boom"))
	second := r.Begin("build")
	r.Finish(second, StatusSucceeded, 0, nil)

	result, ok := r.Get("build")
	require.True(t, ok)
	assert.Equal(t, StatusSucceeded, result.Status)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, 2, result.Attempts)
	assert.Equal(t, 1, result.Retries)
	assert.Empty(t, result.Error)
}

func TestRecorderSkipAndCacheOverrides(t *testing.T) {
	t.Parallel()

	r := NewRecorder()

	skipped := r.Begin("other-os")
	skipped.MarkSkipped()
	r.Finish(skipped, StatusFailed, 9, errors.New("must not leak"))
	skippedResult, ok := r.Get("other-os")
	require.True(t, ok)
	assert.Equal(t, StatusSkipped, skippedResult.Status)
	assert.Equal(t, 0, skippedResult.ExitCode)

	cached := r.Begin("cached")
	cached.MarkCacheHit(CacheSourceChecksum)
	r.Finish(cached, StatusFailed, 9, errors.New("must not leak"))
	cachedResult, ok := r.Get("cached")
	require.True(t, ok)
	assert.Equal(t, StatusUpToDate, cachedResult.Status)
	assert.Equal(t, CacheSourceChecksum, cachedResult.CacheSource)
	assert.Equal(t, 0, cachedResult.ExitCode)
}

func TestRecorderDependencySummary(t *testing.T) {
	t.Parallel()

	r := NewRecorder()

	lint := r.Begin("lint")
	r.Finish(lint, StatusSucceeded, 0, nil)
	test := r.Begin("test")
	r.Finish(test, StatusFailed, 7, errors.New("exit status 7"))

	build := r.Begin("build")
	build.SetDeps([]string{"lint", "test"})
	r.Finish(build, StatusFailed, 7, errors.New("exit status 7"))

	result, ok := r.Get("build")
	require.True(t, ok)
	assert.Equal(t, []DepSummary{
		{Name: "lint", Status: StatusSucceeded},
		{Name: "test", Status: StatusFailed},
	}, result.Deps)

	// Report snapshot is sorted by name.
	report := r.Report()
	require.Len(t, report.Tasks, 3)
	assert.Equal(t, "build", report.Tasks[0].Name)
	assert.Equal(t, "lint", report.Tasks[1].Name)
	assert.Equal(t, "test", report.Tasks[2].Name)
}

func TestRecorderConcurrentFinish(t *testing.T) {
	t.Parallel()

	r := NewRecorder()

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			attempt := r.Begin("parallel")
			r.Finish(attempt, StatusSucceeded, 0, nil)
		})
	}
	wg.Wait()

	result, ok := r.Get("parallel")
	require.True(t, ok)
	assert.Equal(t, 20, result.Attempts)
	assert.Equal(t, 19, result.Retries)
	assert.Equal(t, StatusSucceeded, result.Status)
}

func TestRecorderReset(t *testing.T) {
	t.Parallel()

	r := NewRecorder()
	attempt := r.Begin("build")
	r.Finish(attempt, StatusSucceeded, 0, nil)

	r.Reset(Origin{Source: "file", Location: "Taskfile.yml"})

	_, ok := r.Get("build")
	assert.False(t, ok)
	report := r.Report()
	assert.Empty(t, report.Tasks)
	assert.Equal(t, "file", report.Origin.Source)
	assert.Equal(t, "Taskfile.yml", report.Origin.Location)
}
