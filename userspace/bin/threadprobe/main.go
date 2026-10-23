//go:build goos_userspace

// Command threadprobe checks native threads from inside the running system
// (ADR 0203). "smoke N" runs N threads that compute without allocating and
// shows they ran on several processors at once; "lifecycle" checks thread
// IDs, joins, exit codes, and the task quota; "gc N ROUNDS" has N threads
// allocate, share, and drop objects across collections; "roots" checks the
// roots of threads that run, wait in a call, or allocate in a region while
// another collects; "region-window N ROUNDS" stops a thread inside region
// transitions while another collects (region_window.go); "storm N ROUNDS"
// has N threads ask for collections at once; "fault" and "overflow" end a worker with a fault, which ends the
// process; "bench N OPS MODE" times N threads that allocate, with the heap
// lock alone (lock), with thread-owned slabs (cache), or with owned slabs
// and 256 free pages retained (retain), and prints the allocator profile. Only the main thread prints. It is built only into validation
// images.
package main

import (
	"math/bits"
	"runtime"
	"sync/atomic"

	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "threadprobe smoke|lifecycle|gc|roots|region-window|storm|fault|overflow|bench [ARGS...]",
	Summary: "native thread checks",
	MaxArgs: 4,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	if data.ArgCount() < 1 {
		utility.Usage()
	}
	// Running out of heap is reported, never waited out.
	arena.OnExhausted(func() {
		stats := arena.HeapStats()
		report("THREADPROBE_EXHAUSTED live=", stats.Live, " held=", stats.Held, " collections=", uint64(stats.Collections), "", 0)
		printProfile()
		utility.Exit(4)
	})
	mode, _ := data.Arg(1)
	switch {
	case utility.Equal(mode, "smoke") && data.ArgCount() == 2:
		count, _ := data.Arg(2)
		smoke(int(parse(count)))
	case utility.Equal(mode, "lifecycle"):
		lifecycle()
	case utility.Equal(mode, "gc") && data.ArgCount() == 3:
		count, _ := data.Arg(2)
		rounds, _ := data.Arg(3)
		collect(int(parse(count)), int(parse(rounds)))
	case utility.Equal(mode, "roots"):
		roots()
	case utility.Equal(mode, "region-window") && data.ArgCount() == 3:
		count, _ := data.Arg(2)
		rounds, _ := data.Arg(3)
		regionWindow(int(parse(count)), int(parse(rounds)))
	case utility.Equal(mode, "storm") && data.ArgCount() == 3:
		count, _ := data.Arg(2)
		rounds, _ := data.Arg(3)
		storm(int(parse(count)), int(parse(rounds)))
	case utility.Equal(mode, "bench") && data.ArgCount() == 4:
		count, _ := data.Arg(2)
		ops, _ := data.Arg(3)
		kind, _ := data.Arg(4)
		if !utility.Equal(kind, "lock") && !utility.Equal(kind, "cache") && !utility.Equal(kind, "retain") {
			utility.Usage()
		}
		if utility.Equal(kind, "retain") {
			arena.SetRetained(256)
		}
		bench(int(parse(count)), int(parse(ops)), !utility.Equal(kind, "lock"), kind)
	case utility.Equal(mode, "fault"):
		crash(faultWorker)
	case utility.Equal(mode, "overflow"):
		crash(overflowWorker)
	default:
		utility.Usage()
	}
}

func expect(ok bool, what string) {
	if !ok {
		utility.PrintText("threadprobe: ")
		utility.PrintText(what)
		utility.PrintText(" failed")
		utility.EndLine()
		utility.Exit(1)
	}
}

func marker(text string) {
	utility.PrintText(text)
	utility.EndLine()
}

func report(first string, a uint64, second string, b uint64, third string, c uint64, fourth string, d uint64) {
	utility.PrintText(first)
	utility.PrintUint(a)
	for _, pair := range [...]struct {
		label string
		value uint64
	}{{second, b}, {third, c}, {fourth, d}} {
		if pair.label != "" {
			utility.PrintText(pair.label)
			utility.PrintUint(pair.value)
		}
	}
	utility.EndLine()
}

func parse(text []byte) uint64 {
	value := uint64(0)
	for _, digit := range text {
		if digit < '0' || digit > '9' || value > 1<<20 {
			utility.Usage()
		}
		value = value*10 + uint64(digit-'0')
	}
	return value
}

// maxWorkers is the most threads a mode starts besides the main one.
const maxWorkers = thread.Slots - 1

