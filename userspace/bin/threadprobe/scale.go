//go:build goos_userspace

package main

import (
	"runtime"
	"sync/atomic"

	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
	"goos/userspace/utility"
	"goos/userspace/wait"
)

// Allocator workloads beyond the bench (ADR 0205). They use only the
// runtime interfaces 0.2.8 already had, so the same probe runs against
// that allocator for a comparison.
//
// "remote N OPS one|two" pairs threads: in each pair one allocates and the
// other frees what it got (one), or both do (two). Every object carries a
// tag its producer wrote; a consumer that reads another tag found a slot
// handed out twice. Producers also drop garbage, so collections run while
// remote frees wait. "live N OPS KEEP" is the bench with KEEP objects kept
// per thread, so most of the heap survives each collection. "idle N
// ROUNDS" allocates a burst on N threads, then has every thread wait until
// the idle collection has run, and prints the process's pages each round.
// "quota N ROUNDS" has N threads hold a third of the page quota left at
// start, drop it, and take it again, ROUNDS times: the collector must keep
// the process inside its quota.

// pairRing carries objects from one thread of a pair to the other. Only
// the producer writes a slot and tail, only the consumer clears a slot and
// writes head; the atomic stores publish the slots.
type pairRing struct {
	slots      [256][]byte
	head, tail atomic.Uint32
	done       atomic.Bool
	_          [40]byte
}

var (
	remoteRings  [thread.Slots]pairRing
	remoteBad    atomic.Uint64
	remoteFreed  atomic.Uint64
	remoteTwoWay bool
	remoteSink   [thread.Slots]atomic.Pointer[byte]
	remoteSizes  = [...]int{16, 48, 96, 200, 400, 1000, 2000}
)

// tagWord reads or, with set, writes the eight bytes of buffer at offset.
func tagWord(buffer []byte, offset int, value uint64, set bool) uint64 {
	word := uint64(0)
	for index := 7; index >= 0; index-- {
		if set {
			buffer[offset+index] = byte(value >> (8 * index))
		}
		word = word<<8 | uint64(buffer[offset+index])
	}
	return word
}

// push hands buffer to the ring, consuming inbox while the ring is full.
func (r *pairRing) push(buffer []byte, inbox *pairRing) {
	for r.tail.Load()-r.head.Load() == uint32(len(r.slots)) {
		if inbox == nil || !inbox.pop() {
			thread.Yield()
		}
	}
	tail := r.tail.Load()
	r.slots[tail%uint32(len(r.slots))] = buffer
	r.tail.Store(tail + 1)
}

// pop frees the oldest object in the ring after checking its tag, and
// reports whether there was one.
func (r *pairRing) pop() bool {
	head := r.head.Load()
	if head == r.tail.Load() {
		return false
	}
	slot := &r.slots[head%uint32(len(r.slots))]
	buffer := *slot
	*slot = nil
	r.head.Store(head + 1)
	tag := tagWord(buffer, 0, 0, false)
	if tag>>56 == 0 || tagWord(buffer, 8, 0, false) != ^tag {
		remoteBad.Add(1)
	}
	arena.FreeBytes(buffer)
	remoteFreed.Add(1)
	return true
}

// remoteLoop is one thread of a pair: it sends benchOps objects to its
// partner's ring and frees what arrives in its own.
func remoteLoop(index int) {
	partner := index ^ 1
	out, in := &remoteRings[partner], &remoteRings[index]
	producer := remoteTwoWay || index%2 == 0
	if producer {
		for op := 0; op < benchOps; op++ {
			size := remoteSizes[(op+index)%len(remoteSizes)]
			buffer := make([]byte, size)
			tag := uint64(index+1)<<56 | uint64(op)
			tagWord(buffer, 0, tag, true)
			tagWord(buffer, 8, ^tag, true)
			out.push(buffer, in)
			in.pop()
			// Garbage no one frees: the heap reaches its goal, so
			// collections run while remote frees wait in the slabs.
			// The sink makes it escape to the heap.
			garbage := make([]byte, remoteSizes[(op+index+3)%len(remoteSizes)])
			garbage[0] = byte(op)
			remoteSink[index].Store(&garbage[0])
		}
	}
	// The partner frees until this thread says it sends no more.
	out.done.Store(true)
	for !in.done.Load() || in.head.Load() != in.tail.Load() {
		if !in.pop() {
			thread.Yield()
		}
	}
}

