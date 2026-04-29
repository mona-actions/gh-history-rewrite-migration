package workdir

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLock_ConcurrentContention spawns N goroutines all attempting to Lock
// the same WorkDir. Exactly one must succeed; the rest must fail with a
// clear "locked by process" (or other "locked") error. This verifies the
// O_EXCL atomic-create contract of Lock(): even with concurrent same-process
// callers, only one wins. It also guards against the empty-file window
// between OpenFile(O_EXCL) succeeding and the PID being flushed — a racing
// loser must NEVER auto-remove the winner's not-yet-populated lock.
func TestLock_ConcurrentContention(t *testing.T) {
	wd, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	t.Cleanup(func() { _ = wd.Unlock() })

	const goroutines = 8
	var (
		successes int32
		failures  int32
		errs      []error
		errMu     sync.Mutex
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := wd.Lock(); err != nil {
				atomic.AddInt32(&failures, 1)
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
				return
			}
			atomic.AddInt32(&successes, 1)
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&successes); got != 1 {
		t.Fatalf("expected exactly 1 successful Lock, got %d (failures=%d)",
			got, atomic.LoadInt32(&failures))
	}
	if got := atomic.LoadInt32(&failures); got != goroutines-1 {
		t.Fatalf("expected %d failed Locks, got %d", goroutines-1, got)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "locked") {
			t.Errorf("expected 'locked' in error, got: %v", e)
		}
	}
}
