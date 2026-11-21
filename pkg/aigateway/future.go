// Package aigateway builds on pkg/composite to implement a realistic AI-gateway
// plugin pipeline:
//
//	request: parse body -> extract model -> route by model -> start async INPUT guardrail
//	response: EncodeHeaders -> start async OUTPUT guardrail
//	          EncodeData -> WAIT for input+output results before releasing the chunk
//	          metrics + token accounting on completion
//
// The central primitive here is Future: a one-shot async result that a filter
// can start early and await later (in EncodeData), with timeout and fail-open /
// fail-closed policy. This is exactly the "async guardrail in encodeData" pattern.
package aigateway

import (
	"errors"
	"sync"
	"time"
)

// ErrFutureTimeout is returned when waiting for an async result exceeded its deadline.
var ErrFutureTimeout = errors.New("aigateway: future timeout")

// Future is a one-shot asynchronous result. Safe for one producer / many waiters.
type Future[T any] struct {
	mu      sync.Mutex
	done    chan struct{}
	value   T
	err     error
	ready   bool
	waiters int
}

// NewFuture creates a pending future.
func NewFuture[T any]() *Future[T] {
	return &Future[T]{done: make(chan struct{})}
}

// Resolve completes the future with a value.
func (f *Future[T]) Resolve(v T) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready {
		return
	}
	f.value = v
	f.ready = true
	close(f.done)
}

// Fail completes the future with an error.
func (f *Future[T]) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready {
		return
	}
	f.err = err
	f.ready = true
	close(f.done)
}

// Ready reports whether the future has completed (non-blocking).
func (f *Future[T]) Ready() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Get waits until the future completes, then returns (value, error).
func (f *Future[T]) Get() (T, error) {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.value, f.err
}

// Wait blocks until the future completes or the timeout elapses.
// On timeout it returns ErrFutureTimeout and the zero value.
func (f *Future[T]) Wait(timeout time.Duration) (T, error) {
	var zero T
	if timeout <= 0 {
		return f.Get()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.value, f.err
	case <-timer.C:
		return zero, ErrFutureTimeout
	}
}

// GuardVerdict is the result of an async guardrail check.
type GuardVerdict struct {
	Direction string // "input" | "output"
	Allowed   bool
	Code      string // e.g. GUARDRAIL_REJECTED
	Reason    string
	CheckedAt int64
}
