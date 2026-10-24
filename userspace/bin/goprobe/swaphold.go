//go:build goos_userspace

package main

import (
	"runtime"
	"sync/atomic"

	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
	"goos/userspace/utility"
)

// Instruction holds (ADR 0204). "swaphold" stops a goroutine worker thread
// at every instruction boundary of goos_g_swap and of goos_preempt_entry,
// through the validation kernel's execute breakpoint and single step
// (sys.ThreadHoldRange), and at each stop forces a collection from a
// thread.Start thread while the worker stands there. Heap lists live only
// on the workers' stacks and in their registers; validation sweep poisoning
// turns a list freed while live into a failed check when its goroutine
// reads it again.
//
// One permit runs goroutines, so the held goroutines always run on the main
// thread and every switch of the region happens there; the collector is the
// controller's own native thread.
//
//	swap     two goroutines yield in a loop holding a list each: every
//	         switch goes from one of them to the scheduler stack or back,
//	         so a pass stops either in the outgoing goroutine's save or in
//	         the restore of the goroutine switched to. Passes run until both
//	         directions stopped at every boundary.
//	preempt  one goroutine spins without calls on a list it holds only in
//	         registers until the kernel diverts it to goos_preempt_entry;
//	         the pass steps through the saves, the kernel call, the call
//	         into the runtime (stepped over), and the restore to RET 128.
//
// Each region prints its stops, the distinct RIPs among the stops whose
// collection completed, the expected boundary count, the collections that
// completed at a stop, the stops whose collection gave up, and the lists
// lost, then the offsets of the RIPs with a completed collection, per
// direction for the switch, which tools/swaphold checks against the ELF's
// disassembly. A stop whose collection gave up covers nothing.

const (
	// swapBoundaries and preemptBoundaries are the instruction boundaries
	// a held thread reaches in each range: tools/stackframes pins both
	// sequences, swapInstructions and preemptInstructions. The UD2 after a
	// failed preemption take is never reached.
	swapBoundaries    = 15
	preemptBoundaries = 47
	// holdSpan bounds a range's length in bytes.
	holdSpan = 256
	// holdTimeout bounds every wait for a stop.
	holdTimeout = int64(20e9)
	// maxSwapPasses bounds the passes the swap region takes to stop both
	// directions at every boundary.
	maxSwapPasses = 16
	preemptPasses = 2
	// swapNear is how far below a yielder's recorded stack pointer the
	// switch's first stop lies when the yielder is the outgoing goroutine.
	swapNear = 16 << 10
)

// holdRun is one region's shared state.
type holdRun struct {
	stop    atomic.Uint32
	seed    atomic.Uint64
	lost    atomic.Uint32
	checked atomic.Uint64
	ready   atomic.Uint32
	// worker is the thread the workers run on, and sp the stack pointers
	// the yielders recorded before their last yield.
	worker atomic.Uint64
	sp     [2]atomic.Uint64
	done   chan struct{}
}

// holdRegion is what a region's passes saw.
type holdRegion struct {
	name        string
	lo, hi      uintptr
	expected    int
	held        uint64
	collections uint64
	retried     uint64
	seen        [holdSpan]bool
	// directions counts the boundaries seen per direction of a switch:
	// 0 outgoing goroutine, 1 incoming.
	directions [2][holdSpan]bool
	passes     [2]uint64
}

func (r *holdRegion) distinct(seen *[holdSpan]bool) int {
	count := 0
	for _, hit := range seen {
		if hit {
			count++
		}
	}
	return count
}

func (h *holdRun) nextSeed() uint64 { return h.seed.Add(1 << 16) }

// threadID returns the thread the calling goroutine runs on.
func threadID() uint64 {
	id, _, status := sys.ThreadSelf()
	check(status == kcall.StatusOK, "swaphold: thread self")
	return id
}