// Shared state of a smoke run. Workers write only their own entry.
var (
	stop      atomic.Bool
	ourTasks  [thread.Slots]atomic.Uint64
	smokeRuns [maxWorkers]struct {
		iterations, peak, checksum atomic.Uint64
		_                          [40]byte
	}
	cpuWires [thread.Slots][kcall.CPURuntimeSize]byte
)

// running counts the threads of this process that one pass over the
// processors finds running. A thread that moves between processors during
// the pass is counted once.
func running(slot int) uint64 {
	seen := uint32(0)
	for cpu := uint64(0); cpu < 64; cpu++ {
		if sys.CPURuntime(cpu, &cpuWires[slot]) != kcall.StatusOK {
			break
		}
		state, ok := kcall.DecodeCPURuntime(cpuWires[slot][:])
		if !ok || !state.Online || state.Current == 0 {
			continue
		}
		for index := range ourTasks {
			if value := ourTasks[index].Load(); value != 0 && value == state.Current {
				seen |= 1 << index
				break
			}
		}
	}
	return uint64(bits.OnesCount32(seen))
}

// cpuWorker computes until stop, without allocating, and samples which
// processors run this process's threads.
func cpuWorker(argument uintptr) {
	index := int(argument)
	slot := thread.Slot()
	state := uint64(index)*0x9e3779b97f4a7c15 | 1
	iterations := uint64(0)
	for !stop.Load() {
		for step := 0; step < 4096; step++ {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
		}
		iterations++
		if iterations%8 == 0 {
			if seen := running(slot); seen > smokeRuns[index].peak.Load() {
				smokeRuns[index].peak.Store(seen)
			}
		}
	}
	smokeRuns[index].iterations.Store(iterations)
	smokeRuns[index].checksum.Store(state)
}

func smoke(count int) {
	expect(count >= 1 && count <= maxWorkers, "thread count")
	ourTasks[0].Store(uint64(thread.Self()))
	var ids [maxWorkers]thread.ID
	for index := 0; index < count; index++ {
		id, status := thread.Start(cpuWorker, uintptr(index))
		expect(status == kcall.StatusOK, "thread start")
		ids[index] = id
		ourTasks[index+1].Store(uint64(id))
	}
	mainPeak := uint64(0)
	for round := 0; round < 40; round++ {
		utility.Sleep(5)
		if seen := running(0); seen > mainPeak {
			mainPeak = seen
		}
	}
	stop.Store(true)
	peak, total := mainPeak, uint64(0)
	for index := 0; index < count; index++ {
		code, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK && code == 0, "join")
		peak = max(peak, smokeRuns[index].peak.Load())
		total += smokeRuns[index].iterations.Load()
		expect(smokeRuns[index].iterations.Load() > 0, "every thread ran")
	}
	processors := uint64(0)
	for cpu := uint64(0); cpu < 64; cpu++ {
		if sys.CPURuntime(cpu, &cpuWires[0]) != kcall.StatusOK {
			break
		}
		if state, ok := kcall.DecodeCPURuntime(cpuWires[0][:]); ok && state.Online {
			processors++
		}
	}
	report("THREADPROBE_SMOKE threads=", uint64(count), " processors=", processors, " parallel_peak=", peak, " iterations=", total)
	if processors >= 2 && count >= 2 {
		expect(peak >= 2, "threads of one process ran on two processors at once")
	}
	marker("THREADPROBE_SMOKE_OK")
}

// Lifecycle checks.
var (
	exitWith  atomic.Uint64
	joinSelf  atomic.Uint64
	selfIDs   [thread.Slots]atomic.Uint64
	released  atomic.Bool
	startedOK atomic.Uint32
)

func exitWorker(argument uintptr) {
	selfIDs[thread.Slot()].Store(uint64(thread.Self()))
	thread.Exit(uint64(argument))
}

func selfJoinWorker(uintptr) {
	_, status := thread.Join(thread.Self())
	joinSelf.Store(uint64(status))
}

func holdWorker(uintptr) {
	startedOK.Add(1)
	for !released.Load() {
		thread.Yield()
	}
}

