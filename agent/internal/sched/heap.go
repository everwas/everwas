package sched

import "time"

// fireItem is one pending run. base is the schedule's own fire time and is
// what the job id is built from; at is base plus this agent's deterministic
// jitter, which is when we actually run.
type fireItem struct {
	entryID string
	base    time.Time
	at      time.Time
}

// fireHeap is a min-heap on at. It implements heap.Interface.
type fireHeap []fireItem

func (h fireHeap) Len() int           { return len(h) }
func (h fireHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h fireHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fireHeap) Push(x any)        { *h = append(*h, x.(fireItem)) }
func (h *fireHeap) Pop() any          { old := *h; n := len(old); it := old[n-1]; *h = old[:n-1]; return it }
func (h fireHeap) peek() (fireItem, bool) {
	if len(h) == 0 {
		return fireItem{}, false
	}
	return h[0], true
}
