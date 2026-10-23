//go:build goos_userspace

package main

import (
	"runtime"
	"sync/atomic"

	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
	"goos/userspace/utility"
)

// Region transition checks (ADR 0204). "region-window N ROUNDS" starts N
// workers. Each round one of them, the mover, enters an outer region, then
// an inner one, and leaves the inner one; the validation hold stops it
// inside both transitions, after its cursor store and before its innermost
// store. While it is stopped there, the round's collector, the main thread
// or another worker, asks for collections: each must hold the mover, see
// its open region window, let it go, and give up without marking or
// sweeping. Once the mover is released the next collection must run.
//
// The mover keeps one list only on its stack, outer lists only in holders
// inside the outer region, and inner lists only in holders inside the inner
// region. A collection that went ahead while the mover was stopped would
// scan the outer region over the inner one's range on entry, and the inner
// region over the outer one's range on exit, and sweep those lists; the
// collector then allocates lists of the same sizes, which would reuse their
// slots. Every list must still hold its values afterwards. Once the inner
// region is cleared, its lists must be swept.
const (
	windowLists  = 8
	windowLength = 32
	// windowRejections is how many collections the collector asks for
	// while the mover is stopped in each transition.
	windowRejections = 2
	// windowNodeFloor is the least a list node takes in the heap: a
	// 40-byte node in a 48-byte slot and at least 16 bytes of data. A
	// cleared inner region must give back at least half of its lists:
	// conservative scanning may keep a few alive through stale words.
	windowNodeFloor    = 64
	windowReclaimFloor = windowLists * windowLength * windowNodeFloor / 2
	// windowTimeout bounds every wait for the other side, in nanoseconds.
	windowTimeout = 30_000_000_000
)

// The steps of one round. The mover and the collector take turns: each
// waits for the step the other stores.
const (
	stepEntered    = 1 // the mover finished Enter and filled the inner region
	stepCollected  = 2 // a collection ran with the inner region entered
	stepLeft       = 3 // the mover finished Leave and checked every list
	stepMeasured   = 4 // a collection ran and the live bytes were read
	stepCleared    = 5 // the mover cleared the inner region
	stepReclaimed  = 6 // a collection swept the inner region's lists
	stepRoundEnded = 7 // the mover left the outer region
)

var (
	// windowRound is the round that runs, from one; the main thread stores
	// the round's mover and collector before it.
	windowRound     atomic.Uint32
	windowStep      atomic.Uint32
	windowMover     atomic.Int32
	windowCollector atomic.Int32
	windowStop      atomic.Bool
	windowReady     atomic.Uint32
	// Each worker acknowledges the round before the main thread changes
	// its roles or resets its steps, including workers with no role.
	windowDone [maxWorkers]atomic.Uint32
	// What the rounds counted, for the report.
	windowRejected, windowRetries, windowCollections atomic.Uint64
	windowReclaimed, windowPeerRounds, windowChecks  atomic.Uint64
	windowPeerCPUOverlaps                            atomic.Uint64
	// windowBefore and windowAfter belong to the round's collector.
	windowBefore, windowAfter arena.Diagnostics
)

func windowSeed(round uint32, mover int, kind, list int) uint64 {
	return uint64(round)<<32 | uint64(mover+1)<<24 | uint64(kind)<<16 | uint64(list)<<8 | 5
}

// awaitStep waits until the other side stores step, or ends the process.
// It allocates nothing, so a collector's measurement of the live bytes
// spans the wait.
func awaitStep(step uint32) {
	deadline := utility.ClockNanos() + windowTimeout
	for windowStep.Load() < step {
		expect(utility.ClockNanos() < deadline, "region window: the other side of the round")
		thread.Yield()
	}
}

// awaitHeld waits until the mover stops inside its transition.
func awaitHeld() {
	deadline := utility.ClockNanos() + windowTimeout
	for !arena.RegionTransitionHeld() {
		expect(utility.ClockNanos() < deadline, "region window: the mover stopped inside its transition")
		thread.Yield()
	}
}

