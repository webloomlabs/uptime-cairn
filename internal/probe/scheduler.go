package probe

import (
	"container/heap"
	"hash/fnv"
	"sync"
	"time"
)

// task is one due check.
type task struct {
	monitorID string
	due       time.Time

	// attempt is 1-based and counts retries within one logical check, which is
	// what heartbeats.attempt records.
	attempt uint32
}

// scheduler is one min-heap keyed on next-due, not a timer per monitor: 5,000
// timers is survivable in Go and the wrong shape at 50,000 (probe-plan §4.4).
type scheduler struct {
	mu    sync.Mutex
	queue taskHeap
	wake  chan struct{}
}

func newScheduler() *scheduler {
	return &scheduler{wake: make(chan struct{}, 1)}
}

func (s *scheduler) push(t task) {
	s.mu.Lock()
	heap.Push(&s.queue, t)
	s.mu.Unlock()
	s.signal()
}

// pop returns the earliest task if it is due, otherwise the time to wait.
func (s *scheduler) pop(now time.Time) (task, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.queue.Len() == 0 {
		return task{}, 0, false
	}
	if next := s.queue[0]; next.due.After(now) {
		return task{}, next.due.Sub(now), false
	}
	return heap.Pop(&s.queue).(task), 0, true
}

// drop removes every task for a monitor, which is what an unassignment means.
func (s *scheduler) drop(monitorID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.queue[:0]
	for _, t := range s.queue {
		if t.monitorID != monitorID {
			kept = append(kept, t)
		}
	}
	s.queue = kept
	heap.Init(&s.queue)
}

func (s *scheduler) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queue.Len()
}

// signal wakes the loop without blocking. The channel has depth one because a
// second wake-up tells the loop nothing the first did not.
func (s *scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type taskHeap []task

func (h taskHeap) Len() int           { return len(h) }
func (h taskHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h taskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *taskHeap) Push(x any)        { *h = append(*h, x.(task)) }
func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	*h = old[:n-1]
	return t
}

// firstDue disperses a monitor's start time deterministically:
// hash(monitor_id) mod interval.
//
// Without this, 5,000 monitors created by an importer on a 60-second interval
// all fire in the same second — a 5,000-wide burst every minute instead of 83
// checks a second, and the peak is what sizes the memory. Cheapest line in the
// design and the easiest to forget (probe-plan §4.4).
func firstDue(monitorID string, interval time.Duration, now time.Time) time.Time {
	h := fnv.New64a()
	_, _ = h.Write([]byte(monitorID))
	offset := time.Duration(h.Sum64() % uint64(interval))

	due := now.Truncate(interval).Add(offset)
	if !due.After(now) {
		due = due.Add(interval)
	}
	return due
}
