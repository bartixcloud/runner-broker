package dispatcher

import (
	"context"
	"strings"
	"sync"
)

const defaultOrgParallel = 4

// orgScheduler bounds how many org plans run at once and queues plans for the same
// repository one behind the other in arrival order, so backup, detection, restic
// and mirror of one repo never interleave with another event's for that repo.
type orgScheduler struct {
	sem chan struct{}

	mu   sync.Mutex
	tail map[string]chan struct{} // key -> done channel of the newest queued plan
}

func newOrgScheduler(parallel int) *orgScheduler {
	if parallel < 1 {
		parallel = defaultOrgParallel
	}
	return &orgScheduler{sem: make(chan struct{}, parallel), tail: map[string]chan struct{}{}}
}

// orgKey serialises per repository; plans without one (the org-wide workflow, name
// problems) share one queue. GitHub repository names are case-insensitive.
func orgKey(p OrgPlan) string {
	if p.Repo == "" {
		return "-"
	}
	return strings.ToLower(p.Repo)
}

// enqueue fixes the caller's place in key's queue now (call it in arrival order, from
// the request handler) and returns the function that waits for that place, takes a
// parallelism slot and runs fn. It reports false, without running fn, when ctx ended
// first; the queue keeps moving either way.
func (s *orgScheduler) enqueue(key string) func(ctx context.Context, fn func()) bool {
	mine := make(chan struct{})
	s.mu.Lock()
	prev := s.tail[key]
	s.tail[key] = mine
	s.mu.Unlock()

	return func(ctx context.Context, fn func()) bool {
		defer func() {
			close(mine)
			s.mu.Lock()
			if s.tail[key] == mine {
				delete(s.tail, key)
			}
			s.mu.Unlock()
		}()
		if prev != nil {
			select {
			case <-prev:
			case <-ctx.Done():
				return false
			}
		}
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		defer func() { <-s.sem }()
		// Both selects above can be ready at once and pick either; a cancelled
		// context must always win, so a shut-down daemon starts no new plan.
		if ctx.Err() != nil {
			return false
		}
		fn()
		return true
	}
}