// windowWorker serves rounds until the main thread stops them. Its regions
// last the whole run, so every round reuses them after a Clear.
func windowWorker(argument uintptr) {
	index := int32(argument)
	outer := arena.NewRegion(64 << 10)
	inner := arena.NewRegion(64 << 10)
	seen := uint32(0)
	windowReady.Add(1)
	for {
		round := windowRound.Load()
		if round == seen {
			if windowStop.Load() {
				break
			}
			thread.Yield()
			continue
		}
		seen = round
		switch index {
		case windowMover.Load():
			moveRound(round, int(index), outer, inner)
		case windowCollector.Load():
			collectRound()
		}
		windowDone[index].Store(round)
	}
	outer.Release()
	inner.Release()
}

// checkHolders reports whether every holder still leads to its list.
//
//go:noinline
func checkHolders(holders *[windowLists]*holder, round uint32, mover, kind int) bool {
	for index, h := range holders {
		if h == nil || !listHolds(h.list, windowSeed(round, mover, kind, index), windowLength) {
			return false
		}
	}
	windowChecks.Add(windowLists)
	return true
}

// windowHeads carries lists from a retired builder frame into the regions.
// Atomic swaps prevent LLVM from removing the clears as dead stores and
// leaving heap addresses in the mover's live frame through inner.Clear.
type windowHeads [2 * windowLists]atomic.Pointer[node]

//go:noinline
func seedWindow(round uint32, mover int) *windowHeads {
	heads := new(windowHeads)
	for kind := 0; kind < 2; kind++ {
		for index := range windowLists {
			heads[kind*windowLists+index].Store(buildList(windowSeed(round, mover, kind, index), windowLength))
		}
	}
	return heads
}

// placeHolders runs inside a region. It leaves only region holders pointing
// to the lists, with no direct list address returned to its caller.
//
//go:noinline
func placeHolders(heads *windowHeads, kind int) *[windowLists]*holder {
	holders := new([windowLists]*holder)
	for index := range windowLists {
		holders[index] = &holder{list: heads[kind*windowLists+index].Swap(nil)}
	}
	return holders
}

// innerRound returns before the mover publishes stepCleared. Otherwise
// conservative scanning would retain the old heads LLVM spilled in this
// frame, even after the inner region's used range became empty.
//
//go:noinline
func innerRound(round uint32, mover int, inner *arena.Region, heads *windowHeads, outerHolders *[windowLists]*holder) {
	arena.HoldRegionTransition()
	innerScope := inner.Enter()
	innerHolders := placeHolders(heads, 1)
	expect(inner.Used() >= windowLists*16, "region window: holders allocated in the inner region")
	windowStep.Store(stepEntered)
	awaitStep(stepCollected)
	expect(checkHolders(outerHolders, round, mover, 0), "region window: outer lists after a held Enter")
	expect(checkHolders(innerHolders, round, mover, 1), "region window: inner lists after a held Enter")

	arena.HoldRegionTransition()
	innerScope.Leave()
	expect(checkHolders(innerHolders, round, mover, 1), "region window: inner lists after a held Leave")
	expect(checkHolders(outerHolders, round, mover, 0), "region window: outer lists after a held Leave")
	windowStep.Store(stepLeft)

	awaitStep(stepMeasured)
	inner.Clear()
	expect(inner.Used() == 0, "region window: the inner region cleared")
}

