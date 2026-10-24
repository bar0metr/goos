//go:build goos_userspace

package main

import (
	"runtime"
	"sync/atomic"
	"time"

	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/memory"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
)

// XMM roots (ADR 0204). "goprobe xmmroots" shows, for each of XMM0 to XMM15
// alone, that collections keep a list whose only reference a thread holds in
// that register. sys.ProbeXMMRoot puts the list's address in one register,
// zeroes every other register copy, and spins without calls or stack
// writes; its callers keep only the list's token. Two situations run:
//
//	thread     a thread.Start thread, which takes no preemption entry: each
//	           collection finds the address in the held thread's vector
//	           snapshot words
//	goroutine  a goroutine that takes preemption entries, first on every
//	           permit, where a collection holds it in user code or inside
//	           goos_preempt_entry, then on one permit, where every
//	           collection runs while it is switched out and the address lies
//	           only in the FXSAVE image on its stack
//
// Control runs hold the address in no register: the collections must then
// free the list, which shows that no other root keeps it. Validation images
// poison what a sweep frees, so a freed list reads changed.
const (
	xmmRegisters = 16
	// xmmCollections is how many collections each hold must span.
	xmmCollections = 4
	// xmmControls is how many control runs each situation makes.
	xmmControls = 4
	// xmmNowhere is the index that holds the address in no register.
	xmmNowhere = xmmRegisters
	// xmmEntryBudget bounds how long a running goroutine's hold goes on
	// collecting, past xmmCollections, until a collection finds the
	// spinner inside goos_preempt_entry.
	xmmEntryBudget = 5 * time.Second
)

// xmmHold is one spinner's state. It holds the list's token, never its
// address.
type xmmHold struct {
	// state[0] stops the spinner; the spinner sets state[1] once the
	// register holds the address.
	state [2]uint32
	index uint64
	token uint64
	seed  uint64
	// kept reports whether the register gave the address back; bad is the
	// first cell xmmFirstBad found changed.
	kept bool
	bad  int
}

// xmmTally is one situation's totals.
type xmmTally struct {
	collections uint64
	lost        uint64
	reclaimed   uint64
	// entries counts the holds a collection found inside
	// goos_preempt_entry.
	entries uint64
}

// xmmStart starts a spinner on h and returns the function that waits for it
// to end.
type xmmStart func(h *xmmHold) func()

// xmmRoots runs both situations.
//
//go:noinline
func xmmRoots(taskLimit int) {
	check(taskLimit > 1, "xmmroots: task quota cannot hold a native thread")
	if maximum := taskLimit - 1; runtime.GOMAXPROCS(0) > maximum {
		runtime.GOMAXPROCS(maximum)
	}
	permits := runtime.GOMAXPROCS(0)
	seed := uint64(0x7a3d) << 32
	poisoned := arena.PoisonedBytes()

	step("xmmroots thread")
	var threads xmmTally
	for index := uint64(0); index < xmmRegisters; index++ {
		seed += 1 << 16
		xmmHoldRun(&threads, "thread", index, seed, xmmThread, 0)
	}
	for k := 0; k < xmmControls; k++ {
		seed += 1 << 16
		xmmControlRun(&threads, "thread", seed, xmmThread)
	}
	record("GOPROBE_XMMROOTS").text(" situation=thread").field("regs", xmmRegisters).
		field("collections", threads.collections).field("lost", threads.lost).
		field("controls", xmmControls).field("reclaimed", threads.reclaimed).end()

	step("xmmroots goroutine")
	var g0, g1, g2 goroutine.Stats
	goroutine.ReadStats(&g0)
	var running, switched xmmTally
	for index := uint64(0); index < xmmRegisters; index++ {
		seed += 1 << 16
		xmmHoldRun(&running, "goroutine-running", index, seed, xmmGoroutine, xmmEntryBudget)
	}
	goroutine.ReadStats(&g1)
	// One permit: the collector runs only while the spinner is switched
	// out, so each collection finds the address in the spinner's FXSAVE
	// image alone.
	runtime.GOMAXPROCS(1)
	for index := uint64(0); index < xmmRegisters; index++ {
		seed += 1 << 16
		xmmHoldRun(&switched, "goroutine-switched", index, seed, xmmGoroutine, 0)
	}
	goroutine.ReadStats(&g2)
	runtime.GOMAXPROCS(permits)
	var controls xmmTally
	for k := 0; k < xmmControls; k++ {
		seed += 1 << 16
		xmmControlRun(&controls, "goroutine", seed, xmmGoroutine)
	}
	record("GOPROBE_XMMROOTS").text(" situation=goroutine").field("regs", xmmRegisters).
		field("collections", running.collections+switched.collections).field("lost", running.lost+switched.lost).
		field("running", running.collections).field("switched", switched.collections).
		field("permits", uint64(permits)).
		field("running_visits", g1.PreemptVisits-g0.PreemptVisits).
		field("running_preemptions", g1.Preemptions-g0.Preemptions).
		field("entry_regs", running.entries).
		field("switched_preemptions", g2.Preemptions-g1.Preemptions).
		field("controls", xmmControls).field("reclaimed", controls.reclaimed).end()

	check(threads.lost == 0, "xmmroots: a list held only in an XMM register of a thread was freed")
	check(running.lost+switched.lost == 0, "xmmroots: a list held only in an XMM register of a goroutine was freed")
	check(threads.reclaimed == xmmControls && controls.reclaimed == xmmControls,
		"xmmroots: a list held in no register survived its collections")
	check(g2.Preemptions > g1.Preemptions, "xmmroots: the spinner was not switched out on one permit")
	check(arena.PoisonedBytes() > poisoned, "xmmroots: sweep poisoned nothing in a validation image")
	ok("GOPROBE_XMMROOTS_OK")
}

