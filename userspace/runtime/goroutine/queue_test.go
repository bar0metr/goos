package goroutine

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunQueueConcurrentStealers(t *testing.T) {
	const total = 40000
	for _, workers := range []int{2, 4, 8} {
		t.Run(string(rune('0'+workers)), func(t *testing.T) {
			var q runQueue
			var seen [total]atomic.Uint32
			var done atomic.Bool
			var consumed atomic.Uint32
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var batch [queueSize / 2]uint32
					for !done.Load() || q.length() != 0 {
						count := q.steal(&batch)
						for j := 0; j < count; j++ {
							index := batch[j]
							if index >= total {
								t.Errorf("invalid index %d", index)
								return
							}
							seen[index].Add(1)
							consumed.Add(1)
						}
						threadYield()
					}
				}()
			}
			for i := uint32(0); i < total; i++ {
				for !q.put(i) {
					threadYield()
				}
			}
			done.Store(true)
			wg.Wait()
			if consumed.Load() != total {
				t.Fatalf("consumed %d of %d", consumed.Load(), total)
			}
			for i := range seen {
				if got := seen[i].Load(); got != 1 {
					t.Fatalf("index %d consumed %d times", i, got)
				}
			}
		})
	}
}

func TestRunQueueWrapAndFull(t *testing.T) {
	var q runQueue
	q.head.Store(^uint32(0) - 127)
	q.tail.Store(q.head.Load())
	for i := uint32(0); i < queueSize; i++ {
		if !q.put(i) {
			t.Fatalf("full after %d entries", i)
		}
	}
	if q.put(1234) {
		t.Fatal("accepted overflow")
	}
	for i := uint32(0); i < queueSize; i++ {
		got, ok := q.get()
		if !ok || got != i {
			t.Fatalf("get %d: %d %v", i, got, ok)
		}
	}
	if _, ok := q.get(); ok {
		t.Fatal("entry left in empty queue")
	}
}
