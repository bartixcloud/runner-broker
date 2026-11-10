package dispatcher

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRunsOneKeyInArrivalOrderNeverConcurrently(t *testing.T) {
	s := newOrgScheduler(4)
	var (
		mu      sync.Mutex
		order   []int
		running atomic.Int32
		peak    atomic.Int32
		wg      sync.WaitGroup
	)
	// enqueue is called in arrival order, as the handler does; the goroutines race.
	for i := range 8 {
		run := s.enqueue("repo")
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(t.Context(), func() {
				n := running.Add(1)
				if n > peak.Load() {
					peak.Store(n)
				}
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				running.Add(-1)
			})
		}()
	}
	wg.Wait()
	if !slices.Equal(order, []int{0, 1, 2, 3, 4, 5, 6, 7}) || peak.Load() != 1 {
		t.Errorf("order = %v, peak = %d, want arrival order and 1", order, peak.Load())
	}
}

func TestSchedulerBoundsParallelismAcrossKeys(t *testing.T) {
	s := newOrgScheduler(2)
	var running, peak atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		run := s.enqueue(key)
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(t.Context(), func() {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				<-release
				running.Add(-1)
			})
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if got := running.Load(); got != 2 {
		t.Errorf("%d jobs running at once, want exactly the limit 2", got)
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Errorf("peak = %d", peak.Load())
	}
}

func TestSchedulerDifferentKeysRunInParallel(t *testing.T) {
	s := newOrgScheduler(4)
	started := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b"} {
		run := s.enqueue(key)
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(t.Context(), func() { started <- key; <-release })
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("two repos did not run in parallel")
		}
	}
	close(release)
	wg.Wait()
}

func TestSchedulerDropsQueuedJobsWhenTheContextEndsButKeepsTheChainMoving(t *testing.T) {
	s := newOrgScheduler(1)
	ctx, cancel := context.WithCancel(t.Context())
	blocker := make(chan struct{})
	first := s.enqueue("r")
	second := s.enqueue("r")
	third := s.enqueue("r")
	var ran, dropped atomic.Int32
	var wg sync.WaitGroup
	for _, run := range []func(context.Context, func()) bool{first, second, third} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if run(ctx, func() { ran.Add(1); <-blocker }) {
				return
			}
			dropped.Add(1)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(blocker)
	wg.Wait()
	if ran.Load() != 1 || dropped.Load() != 2 {
		t.Errorf("ran = %d, dropped = %d, want 1 and 2", ran.Load(), dropped.Load())
	}
	// The map must not leak finished keys.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tail) != 0 {
		t.Errorf("tail = %v, want empty", s.tail)
	}
}

func TestSchedulerKeyOfAPlanIsTheLowerCasedRepoOrADash(t *testing.T) {
	if orgKey(OrgPlan{Repo: "Demo"}) != "demo" || orgKey(OrgPlan{}) != "-" {
		t.Errorf("keys: %q %q", orgKey(OrgPlan{Repo: "Demo"}), orgKey(OrgPlan{}))
	}
}
