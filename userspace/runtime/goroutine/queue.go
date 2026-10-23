package goroutine

import "sync/atomic"

// runQueue is a thread's queue of runnable goroutines, as table indexes.
// Only its thread puts and takes at the tail; any thread takes at the head,
// with a compare-and-swap, so stealing needs no lock.
type runQueue struct {
	head atomic.Uint32
	tail atomic.Uint32
	ring [queueSize]atomic.Uint32
}

// put appends index. Only the owner calls it. It reports false when the
// queue is full.
func (q *runQueue) put(index uint32) bool {
	head, tail := q.head.Load(), q.tail.Load()
	if tail-head >= queueSize {
		return false
	}
	q.ring[tail%queueSize].Store(index)
	q.tail.Store(tail + 1)
	return true
}

// get takes the oldest index.
func (q *runQueue) get() (uint32, bool) {
	for {
		head, tail := q.head.Load(), q.tail.Load()
		if head == tail {
			return 0, false
		}
		index := q.ring[head%queueSize].Load()
		if q.head.CompareAndSwap(head, head+1) {
			return index, true
		}
	}
}

// steal takes up to half of the queue into into, oldest first, and returns
// how many it took.
func (q *runQueue) steal(into *[queueSize / 2]uint32) int {
	for {
		head, tail := q.head.Load(), q.tail.Load()
		count := tail - head
		if count > queueSize {
			// The owner moved on between the loads; read again.
			continue
		}
		count -= count / 2
		if count == 0 {
			return 0
		}
		for offset := uint32(0); offset < count; offset++ {
			into[offset] = q.ring[(head+offset)%queueSize].Load()
		}
		if q.head.CompareAndSwap(head, head+count) {
			return int(count)
		}
	}
}

// length returns how many indexes the queue holds, approximately when
// other threads change it.
func (q *runQueue) length() uint32 {
	head, tail := q.head.Load(), q.tail.Load()
	if tail-head > queueSize {
		return 0
	}
	return tail - head
}

// globalQueue is the queue every thread may use, linked through G.next.
// Its lock is the scheduler lock.
type globalQueue struct {
	head, tail uint32 // index plus one
	length     atomic.Int32
}

func (q *globalQueue) push(table []G, index uint32) {
	table[index].next = 0
	if q.tail == 0 {
		q.head = index + 1
	} else {
		table[q.tail-1].next = index + 1
	}
	q.tail = index + 1
	q.length.Add(1)
}

func (q *globalQueue) pop(table []G) (uint32, bool) {
	if q.head == 0 {
		return 0, false
	}
	index := q.head - 1
	q.head = table[index].next
	if q.head == 0 {
		q.tail = 0
	}
	table[index].next = 0
	q.length.Add(-1)
	return index, true
}