func lifecycle() {
	mainID := thread.Self()
	expect(mainID != 0 && thread.Slot() == 0, "the main thread has an ID and slot 0")

	id, status := thread.Start(exitWorker, 5)
	expect(status == kcall.StatusOK, "start")
	code, status := thread.Join(id)
	expect(status == kcall.StatusOK && code == 5, "a thread's exit code reaches its joiner")
	_, status = thread.Join(id)
	expect(status == kcall.StatusNotFound, "a thread joins once")
	_, status = thread.Join(thread.ID(12345))
	expect(status == kcall.StatusNotFound, "an unknown ID is refused")

	id, status = thread.Start(selfJoinWorker, 0)
	expect(status == kcall.StatusOK, "start")
	code, status = thread.Join(id)
	expect(status == kcall.StatusOK && code == 0 && joinSelf.Load() == uint64(kcall.StatusInvalidArgument), "a thread cannot join itself")

	// Start until the task quota refuses; every thread waits to be let go.
	var ids [maxWorkers]thread.ID
	started := 0
	for started < maxWorkers {
		id, status := thread.Start(holdWorker, 0)
		if status != kcall.StatusOK {
			expect(status == kcall.StatusResourceExhausted, "the quota refuses with ResourceExhausted")
			break
		}
		ids[started] = id
		started++
	}
	expect(started >= 1, "at least one thread starts")
	for int(startedOK.Load()) < started {
		thread.Yield()
	}
	_, done, status := thread.Done(ids[0])
	expect(status == kcall.StatusOK && !done, "a running thread is not done")
	released.Store(true)
	for index := 0; index < started; index++ {
		_, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK, "join")
	}
	// A slot freed by a join takes the next thread, under a new ID.
	again, status := thread.Start(exitWorker, 1)
	expect(status == kcall.StatusOK, "a freed slot is used again")
	_, status = thread.Join(again)
	expect(status == kcall.StatusOK && again != ids[0] && again != id, "a new thread gets a new ID")
	report("THREADPROBE_LIFECYCLE quota_threads=", uint64(started), "", 0, "", 0, "", 0)
	marker("THREADPROBE_LIFECYCLE_OK")
}

// Collection checks.
type node struct {
	next  *node
	value uint64
	data  []byte
}

var (
	gcRounds   int
	gcFailures atomic.Uint64
	gcChecks   atomic.Uint64
	shared     [maxWorkers]atomic.Pointer[node]
)

//go:noinline
func buildList(seed uint64, length int) *node {
	var head *node
	for index := 0; index < length; index++ {
		head = &node{next: head, value: seed + uint64(index), data: make([]byte, 16+index%200)}
		head.data[0] = byte(seed)
	}
	return head
}

//go:noinline
func listHolds(head *node, seed uint64, length int) bool {
	count := 0
	expected := seed + uint64(length) - 1
	for n := head; n != nil; n = n.next {
		if n.value != expected || n.data[0] != byte(seed) {
			return false
		}
		expected--
		count++
	}
	return count == length
}

// gcWorker builds lists only it holds, hands one to the next worker, checks
// the one it was handed, and drops the rest, collecting now and then.
func gcWorker(argument uintptr) {
	index := int(argument)
	for round := 0; round < gcRounds; round++ {
		seed := uint64(index)<<32 | uint64(round)<<8
		mine := buildList(seed, 300)
		shared[index].Store(buildList(seed|1, 100))
		for garbage := 0; garbage < 8; garbage++ {
			buildList(seed|2, 200)
		}
		if round%4 == index%4 {
			runtime.GC()
		}
		if !listHolds(mine, seed, 300) {
			gcFailures.Add(1)
		}
		if other := shared[(index+1)%len(shared)].Load(); other != nil {
			gcChecks.Add(1)
		}
		runtime.KeepAlive(mine)
	}
}

func collect(count int, rounds int) {
	expect(count >= 1 && count <= maxWorkers && rounds >= 1, "arguments")
	gcRounds = rounds
	before := arena.HeapStats()
	var ids [maxWorkers]thread.ID
	for index := 0; index < count; index++ {
		id, status := thread.Start(gcWorker, uintptr(index))
		expect(status == kcall.StatusOK, "thread start")
		ids[index] = id
	}
	mine := buildList(7, 500)
	for round := 0; round < rounds; round++ {
		buildList(9, 300)
		if round%3 == 0 {
			runtime.GC()
		}
	}
	for index := 0; index < count; index++ {
		_, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK, "join")
	}
	expect(listHolds(mine, 7, 500), "the main thread's list survived")
	for index := range shared {
		shared[index].Store(nil)
	}
	runtime.GC()
	after := arena.HeapStats()
	report("THREADPROBE_GC threads=", uint64(count), " collections=", uint64(after.Collections-before.Collections), " failures=", gcFailures.Load(), " live=", after.Live)
	expect(gcFailures.Load() == 0, "lists held only by their threads survived every collection")
	expect(after.Collections > before.Collections, "collections ran")
	marker("THREADPROBE_GC_OK")
}

// Root checks. Each worker holds lists no global names while the main thread
// collects: one computes in a loop that makes no call, one waits in a
// sleeping call, and one keeps its lists in objects of a region it is inside.
const (
	rootLists  = 24
	rootLength = 40
)