func remoteWorker(argument uintptr) {
	benchReady.Add(1)
	for !benchGo.Load() {
		thread.Yield()
	}
	remoteLoop(int(argument))
}

func remote(count int, ops int, mode []byte) {
	expect(count >= 2 && count%2 == 0 && count <= maxWorkers+1 && ops >= 1, "arguments")
	expect(utility.Equal(mode, "one") || utility.Equal(mode, "two"), "mode")
	benchOps, remoteTwoWay = ops, utility.Equal(mode, "two")
	before := arena.HeapStats()
	start := startWorkers(count, remoteWorker)
	remoteLoop(0)
	elapsed := joinWorkers(count, start)
	after := arena.HeapStats()
	utility.PrintText("THREADPROBE_REMOTE mode=")
	utility.PrintText(string(mode))
	report(" threads=", uint64(count), " freed=", remoteFreed.Load(), " ns=", elapsed, " collections=", uint64(after.Collections-before.Collections))
	report("THREADPROBE_REMOTE_CHECK bad=", remoteBad.Load(), "", 0, "", 0, "", 0)
	printProfile()
	expect(remoteBad.Load() == 0, "every object reached its consumer intact")
	marker("THREADPROBE_REMOTE_OK")
}

// startWorkers starts count-1 workers on fn, waits until each is ready,
// releases them, and returns the start time.
func startWorkers(count int, fn func(uintptr)) uint64 {
	for index := 0; index < count-1; index++ {
		id, status := thread.Start(fn, uintptr(index+1))
		expect(status == kcall.StatusOK, "thread start")
		workerIDs[index] = id
	}
	for int(benchReady.Load()) < count-1 {
		thread.Yield()
	}
	start := utility.ClockNanos()
	benchGo.Store(true)
	return start
}

// joinWorkers joins what startWorkers started and returns the time since
// start.
func joinWorkers(count int, start uint64) uint64 {
	for index := 0; index < count-1; index++ {
		_, status := thread.Join(workerIDs[index])
		expect(status == kcall.StatusOK, "join")
	}
	return utility.ClockNanos() - start
}

var workerIDs [maxWorkers]thread.ID

// High-live workload: the bench mix, with liveKeep objects kept by each
// thread.
var liveKeep int

//go:noinline
func liveLoop(index int) {
	ring := make([][]byte, liveKeep)
	for op := 0; op < benchOps; op++ {
		buffer := make([]byte, benchSizes[(op+index)%len(benchSizes)])
		buffer[0] = byte(op)
		ring[op%len(ring)] = buffer
	}
	benchSums[index].Store(uint64(ring[0][0]))
}

func liveWorker(argument uintptr) {
	benchReady.Add(1)
	for !benchGo.Load() {
		thread.Yield()
	}
	liveLoop(int(argument))
}

func live(count, ops, keep int) {
	expect(count >= 1 && count <= maxWorkers+1 && ops >= 1 && keep >= 1, "arguments")
	benchOps, liveKeep = ops, keep
	before := arena.HeapStats()
	start := startWorkers(count, liveWorker)
	liveLoop(0)
	elapsed := joinWorkers(count, start)
	after := arena.HeapStats()
	report("THREADPROBE_LIVE threads=", uint64(count), " keep=", uint64(keep), " ns=", elapsed, " collections=", uint64(after.Collections-before.Collections))
	printProfile()
	marker("THREADPROBE_LIVE_OK")
}

// Burst and idle: every round, every thread allocates a burst and then
// waits long enough for the idle collection.
var (
	idleRounds  int
	idleArrived atomic.Uint32
	// idleRound is the round the workers may start; the main thread
	// raises it once it read the last one's pages.
	idleRound atomic.Uint32
)

const (
	idleBurst = 20000
	idleWait  = clock.TicksPerSecond * 4 / 10
)

//go:noinline
func idleBurstLoop(index, round int) {
	var ring [64][]byte
	for op := 0; op < idleBurst; op++ {
		buffer := make([]byte, benchSizes[(op+index+round)%len(benchSizes)])
		buffer[0] = byte(op)
		ring[op%len(ring)] = buffer
	}
	benchSums[index].Store(uint64(ring[0][0]))
}

