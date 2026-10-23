package lock

import (
	"fmt"
	"sync"
	"testing"
)

func TestMutexExcludes(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var mu Mutex
			// counter and inside are plain words: only the lock orders the
			// accesses, so the race detector reports a missing exclusion.
			counter, inside := 0, 0
			const rounds = 2000
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func() {
					defer group.Done()
					for round := 0; round < rounds; round++ {
						mu.Lock()
						inside++
						if inside != 1 {
							t.Errorf("%d holders inside the lock", inside)
						}
						counter++
						inside--
						mu.Unlock()
					}
				}()
			}
			group.Wait()
			if counter != workers*rounds {
				t.Fatalf("counter = %d, want %d", counter, workers*rounds)
			}
		})
	}
}

func TestTryLock(t *testing.T) {
	var mu Mutex
	if !mu.TryLock() {
		t.Fatal("TryLock of a free Mutex failed")
	}
	if mu.TryLock() {
		t.Fatal("TryLock of a held Mutex succeeded")
	}
	mu.Unlock()
	mu.Lock()
	mu.Unlock()
	if !mu.TryLock() {
		t.Fatal("TryLock after Unlock failed")
	}
	mu.Unlock()
}
