package main

import (
	"context"
	"sync"
)

// steerCap bounds the steer buffer and the next-turn queue: enough for a burst
// of typed corrections, small enough that a stuck script cannot grow it.
const steerCap = 32

// steerItem is one message injected into a running turn between rounds.
type steerItem struct {
	text string
	plan bool
}

// steerQueue is the hand-off between the UI thread (push) and the turn
// goroutine (drain at each round boundary). A mutex is enough: pushes and
// drains are tiny and infrequent.
type steerQueue struct {
	mu    sync.Mutex
	items []steerItem
}

func (q *steerQueue) push(it steerItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= steerCap {
		return false
	}
	q.items = append(q.items, it)
	return true
}

// drain hands over everything queued so far. Items pushed after this call
// stay for the next round boundary.
func (q *steerQueue) drain() []steerItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	out := q.items
	q.items = nil
	return out
}

// steerQueueKey carries the active turn's buffer through the turn context.
// Only runModelTurn drains it; the transports never look at it.
type steerQueueKey struct{}

func withSteerQueue(ctx context.Context, q *steerQueue) context.Context {
	return context.WithValue(ctx, steerQueueKey{}, q)
}

func drainSteers(ctx context.Context) []steerItem {
	q, _ := ctx.Value(steerQueueKey{}).(*steerQueue)
	if q == nil {
		return nil
	}
	return q.drain()
}