func idleLoop(index int) {
	for round := 0; round < idleRounds; round++ {
		idleBurstLoop(index, round)
		idleArrived.Add(1)
		// The thread stays in wait.Any, idle, until the main thread has
		// read the round's pages.
		for int(idleRound.Load()) <= round {
			wait.Sleep(idleWait)
		}
	}
}

func idleWorker(argument uintptr) {
	benchReady.Add(1)
	for !benchGo.Load() {
		thread.Yield()
	}
	idleLoop(int(argument))
}

func idle(count, rounds int) {
	expect(count >= 1 && count <= maxWorkers+1 && rounds >= 1, "arguments")
	idleRounds = rounds
	start := startWorkers(count, idleWorker)
	for round := 0; round < rounds; round++ {
		idleBurstLoop(0, round)
		idleArrived.Add(1)
		for int(idleArrived.Load()) < count*(round+1) {
			thread.Yield()
		}
		// Every worker waits now: this wait's idle probe runs out and
		// the idle collection runs.
		wait.Sleep(idleWait)
		stats := arena.HeapStats()
		report("THREADPROBE_IDLE round=", uint64(round), " pages=", processPages(), " held=", stats.Held, " collections=", uint64(stats.Collections))
		idleRound.Store(uint32(round + 1))
	}
	elapsed := joinWorkers(count, start)
	report("THREADPROBE_IDLE_DONE threads=", uint64(count), " rounds=", uint64(rounds), " ns=", elapsed, "", 0)
	printProfile()
	marker("THREADPROBE_IDLE_OK")
}

// processInfos are globals so that reading them allocates nothing, one per
// thread so that threads read their records at once.
var processInfos [thread.Slots][kcall.ProcessInfoSize]byte

// processPages returns the pages charged to this process, and quotaPages
// its page quota.
func processPages() uint64 { return processCounter(kcall.ProcessInfoUsedOffset) }
func quotaPages() uint64   { return processCounter(kcall.ProcessInfoLimitsOffset) }

func processCounter(base int) uint64 {
	record := &processInfos[thread.Slot()]
	expect(sys.ProcessInfo(kcall.ProcessInfoSelf, record) == kcall.StatusOK, "process record")
	offset := base + 4*kcall.ProcessPages
	value := uint64(0)
	for index := 3; index >= 0; index-- {
		value = value<<8 | uint64(record[offset+index])
	}
	return value
}

// Quota pressure: every round each thread holds its share of a third of
// the quota left at start, then drops it.
var (
	quotaShare  uint64
	quotaRounds int
	quotaPeak   atomic.Uint64
)

//go:noinline
func quotaRound(index, round int) {
	held := make([][]byte, 0, 1024)
	total := uint64(0)
	for op := 0; total < quotaShare; op++ {
		size := remoteSizes[(op+index+round)%len(remoteSizes)]
		if op%16 == 0 {
			size = 6000
		}
		buffer := make([]byte, size)
		buffer[0] = byte(op)
		held = append(held, buffer)
		total += uint64(size)
		if op%256 == 0 {
			if pages := processPages(); pages > quotaPeak.Load() {
				quotaPeak.Store(pages)
			}
		}
	}
	runtime.KeepAlive(held)
}

func quotaLoop(index int) {
	for round := 0; round < quotaRounds; round++ {
		quotaRound(index, round)
	}
}

func quotaWorker(argument uintptr) {
	benchReady.Add(1)
	for !benchGo.Load() {
		thread.Yield()
	}
	quotaLoop(int(argument))
}

func quota(count, rounds int) {
	expect(count >= 1 && count <= maxWorkers+1 && rounds >= 1, "arguments")
	limit, used := quotaPages(), processPages()
	expect(limit > used, "page quota")
	quotaShare, quotaRounds = (limit-used)*kcall.SystemInfoPageSize/3/uint64(count), rounds
	before := arena.HeapStats()
	start := startWorkers(count, quotaWorker)
	quotaLoop(0)
	elapsed := joinWorkers(count, start)
	after := arena.HeapStats()
	report("THREADPROBE_QUOTA threads=", uint64(count), " limit=", limit, " peak=", quotaPeak.Load(), " collections=", uint64(after.Collections-before.Collections))
	report("THREADPROBE_QUOTA_TIME ns=", elapsed, " start=", used, "", 0, "", 0)
	printProfile()
	marker("THREADPROBE_QUOTA_OK")
}
