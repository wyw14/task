//go:build watch

package task_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
	"github.com/go-task/task/v3/internal/filepathext"
)

// joinPath joins path elements for test fixture files.
func joinPath(parts ...string) string {
	return filepath.Join(parts...)
}

func TestFileWatch(t *testing.T) {
	t.Parallel()

	const dir = "testdata/watch"
	_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
	_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))

	expectedOutput := strings.TrimSpace(`
task: Started watching for tasks: default
task: [default] echo "Task running!"
Task running!
task: task "default" finished running
task: [default] echo "Task running!"
Task running!
task: task "default" finished running
	`)

	var buff bytes.Buffer
	e := task.NewExecutor(
		task.WithDir(dir),
		task.WithStdout(&buff),
		task.WithStderr(&buff),
		task.WithWatch(true),
	)

	require.NoError(t, e.Setup())
	buff.Reset()

	dirPath := filepathext.SmartJoin(dir, "src")
	filePath := filepathext.SmartJoin(dirPath, "a")

	err := os.MkdirAll(dirPath, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filePath, []byte("test"), 0o644)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				err := e.Run(ctx, &task.Call{Task: "default"})
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	// Keep generous margins over the 100ms debounce: the watcher runs in
	// several goroutines and the test binary may be under heavy load.
	time.Sleep(500 * time.Millisecond)
	err = os.WriteFile(filePath, []byte("test updated"), 0o644)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)
	cancel()
	assert.Equal(t, expectedOutput, strings.TrimSpace(buff.String()))
}

func TestShouldIgnore(t *testing.T) {
	t.Parallel()

	tt := []struct {
		path   string
		expect bool
	}{
		{"/.git/hooks", true},
		{"/.github/workflows/build.yaml", false},
	}

	for k, ct := range tt {
		ct := ct
		t.Run(fmt.Sprintf("ignore - %d", k), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, task.ShouldIgnore(ct.path), ct.expect)
		})
	}
}

// safeBuffer is a goroutine-safe buffer used to capture watcher output, which
// is produced from the event loop and one goroutine per watched task.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func requireContainsEventually(t *testing.T, buff *safeBuffer, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return strings.Contains(buff.String(), want)
	}, 10*time.Second, 25*time.Millisecond,
		"expected watch output to contain %q\nactual output:\n%s", want, buff.String())
}

// startWatchDiagnosticsExecutor starts a watching executor with event
// attribution diagnostics enabled and waits until it is ready to receive
// events. The watcher keeps running for the lifetime of the test, like in
// TestFileWatch.
func startWatchDiagnosticsExecutor(t *testing.T, dir string, interval time.Duration, calls ...*task.Call) *safeBuffer {
	t.Helper()

	buff := &safeBuffer{}
	e := task.NewExecutor(
		task.WithDir(dir),
		task.WithStdout(buff),
		task.WithStderr(buff),
		task.WithWatch(true),
		task.WithWatchDiagnostics(true),
		task.WithInterval(interval),
	)
	require.NoError(t, e.Setup())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Run(ctx, calls...); err != nil {
			panic(err)
		}
	}()
	// Stop the watcher and wait for it to release checksum files before
	// later tests remove the test directories (registered earlier, so this
	// cleanup runs first).
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		// Give in-flight runs cancelled alongside the watcher time to close
		// their checksum handles.
		time.Sleep(100 * time.Millisecond)
	})

	requireContainsEventually(t, buff, "task: Started watching")
	return buff
}

// resetWatchDiagDir removes watch artifacts from a previous run and creates
// the given sub directories with an empty seed file so the watcher
// registration finds something to watch.
func resetWatchDiagDir(t *testing.T, dir string, subDirs ...string) {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepathext.SmartJoin(dir, ".task")))
	for _, sub := range subDirs {
		subPath := filepathext.SmartJoin(dir, sub)
		require.NoError(t, os.RemoveAll(subPath))
		require.NoError(t, os.MkdirAll(subPath, 0o755))
		seed := filepathext.SmartJoin(subPath, "seed.txt")
		require.NoError(t, os.WriteFile(seed, []byte("seed"), 0o644))
	}
}

func TestWatchDiagnosticsCreateEvent(t *testing.T) {
	const dir = "testdata/watch_diagnostics"
	resetWatchDiagDir(t, dir, "src")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))
	})

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(400 * time.Millisecond)

	filePath := joinPath(dir, "src", "create.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("created"), 0o644))

	requireContainsEventually(t, buff, `event CREATE "src/create.txt"`)
	requireContainsEventually(t, buff, `"src/create.txt" matches task sources`)
	requireContainsEventually(t, buff, `task "default": rerun triggered`)
}

func TestWatchDiagnosticsDeleteEvent(t *testing.T) {
	const dir = "testdata/watch_diagnostics"
	resetWatchDiagDir(t, dir, "src")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))
	})
	require.NoError(t, os.WriteFile(joinPath(dir, "src", "delete.txt"), []byte("bye"), 0o644))

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(400 * time.Millisecond)

	require.NoError(t, os.Remove(joinPath(dir, "src", "delete.txt")))

	requireContainsEventually(t, buff, `event REMOVE "src/delete.txt"`)
	requireContainsEventually(t, buff, `REMOVE event "src/delete.txt" bypasses the sources check`)
	requireContainsEventually(t, buff, `task "default": rerun triggered`)
}