var (
	rootsHeld   atomic.Uint32
	rootsStop   atomic.Bool
	rootsResult [3]atomic.Uint32
	// handed passes each worker its lists; the worker clears its entry
	// before it reports that it holds them.
	handed [3]atomic.Pointer[[rootLists]*node]
)

func rootSeed(worker, list int) uint64 { return uint64(worker+1)<<40 | uint64(list)<<16 }

//go:noinline
func takeLists(worker int) *[rootLists]*node {
	lists := handed[worker].Load()
	handed[worker].Store(nil)
	return lists
}

//go:noinline
func checkLists(worker int, lists *[rootLists]*node) uint32 {
	good := uint32(0)
	for index, head := range lists {
		if listHolds(head, rootSeed(worker, index), rootLength) {
			good++
		}
	}
	return good
}

// spinWorker holds its lists while it loops on an atomic load.
func spinWorker(uintptr) {
	lists := takeLists(0)
	rootsHeld.Add(1)
	spins := uint64(0)
	for !rootsStop.Load() {
		spins++
	}
	crashSink.Store(spins)
	rootsResult[0].Store(checkLists(0, lists))
}

// sleepWorker holds its lists while it waits in the kernel.
func sleepWorker(uintptr) {
	lists := takeLists(1)
	rootsHeld.Add(1)
	for !rootsStop.Load() {
		utility.Sleep(1)
	}
	rootsResult[1].Store(checkLists(1, lists))
}

// holder is allocated in a region; it is the only record of its list.
type holder struct {
	list *node
}

// regionWorker moves its lists into holders it allocates inside a region,
// so the region's objects past its recorded position hold them.
func regionWorker(uintptr) {
	lists := takeLists(2)
	region := arena.NewRegion(64 << 10)
	scope := region.Enter()
	var holders [rootLists]*holder
	for index := range lists {
		holders[index] = &holder{list: lists[index]}
		lists[index] = nil
	}
	rootsHeld.Add(1)
	for !rootsStop.Load() {
		thread.Yield()
	}
	good := uint32(0)
	for index, h := range holders {
		if listHolds(h.list, rootSeed(2, index), rootLength) {
			good++
		}
	}
	scope.Leave()
	region.Release()
	rootsResult[2].Store(good)
}

func roots() {
	workers := [...]func(uintptr){spinWorker, sleepWorker, regionWorker}
	var ids [len(workers)]thread.ID
	for worker := range workers {
		lists := new([rootLists]*node)
		for index := range lists {
			lists[index] = buildList(rootSeed(worker, index), rootLength)
		}
		handed[worker].Store(lists)
		id, status := thread.Start(workers[worker], 0)
		expect(status == kcall.StatusOK, "thread start")
		ids[worker] = id
	}
	for rootsHeld.Load() < uint32(len(workers)) {
		thread.Yield()
	}
	before := arena.HeapStats()
	for round := 0; round < 12; round++ {
		for garbage := 0; garbage < 40; garbage++ {
			buildList(uint64(round), 64)
		}
		runtime.GC()
	}
	after := arena.HeapStats()
	rootsStop.Store(true)
	for worker := range workers {
		_, status := thread.Join(ids[worker])
		expect(status == kcall.StatusOK, "join")
	}
	report("THREADPROBE_ROOTS running=", uint64(rootsResult[0].Load()), " sleeping=", uint64(rootsResult[1].Load()),
		" region=", uint64(rootsResult[2].Load()), " collections=", uint64(after.Collections-before.Collections))
	expect(rootsResult[0].Load() == rootLists, "a running thread's lists survived")
	expect(rootsResult[1].Load() == rootLists, "a sleeping thread's lists survived")
	expect(rootsResult[2].Load() == rootLists, "a region's objects kept their lists")
	marker("THREADPROBE_ROOTS_OK")
}

// stormWorker asks for a collection every round while it allocates.
func stormWorker(argument uintptr) {
	index := int(argument)
	for round := 0; round < gcRounds; round++ {
		seed := uint64(index)<<32 | uint64(round)<<8 | 3
		mine := buildList(seed, 50)
		runtime.GC()
		if !listHolds(mine, seed, 50) {
			gcFailures.Add(1)
		}
		gcChecks.Add(1)
	}
}

