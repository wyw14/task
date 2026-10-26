package fsnotifyext

import (
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWaitTime = 25 * time.Millisecond

func newTestDeduper(t *testing.T) (chan<- fsnotify.Event, <-chan Event) {
	t.Helper()
	raw := make(chan fsnotify.Event, 16)
	deduper := newDeduper(raw, testWaitTime)
	return raw, deduper.GetChan()
}

func sendEvent(t *testing.T, ch chan<- fsnotify.Event, event fsnotify.Event) {
	t.Helper()
	select {
	case ch <- event:
	case <-time.After(time.Second):
		t.Fatalf("timed out sending event: %v", event)
	}
}

func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deduplicated event")
		return Event{}
	}
}

func assertNoMoreEvents(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event := <-ch:
		t.Fatalf("unexpected extra event: %v (merged=%d)", event.Event, event.Merged)
	case <-time.After(3 * testWaitTime):
	}
}

func TestDeduperCreateEvent(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)
	sendEvent(t, raw, fsnotify.Event{Name: "src/a", Op: fsnotify.Create})

	event := recvEvent(t, deduped)
	assert.Equal(t, fsnotify.Create, event.Op)
	assert.Equal(t, "src/a", event.Name)
	assert.Zero(t, event.Merged, "a single create event must not be reported as merged")
	assertNoMoreEvents(t, deduped)
}

func TestDeduperDeleteEvent(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)
	sendEvent(t, raw, fsnotify.Event{Name: "src/a", Op: fsnotify.Remove})

	event := recvEvent(t, deduped)
	assert.Equal(t, fsnotify.Remove, event.Op)
	assert.Zero(t, event.Merged)
	assertNoMoreEvents(t, deduped)
}

func TestDeduperRenameEvent(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)
	sendEvent(t, raw, fsnotify.Event{Name: "src/old", Op: fsnotify.Rename})
	sendEvent(t, raw, fsnotify.Event{Name: "src/new", Op: fsnotify.Create})

	// Distinct keys can be delivered in any order when their timers elapse
	// at roughly the same time, so collect both before asserting.
	got := make(map[string]fsnotify.Op)
	for range 2 {
		event := recvEvent(t, deduped)
		assert.Zero(t, event.Merged)
		got[event.Name] = event.Op
	}
	assert.Equal(t, fsnotify.Rename, got["src/old"])
	assert.Equal(t, fsnotify.Create, got["src/new"])

	assertNoMoreEvents(t, deduped)
}

func TestDeduperMergesDuplicateEvents(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)

	// A burst of writes on the same file within the debounce window must be
	// delivered as a single event with the number of merged raw events.
	write := fsnotify.Event{Name: "src/a", Op: fsnotify.Write}
	for range 4 {
		sendEvent(t, raw, write)
	}

	event := recvEvent(t, deduped)
	assert.Equal(t, fsnotify.Write, event.Op)
	assert.Equal(t, "src/a", event.Name)
	assert.Equal(t, 3, event.Merged, "4 raw events should be 1 delivery + 3 merges")
	assertNoMoreEvents(t, deduped)

	// After delivery, a new burst on the same key starts counting from zero.
	time.Sleep(testWaitTime)
	for range 2 {
		sendEvent(t, raw, write)
	}
	event = recvEvent(t, deduped)
	assert.Equal(t, 1, event.Merged, "the merge counter must reset after delivery")
	assertNoMoreEvents(t, deduped)
}

func TestDeduperBatchDistinctEvents(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)

	names := []string{"src/a", "src/b", "src/c", "src/d"}
	for _, name := range names {
		sendEvent(t, raw, fsnotify.Event{Name: name, Op: fsnotify.Create})
	}

	seen := make(map[string]int)
	for range names {
		event := recvEvent(t, deduped)
		require.Equal(t, fsnotify.Create, event.Op)
		assert.Zero(t, event.Merged, "distinct events must not be merged together")
		seen[event.Name]++
	}

	for _, name := range names {
		assert.Equal(t, 1, seen[name], "expected exactly one delivery for %s", name)
	}
	assertNoMoreEvents(t, deduped)
}

func TestDeduperSkipsChmod(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)

	sendEvent(t, raw, fsnotify.Event{Name: "src/a", Op: fsnotify.Chmod})
	sendEvent(t, raw, fsnotify.Event{Name: "src/a", Op: fsnotify.Create})

	event := recvEvent(t, deduped)
	assert.Equal(t, fsnotify.Create, event.Op, "chmod events must be skipped")
	assertNoMoreEvents(t, deduped)
}

func TestDeduperClosesOnChannelClose(t *testing.T) {
	t.Parallel()

	raw, deduped := newTestDeduper(t)
	sendEvent(t, raw, fsnotify.Event{Name: "src/a", Op: fsnotify.Write})
	_ = recvEvent(t, deduped)

	close(raw)
	// Once the source channel is closed nothing else must be delivered.
	assertNoMoreEvents(t, deduped)
}