// swapHold runs both regions from a controller thread.
//
//go:noinline
func swapHold() {
	procs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(procs)
	poisoned := arena.PoisonedBytes()
	swapLo, swapHi, preemptLo, preemptHi := goroutine.SwitchRanges()
	for _, region := range []string{"swap", "preempt"} {
		run := &holdRun{done: make(chan struct{}, 2)}
		result := &holdRegion{name: region}
		controller := &holdController{run: run, region: result}
		before := arena.HeapStats()
		var d0 arena.Diagnostics
		arena.ReadDiagnostics(&d0)
		workers := 0
		if region == "swap" {
			result.lo, result.hi = swapLo, swapHi
			result.expected = swapBoundaries
			for index := 0; index < 2; index++ {
				go yielder(run, index)
				workers++
			}
		} else {
			result.lo, result.hi = preemptLo, preemptHi
			result.expected = preemptBoundaries
			go spinner(run)
			workers++
		}
		check(result.hi > result.lo && result.hi-result.lo <= holdSpan, "swaphold: range bounds")
		holdControllers[0] = controller
		id, status := thread.Start(holdControl, 0)
		check(status == kcall.StatusOK, "swaphold: controller start")
		code, status := thread.Join(id)
		check(status == kcall.StatusOK && code == 0, "swaphold: controller join")
		holdControllers[0] = nil
		check(controller.failure == "", controller.failure)
		for index := 0; index < workers; index++ {
			<-run.done
		}
		after := arena.HeapStats()
		var d1 arena.Diagnostics
		arena.ReadDiagnostics(&d1)
		l := record("GOPROBE_SWAPHOLD region=").text(region).
			field("held", result.held).field("distinct_rips", uint64(result.distinct(&result.seen))).
			field("expected", uint64(result.expected)).field("collections", result.collections).
			field("retried", result.retried).field("lost", uint64(run.lost.Load())).
			field("checked", run.checked.Load()).
			field("suspend_failures", d1.SuspendFailures-d0.SuspendFailures).
			field("alloc_retries", d1.AllocWindowRetries-d0.AllocWindowRetries).
			field("region_retries", d1.RegionWindowRetries-d0.RegionWindowRetries).
			field("frees", after.Frees-before.Frees)
		if region == "swap" {
			l.field("outgoing", result.passes[0]).field("incoming", result.passes[1])
		}
		appendOffsets(l, " offsets=", &result.seen)
		if region == "swap" {
			appendOffsets(l, " offsets_out=", &result.directions[0])
			appendOffsets(l, " offsets_in=", &result.directions[1])
		}
		l.end()
		check(run.lost.Load() == 0, "swaphold: a live list was freed")
		check(run.checked.Load() > 0, "swaphold: the workers checked no list")
		check(result.distinct(&result.seen) == result.expected, "swaphold: a boundary of the range had no completed collection")
		check(result.collections > 0 && result.collections+result.retried == result.held, "swaphold: a stop ran no collection")
		check(after.Frees > before.Frees, "swaphold: the collections freed nothing")
		if region == "swap" {
			check(result.distinct(&result.directions[0]) == result.expected && result.distinct(&result.directions[1]) == result.expected,
				"swaphold: a switch direction missed a boundary")
		}
	}
	check(arena.PoisonedBytes() > poisoned, "swaphold: sweep poisoned nothing in a validation image")
	ok("GOPROBE_SWAPHOLD_OK")
}

// appendOffsets appends name and the comma-separated offsets set in seen.
func appendOffsets(l *line, name string, seen *[holdSpan]bool) {
	l.text(name)
	first := true
	for offset, hit := range seen {
		if !hit {
			continue
		}
		if !first {
			l.text(",")
		}
		first = false
		l.length = len(utility.AppendUint(l.buffer[:l.length], uint64(offset)))
	}
}

// holdControllers passes the controller its state: a thread.Start function
// takes only a word.
var holdControllers [1]*holdController

type holdController struct {
	run     *holdRun
	region  *holdRegion
	failure string
	// pending is set while the last stop's collection gave up and no later
	// one completed.
	pending bool
}

// holdControl runs a region's passes on its own native thread, then stops
// the workers. It reports a failure through the controller rather than
// failing the process while a worker is held.
func holdControl(uintptr) {
	c := holdControllers[0]
	run, region := c.run, c.region
	deadline := now() + holdTimeout
	for run.ready.Load() == 0 {
		if now() > deadline {
			c.failure = "swaphold: the workers did not start"
			run.stop.Store(1)
			return
		}
		sys.ThreadYield()
	}
	passes := preemptPasses
	if region.name == "swap" {
		passes = maxSwapPasses
	}
	for pass := 0; pass < passes && c.failure == ""; pass++ {
		c.pass()
		if region.name == "swap" && region.distinct(&region.directions[0]) == region.expected &&
			region.distinct(&region.directions[1]) == region.expected {
			break
		}
	}
	if c.failure == "" && c.pending {
		c.collect()
		if c.pending {
			c.failure = "swaphold: no collection completed after the region"
		}
	}
	run.stop.Store(1)
}