// moveRound owns only outer-region addresses and the stack's list. The
// inner list builder and checker frames retire before the reclamation GC.
//
//go:noinline
func moveRound(round uint32, mover int, outer, inner *arena.Region) {
	heads := seedWindow(round, mover)
	stackList := buildList(windowSeed(round, mover, 2, 0), windowLength)
	outerScope := outer.Enter()
	outerHolders := placeHolders(heads, 0)
	expect(outer.Used() >= windowLists*16, "region window: holders allocated in the outer region")
	innerRound(round, mover, inner, heads, outerHolders)
	expect(listHolds(stackList, windowSeed(round, mover, 2, 0), windowLength), "region window: the stack's list")
	windowStep.Store(stepCleared)

	awaitStep(stepReclaimed)
	expect(checkHolders(outerHolders, round, mover, 0), "region window: outer lists after the inner region was swept")
	expect(listHolds(stackList, windowSeed(round, mover, 2, 0), windowLength), "region window: the stack's list at the end")
	outerScope.Leave()
	outer.Clear()
	windowStep.Store(stepRoundEnded)
}

// churn allocates lists of the sizes the mover's lists have and drops them,
// so a slot a wrong collection freed is written again.
//
//go:noinline
func churn() {
	for index := range 2 * windowLists {
		buildList(uint64(index)<<8|0xee, windowLength)
	}
}

// rejectHeld asks for collections while the mover waits inside a region
// transition. Each must give up: no collection, no hold kept, and at least
// one region-window retry per request.
func rejectHeld() {
	awaitHeld()
	// A CPURuntime sample from this collector identifies the mover on a
	// different CPU while the validation hook keeps its window open.
	mover := ourTasks[int(windowMover.Load())+1].Load()
	for cpu := uint64(0); cpu < 64; cpu++ {
		wire := &cpuWires[thread.Slot()]
		if sys.CPURuntime(cpu, wire) != kcall.StatusOK {
			break
		}
		if state, ok := kcall.DecodeCPURuntime(wire[:]); ok && state.Online && state.Current == mover {
			if thread.Slot() != 0 {
				windowPeerCPUOverlaps.Add(1)
			}
			break
		}
	}
	collections := arena.HeapStats().Collections
	arena.ReadDiagnostics(&windowBefore)
	previousRetries := windowBefore.RegionWindowRetries
	for range windowRejections {
		runtime.GC()
		arena.ReadDiagnostics(&windowAfter)
		expect(windowAfter.RegionWindowRetries > previousRetries, "region window: each request saw the open window")
		previousRetries = windowAfter.RegionWindowRetries
	}
	arena.ReadDiagnostics(&windowAfter)
	expect(arena.HeapStats().Collections == collections, "region window: no collection while the window was open")
	expect(windowAfter.Suspensions == windowBefore.Suspensions, "region window: no hold was kept while the window was open")
	expect(windowAfter.SuspendFailures-windowBefore.SuspendFailures == windowRejections, "region window: every request gave up")
	retries := windowAfter.RegionWindowRetries - windowBefore.RegionWindowRetries
	expect(retries >= windowRejections, "region window: the collector saw the open region window")
	windowRejected.Add(windowRejections)
	windowRetries.Add(retries)
	churn()
	expect(arena.RegionTransitionHeld(), "region window: the mover stayed inside its transition")
	expect(arena.ReleaseRegionTransition(), "region window: the mover was released")
}

// collectOnce asks for one collection with the mover outside every
// transition window. It must run.
func collectOnce() {
	collections := arena.HeapStats().Collections
	arena.ReadDiagnostics(&windowBefore)
	runtime.GC()
	arena.ReadDiagnostics(&windowAfter)
	expect(arena.HeapStats().Collections == collections+1, "region window: a collection ran once the window closed")
	expect(windowAfter.RegionWindowRetries == windowBefore.RegionWindowRetries, "region window: no region retry with the window closed")
	windowCollections.Add(1)
}