// xmmThread runs the spinner on a thread.Start thread.
func xmmThread(h *xmmHold) func() {
	id, status := thread.Start(func(uintptr) { h.spin() }, 0)
	check(status == kcall.StatusOK, "xmmroots: thread start")
	return func() {
		code, status := thread.Join(id)
		check(status == kcall.StatusOK && code == 0, "xmmroots: thread join")
	}
}

// xmmGoroutine runs the spinner on a goroutine.
func xmmGoroutine(h *xmmHold) func() {
	done := make(chan struct{})
	go func() {
		h.spin()
		close(done)
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(stressTimeout):
			fail("xmmroots: the spinner did not stop")
		}
	}
}

// spin runs the spinner and checks the list with the address the register
// gave back still live: once the spinner drops it, only the token remains,
// and any collection may free the list.
//
//go:noinline
func (h *xmmHold) spin() {
	address := sys.ProbeXMMRoot(h.index, h.token, &h.state)
	if h.index >= xmmNowhere {
		h.kept = address == 0
		return
	}
	if address != ^h.token {
		h.bad = xmmFirstBad(h.token, h.seed)
		return
	}
	h.bad = xmmFirstBad(^address, h.seed)
	// The comparison after the walk keeps address live across it.
	h.kept = address == ^h.token
}

// xmmHoldRun holds a list in XMM[index] across xmmCollections collections
// and checks it afterwards. With a nonzero entry budget it goes on
// collecting, for up to that long, until a collection holds a thread inside
// goos_preempt_entry: in the running phase only the spinner takes
// preemption entries.
func xmmHoldRun(t *xmmTally, situation string, index, seed uint64, start xmmStart, entry time.Duration) {
	h := &xmmHold{index: index, token: xmmList(seed), seed: seed}
	wait := start(h)
	xmmAwait(h)
	_, holds := arena.SwitchHolds()
	collections := xmmCollect(xmmCollections)
	entered := false
	if entry > 0 {
		deadline := now() + int64(entry)
		for {
			if _, after := arena.SwitchHolds(); after != holds {
				t.entries++
				entered = true
				break
			}
			if now() >= deadline {
				break
			}
			collections += xmmCollect(1)
		}
	}
	atomic.StoreUint32(&h.state[0], 1)
	wait()
	t.collections += collections
	if !h.kept || h.bad >= 0 {
		t.lost++
		record("GOPROBE_XMMROOTS_LOST").text(" situation=").text(situation).field("reg", index).
			field("collections", collections).field("entered", b2u(entered)).
			field("kept", b2u(h.kept)).field("bad_cell", uint64(h.bad+1)).end()
	}
}

// xmmControlRun runs a spinner that holds the list in no register and
// counts the list as reclaimed when the collections changed it.
func xmmControlRun(t *xmmTally, situation string, seed uint64, start xmmStart) {
	h := &xmmHold{index: xmmNowhere, token: xmmList(seed), seed: seed}
	wait := start(h)
	xmmAwait(h)
	collections := xmmCollect(xmmCollections)
	reclaimed := peekValue(h.token) != seed
	atomic.StoreUint32(&h.state[0], 1)
	wait()
	t.collections += collections
	check(h.kept, "xmmroots: a control spinner returned an address")
	if reclaimed {
		t.reclaimed++
	} else {
		record("GOPROBE_XMMROOTS_KEPT").text(" situation=").text(situation).field("collections", collections).end()
	}
}

// xmmList builds a list and returns only its token, so no frame that stays
// live holds the list's address.
//
//go:noinline
func xmmList(seed uint64) uint64 { return arena.Token(buildCells(seed)) }

// xmmAwait waits until the spinner holds the address.
func xmmAwait(h *xmmHold) {
	deadline := now() + int64(stressTimeout)
	for atomic.LoadUint32(&h.state[1]) == 0 {
		check(now() < deadline, "xmmroots: the spinner did not start")
		runtime.Gosched()
	}
}

// xmmCollect runs collections until count of them complete and returns how
// many completed.
func xmmCollect(count uint64) uint64 {
	before := arena.HeapStats().Collections
	deadline := now() + int64(stressTimeout)
	for {
		churn(2)
		runtime.GC()
		if done := uint64(arena.HeapStats().Collections - before); done >= count {
			return done
		}
		check(now() < deadline, "xmmroots: collections did not complete")
		runtime.Gosched()
	}
}

// xmmFirstBad returns the index of the first cell of the list a token names
// that differs from what buildCells made from seed, stressLength for a list
// that runs on, or -1 for an intact list. It reads words, not pointers, and
// each cell's check before its next word, so a poisoned cell is reported,
// not followed.
//
//go:noinline
func xmmFirstBad(token, seed uint64) int {
	address := uintptr(^token)
	for i := 0; i < stressLength; i++ {
		v := seed + uint64(i)
		if address == 0 || memory.Word(address+cellValueOffset) != v || memory.Word(address+cellValueOffset+8) != v^stressMagic {
			return i
		}
		address = uintptr(memory.Word(address))
	}
	if address != 0 {
		return stressLength
	}
	return -1
}

func b2u(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
