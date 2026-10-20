package pages

import (
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"
)

// hostPool returns a Pool over host memory mapped outside the Go heap, as
// Init would set it up over native reservations.
func hostPool(t *testing.T, pages uint64) *Pool {
	t.Helper()
	memory, err := syscall.Mmap(-1, 0, int(pages*PageSize), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skip("anonymous mmap unavailable:", err)
	}
	stack, err := syscall.Mmap(-1, 0, int((pages*4+PageSize-1)&^(PageSize-1)), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skip("anonymous mmap unavailable:", err)
	}
	t.Cleanup(func() {
		syscall.Munmap(memory)
		syscall.Munmap(stack)
	})
	pool := &Pool{}
	pool.base = uint64(uintptr(unsafe.Pointer(&memory[0])))
	pool.free = uint64(uintptr(unsafe.Pointer(&stack[0])))
	pool.pages, pool.next = pages, 1
	return pool
}

// TestPoolConcurrentOwnership takes and gives back pages from several
// goroutines. A page is never handed to two holders at once, its contents
// stay as its holder wrote them, and every page comes back.
func TestPoolConcurrentOwnership(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			const pageCount = 16
			pool := hostPool(t, pageCount)
			var holders [pageCount + 1]atomic.Int32
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					for round := 0; round < 500; round++ {
						ref, ok := pool.Allocate()
						if !ok {
							continue
						}
						if holders[ref].Add(1) != 1 {
							t.Errorf("page %d handed to two holders", ref)
						}
						page := pool.Page(ref)
						mark := byte(worker*31 + round)
						page[0], page[PageSize-1] = mark, mark
						if page[0] != mark || page[PageSize-1] != mark {
							t.Errorf("page %d changed under its holder", ref)
						}
						holders[ref].Add(-1)
						pool.Free(ref)
					}
				}(worker)
			}
			group.Wait()
			if used := pool.Used(); used != 0 {
				t.Fatalf("Used = %d after every page came back", used)
			}
		})
	}
}

func TestPermanentConcurrentDistinct(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			pool := hostPool(t, 64)
			results := make(chan *[2 * PageSize]byte, 64)
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func() {
					defer group.Done()
					for {
						table := Permanent[[2 * PageSize]byte](pool)
						if table == nil {
							return
						}
						results <- table
					}
				}()
			}
			group.Wait()
			close(results)
			seen := map[*[2 * PageSize]byte]bool{}
			for table := range results {
				if seen[table] {
					t.Fatal("one table handed out twice")
				}
				seen[table] = true
			}
			if len(seen) != 32 {
				t.Fatalf("%d tables of two pages from 64 pages, want 32", len(seen))
			}
		})
	}
}
