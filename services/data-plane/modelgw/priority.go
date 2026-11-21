package modelgw

import (
	"local-agent-stack/pkg/a2a"
)

// PriorityGate wraps the shared a2a.PriorityScheduler (contract §2.4 rule 2).
//
// a2a.PriorityScheduler uses 3 bounded queues (capacity 64) and a weighted
// 3:2:1 rotation high:normal:low so low-priority requests never starve.
// X-Priority is parsed by a2a.ParsePriority: high / normal(default) / low.
//
// The scheduler runs jobs on its worker goroutines; the chat/embeddings
// handler submits its upstream call as a job and waits on a done channel.
// That means under saturation, high-priority work is dequeued first (E17).
type PriorityGate struct {
	s *a2a.PriorityScheduler
}

// NewPriorityGate builds a gate with the given workers (defaults to 4).
func NewPriorityGate(workers int) *PriorityGate {
	return &PriorityGate{s: a2a.NewPriorityScheduler(64, workers)}
}

// Run schedules fn at the priority derived from X-Priority and blocks until
// the scheduler has executed it. Returns false only if the queue is full.
func (g *PriorityGate) Run(priority a2a.Priority, fn func()) bool {
	done := make(chan struct{})
	job := func() {
		defer close(done)
		fn()
	}
	if err := g.s.Submit(priority, job); err != nil {
		return false
	}
	<-done
	return true
}

// Close stops the scheduler workers.
func (g *PriorityGate) Close() { g.s.Close() }