func TestWatchDiagnosticsRenameEvent(t *testing.T) {
	const dir = "testdata/watch_diagnostics"
	resetWatchDiagDir(t, dir, "src")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))
	})
	oldPath := joinPath(dir, "src", "rename.txt")
	require.NoError(t, os.WriteFile(oldPath, []byte("old"), 0o644))

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(400 * time.Millisecond)

	require.NoError(t, os.Rename(oldPath, joinPath(dir, "src", "rename2.txt")))

	// The old name no longer matches any sources, so it cannot rerun the task.
	requireContainsEventually(t, buff, `event RENAME "src/rename.txt"`)
	requireContainsEventually(t, buff, `"src/rename.txt" is not in sources of any watched task; no rerun`)
	// The new name shows up as a creation that matches the sources and reruns.
	requireContainsEventually(t, buff, `"src/rename2.txt" matches task sources`)
	requireContainsEventually(t, buff, `task "default": rerun triggered`)
}

func TestWatchDiagnosticsBatchEvents(t *testing.T) {
	const dir = "testdata/watch_diagnostics"
	resetWatchDiagDir(t, dir, "src")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))
	})

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(400 * time.Millisecond)

	names := []string{"batch1.txt", "batch2.txt", "batch3.txt", "batch4.txt"}
	for _, name := range names {
		require.NoError(t, os.WriteFile(joinPath(dir, "src", name), []byte(name), 0o644))
	}

	// Every distinct file must be attributed separately instead of being
	// folded into a single event.
	for _, name := range names {
		requireContainsEventually(t, buff, fmt.Sprintf(`event CREATE "src/%s"`, name))
		requireContainsEventually(t, buff, fmt.Sprintf(`"src/%s" matches task sources`, name))
	}
	require.Eventually(t, func() bool {
		return strings.Count(buff.String(), "rerun triggered") >= len(names)
	}, 10*time.Second, 25*time.Millisecond,
		"expected at least %d reruns for the batch\nactual output:\n%s", len(names), buff.String())
}

func TestWatchDiagnosticsMergedEvents(t *testing.T) {
	const dir = "testdata/watch_diagnostics"
	resetWatchDiagDir(t, dir, "src")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "src"))
	})

	buff := startWatchDiagnosticsExecutor(t, dir, 300*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(400 * time.Millisecond)

	mergePath := joinPath(dir, "src", "merge.txt")
	require.NoError(t, os.WriteFile(mergePath, []byte("v0"), 0o644))
	// Let the CREATE event be delivered on its own before starting bursts.
	time.Sleep(600 * time.Millisecond)

	// Fire repeated writes in bursts, waiting out the debounce window between
	// rounds until a merged delivery is reported.
	for range 6 {
		for range 10 {
			require.NoError(t, os.WriteFile(mergePath, []byte("updated"), 0o644))
		}
		time.Sleep(600 * time.Millisecond)
		if strings.Contains(buff.String(), `"src/merge.txt": merged with`) {
			break
		}
	}
	require.Contains(t, buff.String(), `"src/merge.txt": merged with`)
}

func TestWatchDiagnosticsIgnoredDirectory(t *testing.T) {
	const dir = "testdata/watch_ignored"
	require.NoError(t, os.RemoveAll(filepathext.SmartJoin(dir, ".task")))
	require.NoError(t, os.RemoveAll(filepathext.SmartJoin(dir, ".git")))
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".git"))
	})

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond, &task.Call{Task: "default"})
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, os.MkdirAll(filepathext.SmartJoin(dir, ".git"), 0o755))

	requireContainsEventually(t, buff, `".git" filtered by ignored directory; no rerun`)
	// Give any (wrongful) rerun time to be logged before asserting it never
	// happened.
	time.Sleep(400 * time.Millisecond)
	require.NotContains(t, buff.String(), "rerun triggered")
}

func TestWatchDiagnosticsMultipleWatchTasks(t *testing.T) {
	const dir = "testdata/watch_multi"
	resetWatchDiagDir(t, dir, "a", "b")
	t.Cleanup(func() {
		_ = os.RemoveAll(filepathext.SmartJoin(dir, ".task"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "a"))
		_ = os.RemoveAll(filepathext.SmartJoin(dir, "b"))
	})

	buff := startWatchDiagnosticsExecutor(t, dir, 100*time.Millisecond,
		&task.Call{Task: "task-a"}, &task.Call{Task: "task-b"})
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, os.WriteFile(joinPath(dir, "a", "changed.txt"), []byte("a"), 0o644))

	// task-a owns the changed source, task-b only sees it through the combined
	// sources of all watched tasks (which is what currently drives reruns).
	requireContainsEventually(t, buff, `"a/changed.txt" matches task sources`)
	requireContainsEventually(t, buff, `"a/changed.txt" is outside this task's sources but matches the combined sources of watched tasks`)
	requireContainsEventually(t, buff, `task "task-a": rerun triggered`)
	requireContainsEventually(t, buff, `task "task-b": rerun triggered`)
}
