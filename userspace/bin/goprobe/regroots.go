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

// Register roots (ADR 0204). "goprobe regroots" shows, for each register
// alone, that collections keep a list whose only reference a thread holds in
// that register: XMM0 to XMM15, and RAX, RBX, RCX, RDX, RSI, RDI, RBP and
// R8 to R15. sys.ProbeXMMRoot and sys.ProbeGPRRoot put the list's address in
// one register, zero every other register copy, and spin without calls or
// stack writes; their callers keep only the list's token. For each class,
// two situations run:
//
//	thread     a thread.Start thread, which takes no preemption entry: each
//	           collection finds the address in the held thread's snapshot
//	goroutine  a goroutine that takes preemption entries, first on every
//	           permit, where a collection holds it in user code or inside
//	           goos_preempt_entry, then on one permit, where every
//	           collection runs while it is switched out and the address lies
//	           only in the registers goos_preempt_entry saved on its stack
//
// Control runs hold the address in no register: the collections must then
// free the list, which shows that no other root keeps it. Validation images
// poison what a sweep frees, so a freed list reads changed.
const (
	// regCollections is how many collections each hold must span.
	regCollections = 4
	// regControls is how many control runs each situation makes.
	regControls = 4
	// regNowhere is an index that holds the address in no register.
	regNowhere = 255
	// regEntryBudget bounds how long a running goroutine's hold goes on
	// collecting, past regCollections, until a collection finds the
	// spinner inside goos_preempt_entry.
	regEntryBudget = 3 * time.Second
)

// regClass is one register class the probe checks.
type regClass struct {
	name      string
	registers uint64
	probe     func(index, token uint64, state *[2]uint32) uint64
}

// regHold is one spinner's state. It holds the list's token, never its
// address.
type regHold struct {
	// state[0] stops the spinner; the spinner sets state[1] once the
	// register holds the address.
	state [2]uint32
	class *regClass
	index uint64
	token uint64
	seed  uint64
	// kept reports whether the register gave the address back; bad is the
	// first cell regFirstBad found changed.
	kept bool
	bad  int
}

// regTally is one situation's totals.
type regTally struct {
	collections uint64
	lost        uint64
	reclaimed   uint64
	// entries counts the holds a collection found inside
	// goos_preempt_entry.
	entries uint64
}

// regStart starts a spinner on h and returns the function that waits for it
// to end.
type regStart func(h *regHold) func()

// regRoots runs both situations for every register class.
//
//go:noinline
func regRoots(taskLimit int) {
	check(taskLimit > 1, "regroots: task quota cannot hold a native thread")
	if maximum := taskLimit - 1; runtime.GOMAXPROCS(0) > maximum {
		runtime.GOMAXPROCS(maximum)
	}
	poisoned := arena.PoisonedBytes()
	seed := uint64(0x7a3d) << 32
	classes := [...]regClass{
		{"xmm", 16, sys.ProbeXMMRoot},
		{"gpr", 15, sys.ProbeGPRRoot},
	}
	for index := range classes {
		seed = regClassRun(&classes[index], seed)
	}
	check(arena.PoisonedBytes() > poisoned, "regroots: sweep poisoned nothing in a validation image")
	ok("GOPROBE_REGROOTS_OK")
}

// regClassRun checks one register class in both situations and returns the
// next seed.
func regClassRun(c *regClass, seed uint64) uint64 {
	permits := runtime.GOMAXPROCS(0)
	step("regroots " + c.name + " thread")
	var threads regTally
	for index := uint64(0); index < c.registers; index++ {
		seed += 1 << 16
		regHoldRun(&threads, c, "thread", index, seed, regThread, 0)
	}
	for k := 0; k < regControls; k++ {
		seed += 1 << 16
		regControlRun(&threads, c, "thread", seed, regThread)
	}
	record("GOPROBE_REGROOTS").text(" class=").text(c.name).text(" situation=thread").
		field("regs", c.registers).field("collections", threads.collections).field("lost", threads.lost).
		field("controls", regControls).field("reclaimed", threads.reclaimed).end()

	step("regroots " + c.name + " goroutine")
	var g0, g1, g2 goroutine.Stats
	goroutine.ReadStats(&g0)
	var running, switched regTally
	for index := uint64(0); index < c.registers; index++ {
		seed += 1 << 16
		regHoldRun(&running, c, "goroutine-running", index, seed, regGoroutine, regEntryBudget)
	}
	goroutine.ReadStats(&g1)
	// One permit: the collector runs only while the spinner is switched
	// out, so each collection finds the address in the frame
	// goos_preempt_entry saved on the spinner's stack alone.
	runtime.GOMAXPROCS(1)
	for index := uint64(0); index < c.registers; index++ {
		seed += 1 << 16
		regHoldRun(&switched, c, "goroutine-switched", index, seed, regGoroutine, 0)
	}
	goroutine.ReadStats(&g2)
	runtime.GOMAXPROCS(permits)
	var controls regTally
	for k := 0; k < regControls; k++ {
		seed += 1 << 16
		regControlRun(&controls, c, "goroutine", seed, regGoroutine)
	}
	record("GOPROBE_REGROOTS").text(" class=").text(c.name).text(" situation=goroutine").
		field("regs", c.registers).
		field("collections", running.collections+switched.collections).field("lost", running.lost+switched.lost).
		field("running", running.collections).field("switched", switched.collections).
		field("permits", uint64(permits)).
		field("running_visits", g1.PreemptVisits-g0.PreemptVisits).
		field("running_preemptions", g1.Preemptions-g0.Preemptions).
		field("entry_regs", running.entries).
		field("switched_preemptions", g2.Preemptions-g1.Preemptions).
		field("controls", regControls).field("reclaimed", controls.reclaimed).end()

	check(threads.lost == 0, "regroots: a list held only in one register of a thread was freed")
	check(running.lost+switched.lost == 0, "regroots: a list held only in one register of a goroutine was freed")
	check(threads.reclaimed == regControls && controls.reclaimed == regControls,
		"regroots: a list held in no register survived its collections")
	check(g2.Preemptions > g1.Preemptions, "regroots: the spinner was not switched out on one permit")
	return seed
}