func storm(count int, rounds int) {
	expect(count >= 2 && count <= maxWorkers && rounds >= 1, "arguments")
	gcRounds = rounds
	before := arena.HeapStats()
	var ids [maxWorkers]thread.ID
	for index := 0; index < count; index++ {
		id, status := thread.Start(stormWorker, uintptr(index))
		expect(status == kcall.StatusOK, "thread start")
		ids[index] = id
	}
	for index := 0; index < count; index++ {
		_, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK, "join")
	}
	after := arena.HeapStats()
	collections := uint64(after.Collections - before.Collections)
	requests := uint64(count * rounds)
	report("THREADPROBE_STORM threads=", uint64(count), " requests=", requests, " collections=", collections, " failures=", gcFailures.Load())
	expect(gcFailures.Load() == 0, "objects survived concurrent collection requests")
	expect(collections >= 1 && collections <= requests, "collection requests were served")
	marker("THREADPROBE_STORM_OK")
}

var crashSink atomic.Uint64

// faultWorker writes to a page it unmapped.
func faultWorker(uintptr) {
	base, status := sys.VMReserve(4096, kcall.VMRead|kcall.VMWrite)
	if status == kcall.StatusOK && sys.VMUnmap(base, 4096) == kcall.StatusOK {
		sys.Window(base, 4096)[0] = 1
	}
}

// overflowWorker recurses past its stack into the guard below it.
func overflowWorker(uintptr) {
	crashSink.Store(uint64(recurse(1 << 30)))
}

// recurse keeps its frame on the stack: TinyGo moves a local array larger
// than 256 bytes to the heap, and such frames would exhaust the page quota
// before the stack reached its guard. Its result feeds the return twice, so
// LLVM cannot turn the recursion into a loop with an accumulator.
//
//go:noinline
func recurse(depth int) int {
	var frame [256]byte
	frame[0], frame[len(frame)-1] = byte(depth), byte(depth>>8)
	if depth <= 1 {
		return int(frame[0])
	}
	result := recurse(depth - 1)
	return result ^ result>>3 ^ int(frame[len(frame)-1])
}

// crash starts a worker that faults. The fault ends the whole process, so
// the main thread never prints the marker after its join.
func crash(worker func(uintptr)) {
	id, status := thread.Start(worker, 0)
	expect(status == kcall.StatusOK, "thread start")
	marker("THREADPROBE_CRASH_STARTED")
	thread.Join(id)
	marker("THREADPROBE_CRASH_SURVIVED")
}

// Allocation benchmark. Every thread, the main one included, allocates a
// mix of small sizes and keeps the last 64 alive.
var (
	benchOps   int
	benchReady atomic.Uint32
	benchGo    atomic.Bool
	benchSums  [thread.Slots]atomic.Uint64
	benchSizes = [...]int{16, 24, 48, 64, 100, 200, 400, 1000}
)

//go:noinline
func benchLoop(index int) {
	var ring [64][]byte
	for op := 0; op < benchOps; op++ {
		buffer := make([]byte, benchSizes[(op+index)%len(benchSizes)])
		buffer[0] = byte(op)
		ring[op%len(ring)] = buffer
	}
	benchSums[index].Store(uint64(ring[0][0]))
}

func benchWorker(argument uintptr) {
	benchReady.Add(1)
	for !benchGo.Load() {
		thread.Yield()
	}
	benchLoop(int(argument))
}

func bench(count int, ops int, caches bool, mode []byte) {
	expect(count >= 1 && count <= thread.Slots && ops >= 1, "arguments")
	benchOps = ops
	arena.UseThreadCaches(caches)
	var ids [maxWorkers]thread.ID
	for index := 0; index < count-1; index++ {
		id, status := thread.Start(benchWorker, uintptr(index+1))
		expect(status == kcall.StatusOK, "thread start")
		ids[index] = id
	}
	for int(benchReady.Load()) < count-1 {
		thread.Yield()
	}
	before := arena.HeapStats()
	start := utility.ClockNanos()
	benchGo.Store(true)
	benchLoop(0)
	for index := 0; index < count-1; index++ {
		_, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK, "join")
	}
	elapsed := utility.ClockNanos() - start
	after := arena.HeapStats()
	total := uint64(count) * uint64(ops)
	utility.PrintText("THREADPROBE_BENCH mode=")
	for _, name := range [...]string{"lock", "cache", "retain"} {
		if utility.Equal(mode, name) {
			utility.PrintText(name)
		}
	}
	report(" threads=", uint64(count), " ops=", total, " ns=", elapsed, " collections=", uint64(after.Collections-before.Collections))
	printProfile()
	marker("THREADPROBE_BENCH_OK")
}

// profile and profileLine are globals so that printing the profile
// allocates nothing.
var (
	profile     arena.Diagnostics
	profileLine [160]byte
)

func printProfile() {
	arena.ReadDiagnostics(&profile)
	profile.AppendProfile("threadprobe", profileLine[:], utility.Write)
}
