package lock

import (
	"goos/internal/validation"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRWAdmitsReadersTogetherAndWritersAlone(t *testing.T) {
	if validation.Enabled {
		t.Skip("goroutines share one processor index, which validation builds treat as recursion")
	}
	Install(Hooks{Pause: runtime.Gosched})
	defer Install(Hooks{})
	var rw RW
	rw.SetLevel(LevelAddressSpace)
	var readers, writers, maxReaders atomic.Int32
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(writer bool) {
			defer group.Done()
			for round := 0; round < 500; round++ {
				if writer {
					rw.Lock()
					if writers.Add(1) != 1 || readers.Load() != 0 {
						t.Error("a writer shared the lock")
					}
					writers.Add(-1)
					rw.Unlock()
					continue
				}
				rw.RLock()
				count := readers.Add(1)
				for peak := maxReaders.Load(); count > peak && !maxReaders.CompareAndSwap(peak, count); peak = maxReaders.Load() {
				}
				if writers.Load() != 0 {
					t.Error("a reader ran beside a writer")
				}
				runtime.Gosched()
				readers.Add(-1)
				rw.RUnlock()
			}
		}(worker%4 == 0)
	}
	group.Wait()
	if rw.state.Load() != 0 {
		t.Fatalf("state %#x after every worker left", rw.state.Load())
	}
}

func TestRWWaitingWriterHoldsOffNewReaders(t *testing.T) {
	Install(Hooks{})
	defer Install(Hooks{})
	var rw RW
	rw.state.Store(1 | rwPending) // a reader inside, a writer waiting
	if rw.tryRead() {
		t.Fatal("a new reader passed a waiting writer")
	}
	stop := uint64(1)
	if rw.LockUnless(&stop) {
		t.Fatal("a stopped writer took a lock a reader holds")
	}
	rw.state.Store(0)
	if !rw.LockUnless(&stop) {
		t.Fatal("a free lock refused")
	}
	rw.Unlock()
	rw.state.Store(rwWriter) // a writer on another processor
	if rw.RLockUnless(&stop) {
		t.Fatal("a stopped reader took a lock a writer holds")
	}
	rw.state.Store(0)
	rw.RLock()
	rw.RUnlock()
}