// pass arms the region on the workers' thread and steps through it until
// the thread leaves it, collecting at every stop.
func (c *holdController) pass() {
	run, region := c.run, c.region
	worker := run.worker.Load()
	if sys.ThreadHoldRange(worker, region.lo, region.hi) != kcall.StatusOK {
		c.failure = "swaphold: the kernel refused the hold"
		return
	}
	direction := -1
	for {
		stop, ok := c.wait(worker)
		if !ok {
			sys.ThreadHoldCancel(worker)
			return
		}
		if stop.State == kcall.HoldDone {
			return
		}
		if stop.RIP < region.lo || stop.RIP >= region.hi {
			c.failure = "swaphold: a stop lies outside the range"
			sys.ThreadHoldCancel(worker)
			return
		}
		offset := stop.RIP - region.lo
		if direction < 0 {
			direction = 1
			if offset == 0 && region.name == "swap" && c.outgoing(stop.SP) {
				direction = 0
			}
			region.passes[direction]++
		}
		region.held++
		c.collect()
		if c.pending {
			region.retried++
		} else {
			// Only a stop whose collection completed covers its boundary.
			region.collections++
			region.seen[offset] = true
			if region.name == "swap" {
				region.directions[direction][offset] = true
			}
		}
		if !c.step(worker) {
			sys.ThreadHoldCancel(worker)
			return
		}
	}
}

// step runs the worker to its next stop. A stale wake can put the stopped
// worker back on a processor for a moment, and the kernel answers Timeout
// until it stands again, so a Timeout is retried until the deadline.
func (c *holdController) step(worker uint64) bool {
	deadline := now() + holdTimeout
	for {
		switch sys.ThreadHoldStep(worker) {
		case kcall.StatusOK:
			return true
		case kcall.StatusTimeout:
			if now() > deadline {
				c.failure = "swaphold: the held thread did not stand for its step"
				return false
			}
			sys.ThreadYield()
		default:
			c.failure = "swaphold: the kernel refused a step"
			return false
		}
	}
}

// outgoing reports whether a switch's first stop, at sp, is on a yielder's
// stack: the yielder is switching out.
func (c *holdController) outgoing(sp uintptr) bool {
	for index := range c.run.sp {
		recorded := uintptr(c.run.sp[index].Load())
		if sp < recorded && recorded-sp < swapNear {
			return true
		}
	}
	return false
}

// collect forces a collection and records whether it completed.
func (c *holdController) collect() {
	before := arena.HeapStats().Collections
	runtime.GC()
	c.pending = arena.HeapStats().Collections == before
}

// wait polls until the worker stops or leaves the range.
func (c *holdController) wait(worker uint64) (sys.HoldStop, bool) {
	deadline := now() + holdTimeout
	for {
		stop, status := sys.ThreadHoldWait(worker)
		switch status {
		case kcall.StatusOK:
			return stop, true
		case kcall.StatusTimeout:
			if now() > deadline {
				c.failure = "swaphold: the held thread did not stop"
				return stop, false
			}
			sys.ThreadYield()
		default:
			c.failure = "swaphold: the hold was lost"
			return stop, false
		}
	}
}

// yielder yields in a loop, each time holding a list that only its stack
// and registers reach, and checks the list after the yields.
func yielder(run *holdRun, index int) {
	for run.stop.Load() == 0 {
		run.worker.Store(threadID())
		run.sp[index].Store(uint64(goroutine.StackPointer()))
		run.ready.Store(1)
		seed := run.nextSeed()
		if !yieldHolding(buildCells(seed), seed, 4) {
			run.lost.Add(1)
		}
		run.checked.Add(1)
	}
	run.done <- struct{}{}
}

// spinner spins on a list it holds only in registers until the controller
// stops it, then checks the list.
func spinner(run *holdRun) {
	run.worker.Store(threadID())
	seed := run.nextSeed()
	head := buildCells(seed)
	run.ready.Store(1)
	if !spinHolding(head, seed, &run.stop) {
		run.lost.Add(1)
	}
	run.checked.Add(1)
	run.done <- struct{}{}
}

// spinHolding walks the list at head without a call until stop is set, so
// head stays in a register, and checks it after.
//
//go:noinline
func spinHolding(head *cell, seed uint64, stop *atomic.Uint32) bool {
	mix := uint64(0)
	for stop.Load() == 0 {
		for n := head; n != nil; n = n.next {
			mix = mix*6364136223846793005 + n.value
		}
	}
	holdMix = mix
	return cellsIntact(head, seed)
}

// holdMix keeps the spin's work from being optimized away.
var holdMix uint64