// regThread runs the spinner on a thread.Start thread.
func regThread(h *regHold) func() {
	id, status := thread.Start(func(uintptr) { h.spin() }, 0)
	check(status == kcall.StatusOK, "regroots: thread start")
	return func() {
		code, status := thread.Join(id)
		check(status == kcall.StatusOK && code == 0, "regroots: thread join")
	}
}

// regGoroutine runs the spinner on a goroutine.
func regGoroutine(h *regHold) func() {
	done := make(chan struct{})
	go func() {
		h.spin()
		close(done)
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(stressTimeout):
			fail("regroots: the spinner did not stop")
		}
	}
}

// spin runs the spinner and checks the list with the address the register
// gave back still live: once the spinner drops it, only the token remains,
// and any collection may free the list.
//
//go:noinline
func (h *regHold) spin() {
	address := h.class.probe(h.index, h.token, &h.state)
	if h.index >= h.class.registers {
		h.kept = address == 0
		return
	}
	if address != ^h.token {
		h.bad = regFirstBad(h.token, h.seed)
		return
	}
	h.bad = regFirstBad(^address, h.seed)
	// The comparison after the walk keeps address live across it.
	h.kept = address == ^h.token
}

// regHoldRun holds a list in register index of class c across
// regCollections collections and checks it afterwards. With a nonzero entry
// budget it goes on collecting, for up to that long, until a collection
// holds a thread inside goos_preempt_entry: in the running phase only the
// spinner takes preemption entries.
func regHoldRun(t *regTally, c *regClass, situation string, index, seed uint64, start regStart, entry time.Duration) {
	h := &regHold{class: c, index: index, token: regList(seed), seed: seed}
	wait := start(h)
	regAwait(h)
	_, holds := arena.SwitchHolds()
	collections := regCollect(regCollections)
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
			collections += regCollect(1)
		}
	}
	atomic.StoreUint32(&h.state[0], 1)
	wait()
	t.collections += collections
	if !h.kept || h.bad >= 0 {
		t.lost++
		record("GOPROBE_REGROOTS_LOST").text(" class=").text(c.name).text(" situation=").text(situation).
			field("reg", index).field("collections", collections).field("entered", b2u(entered)).
			field("kept", b2u(h.kept)).field("bad_cell", uint64(h.bad+1)).end()
	}
}

// regControlRun runs a spinner that holds the list in no register and
// counts the list as reclaimed when the collections changed it.
func regControlRun(t *regTally, c *regClass, situation string, seed uint64, start regStart) {
	h := &regHold{class: c, index: regNowhere, token: regList(seed), seed: seed}
	wait := start(h)
	regAwait(h)
	collections := regCollect(regCollections)
	reclaimed := peekValue(h.token) != seed
	atomic.StoreUint32(&h.state[0], 1)
	wait()
	t.collections += collections
	check(h.kept, "regroots: a control spinner returned an address")
	if reclaimed {
		t.reclaimed++
	} else {
		record("GOPROBE_REGROOTS_KEPT").text(" class=").text(c.name).text(" situation=").text(situation).
			field("collections", collections).end()
	}
}

// regList builds a list and returns only its token, so no frame that stays
// live holds the list's address.
//
//go:noinline
func regList(seed uint64) uint64 { return arena.Token(buildCells(seed)) }

// regAwait waits until the spinner holds the address.
func regAwait(h *regHold) {
	deadline := now() + int64(stressTimeout)
	for atomic.LoadUint32(&h.state[1]) == 0 {
		check(now() < deadline, "regroots: the spinner did not start")
		runtime.Gosched()
	}
}

// regCollect runs collections until count of them complete and returns how
// many completed.
func regCollect(count uint64) uint64 {
	before := arena.HeapStats().Collections
	deadline := now() + int64(stressTimeout)
	for {
		churn(2)
		runtime.GC()
		if done := uint64(arena.HeapStats().Collections - before); done >= count {
			return done
		}
		check(now() < deadline, "regroots: collections did not complete")
		runtime.Gosched()
	}
}

// regFirstBad returns the index of the first cell of the list a token names
// that differs from what buildCells made from seed, stressLength for a list
// that runs on, or -1 for an intact list. It reads words, not pointers, and
// each cell's check before its next word, so a poisoned cell is reported,
// not followed.
//
//go:noinline
func regFirstBad(token, seed uint64) int {
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