// collectRound is the collector's side of a round.
//
//go:noinline
func collectRound() {
	rejectHeld()
	awaitStep(stepEntered)
	collectOnce()
	windowStep.Store(stepCollected)

	rejectHeld()
	awaitStep(stepLeft)
	collectOnce()
	held := arena.HeapStats().Live
	windowStep.Store(stepMeasured)

	awaitStep(stepCleared)
	collectOnce()
	live := arena.HeapStats().Live
	if held <= live || held-live < windowReclaimFloor {
		report("THREADPROBE_REGION_WINDOW_RECLAIM held=", held, " live=", live, " round=", uint64(windowRound.Load()), "", 0)
	}
	expect(held > live && held-live >= windowReclaimFloor, "region window: the cleared inner region's lists were swept")
	windowReclaimed.Add(held - live)
	windowStep.Store(stepReclaimed)
}

func regionWindow(count, rounds int) {
	expect(count >= 1 && count <= maxWorkers && rounds >= 1, "arguments")
	var ids [maxWorkers]thread.ID
	for index := 0; index < count; index++ {
		id, status := thread.Start(windowWorker, uintptr(index))
		expect(status == kcall.StatusOK, "thread start")
		ids[index] = id
		ourTasks[index+1].Store(uint64(id))
	}
	ourTasks[0].Store(uint64(thread.Self()))
	deadline := utility.ClockNanos() + windowTimeout
	for windowReady.Load() != uint32(count) {
		expect(utility.ClockNanos() < deadline, "region window: workers initialized")
		thread.Yield()
	}
	self := int32(count)
	for round := 1; round <= rounds; round++ {
		mover := int32((round - 1) % count)
		collector := int32(((round-1)/count + int(mover) + 1) % (count + 1))
		if collector == mover {
			collector = self
		}
		if collector != self {
			windowPeerRounds.Add(1)
		}
		windowStep.Store(0)
		windowMover.Store(mover)
		windowCollector.Store(collector)
		windowRound.Store(uint32(round))
		if collector == self {
			collectRound()
		}
		awaitStep(stepRoundEnded)
		deadline := utility.ClockNanos() + windowTimeout
		for index := 0; index < count; index++ {
			for windowDone[index].Load() != uint32(round) {
				expect(utility.ClockNanos() < deadline, "region window: round acknowledgments")
				thread.Yield()
			}
		}
	}
	windowStop.Store(true)
	for index := 0; index < count; index++ {
		code, status := thread.Join(ids[index])
		expect(status == kcall.StatusOK && code == 0, "join")
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
	if count > 1 && processors > 1 {
		expect(windowPeerCPUOverlaps.Load() > 0, "region window: a peer collector observed the mover on another CPU")
		expect(windowPeerRounds.Load() > 0, "region window: a worker collected its peer")
	}
	arena.ReadDiagnostics(&profile)
	report("THREADPROBE_REGION_WINDOW threads=", uint64(count), " rounds=", uint64(rounds),
		" rejected=", windowRejected.Load(), " region_retries=", windowRetries.Load())
	report("THREADPROBE_REGION_WINDOW collections=", windowCollections.Load(), " peer_rounds=", windowPeerRounds.Load(),
		" checked_lists=", windowChecks.Load(), " reclaimed=", windowReclaimed.Load())
	report("THREADPROBE_REGION_WINDOW peer_cpu_overlaps=", windowPeerCPUOverlaps.Load(), "", 0, "", 0, "", 0)
	report("THREADPROBE_REGION_WINDOW alloc_retries=", profile.AllocWindowRetries, " suspend_failures=", profile.SuspendFailures,
		" suspensions=", profile.Suspensions, "", 0)
	expect(windowRejected.Load() == uint64(2*windowRejections*rounds), "region window: every held transition was refused")
	expect(windowCollections.Load() == uint64(3*rounds), "region window: every closed window let a collection run")
	expect(windowChecks.Load() == uint64(5*windowLists*rounds), "region window: every holder's list was checked")
	report("THREADPROBE_REGION_WINDOW_OK rounds=", uint64(rounds), " rejected=", windowRejected.Load(),
		" region_retries=", windowRetries.Load(), " collections=", windowCollections.Load())
}
