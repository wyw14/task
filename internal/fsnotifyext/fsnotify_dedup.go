package fsnotifyext

import (
	"math"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Event is a deduplicated [fsnotify.Event] with attribution metadata. The
// metadata is diagnostic only: it never changes when an event is delivered.
type Event struct {
	fsnotify.Event

	// Merged is the number of raw events with the same name and operation
	// that were coalesced into this event while the debounce timer was
	// waiting. Zero means the event was delivered on its own.
	Merged int
}

type Deduper struct {
	events   <-chan fsnotify.Event
	waitTime time.Duration
}

func NewDeduper(w *fsnotify.Watcher, waitTime time.Duration) *Deduper {
	return newDeduper(w.Events, waitTime)
}

// newDeduper builds a Deduper from a raw event channel, which keeps the
// debounce logic testable without a real filesystem watcher.
func newDeduper(events <-chan fsnotify.Event, waitTime time.Duration) *Deduper {
	return &Deduper{
		events:   events,
		waitTime: waitTime,
	}
}

// GetChan returns a chan of deduplicated [Event]s.
//
// [fsnotify.Chmod] operations will be skipped.
func (d *Deduper) GetChan() <-chan Event {
	channel := make(chan Event)

	go func() {
		timers := make(map[string]*dedupTimer)
		for {
			event, ok := <-d.events
			switch {
			case !ok:
				return
			case event.Has(fsnotify.Chmod):
				continue
			}

			timer, ok := timers[event.String()]
			if !ok {
				timer = &dedupTimer{pending: true}
				timer.timer = time.AfterFunc(math.MaxInt64, func() {
					// Reset the merge state before delivering so a later
					// burst on the same key starts fresh. Only that state is
					// touched here; timer scheduling stays exactly the same.
					timer.mu.Lock()
					merged := timer.merged
					timer.merged = 0
					timer.pending = false
					timer.mu.Unlock()
					channel <- Event{Event: event, Merged: merged}
				})
				timer.timer.Stop()
				timers[event.String()] = timer
			} else {
				timer.mu.Lock()
				if timer.pending {
					timer.merged++
				} else {
					// The previous debounce window already elapsed, so this
					// event opens a new window and is not a merge.
					timer.merged = 0
					timer.pending = true
				}
				timer.mu.Unlock()
			}

			timer.timer.Reset(d.waitTime)
		}
	}()

	return channel
}

// dedupTimer pairs the debounce timer with the number of raw events that were
// folded into its pending delivery. pending reports whether a delivery is
// currently scheduled. The mutex only guards this state and is never held
// across channel sends or timer operations, so it cannot influence the
// debounce timing.
type dedupTimer struct {
	timer   *time.Timer
	mu      sync.Mutex
	merged  int
	pending bool
}
