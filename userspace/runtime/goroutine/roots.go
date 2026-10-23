package goroutine

import "goos/userspace/runtime/tls"

// The collector's view of the stacks (ADR 0204). While every other thread is
// held, plan decides what the mark scans, from the threads' stack pointers
// and the tables alone: it takes no lock, since a held thread may hold any
// of them. Each word plan reads is written with one store, in an order that
// keeps a held thread's state readable at every instruction:
//
//   - A goroutine is published runnable only after its stack and saved
//     pointer are set, and is marked dead before its stack is freed.
//   - A goroutine's saved pointer is written by the switch before the
//     thread leaves its stack, so a goroutine that runs on no held thread's
//     stack pointer has a valid one.
//   - A thread's scheduler-stack pointer is saved before it switches to a
//     goroutine, and the frames below it stay until it switches back.
//
// A thread's stack pointer that lies in no stack the tables or the kernel's
// layout name fails the plan: the collection is given up rather than run
// on roots it cannot see.

type stackRange struct{ lo, hi uintptr }

// HeldThread is a thread's slot and stack pointer.
type HeldThread struct {
	Slot int
	SP   uintptr
}

// redZone is how far below a held thread's stack pointer a leaf function
// may keep values.
const redZone = 128

var plan struct {
	ranges  [MaxGoroutines + 2*tls.Slots]stackRange
	count   int
	covered [MaxGoroutines]bool
}

func addRange(lo, hi uintptr) bool {
	if lo >= hi {
		return true
	}
	if plan.count == len(plan.ranges) {
		return false
	}
	plan.ranges[plan.count] = stackRange{lo, hi}
	plan.count++
	return true
}

// locate finds the stack that holds sp on the thread in slot: the
// thread's scheduler stack, a goroutine's, or, for a thread the runtime does
// not know yet, the stack the kernel made for it. It returns the stack's
// bounds and the goroutine's index, or -1.
func locate(slot int, sp uintptr, native func(int) (uintptr, uintptr)) (uintptr, uintptr, int, bool) {
	if slot < 0 || slot >= tls.Slots {
		return 0, 0, -1, false
	}
	m := &ms[slot]
	if m.kind.Load() != mNone && sp >= m.sysLo && sp <= m.sysHi {
		return m.sysLo, m.sysHi, -1, true
	}
	for index := range gs {
		g := &gs[index]
		status := g.status.Load()
		if status == gFree || status == gDead {
			continue
		}
		if sp >= g.lo && sp <= g.hi {
			return g.lo, g.hi, index, true
		}
	}
	lo, hi := native(slot)
	if sp >= lo && sp <= hi {
		return lo, hi, -1, true
	}
	return 0, 0, -1, false
}

// planStacks fills plan for a collection that self runs while held are
// held. The collector's own stack is left out: it scans that from its own
// frame. It reports false when a stack pointer lies in no known stack, or a
// goroutine's saved pointer lies outside its stack.
func planStacks(self HeldThread, held []HeldThread, native func(int) (uintptr, uintptr)) bool {
	plan.count = 0
	for index := range plan.covered {
		plan.covered[index] = false
	}
	_, _, gIndex, ok := locate(self.Slot, self.SP, native)
	if !ok {
		return false
	}
	if gIndex >= 0 {
		plan.covered[gIndex] = true
	}
	for index := range held {
		t := held[index]
		lo, hi, gIndex, ok := locate(t.Slot, t.SP, native)
		if !ok {
			return false
		}
		if gIndex >= 0 {
			plan.covered[gIndex] = true
		}
		from := lo
		if t.SP-redZone > lo && t.SP > redZone {
			from = t.SP - redZone
		}
		if !addRange(from, hi) {
			return false
		}
	}
	for slot := range ms {
		m := &ms[slot]
		if m.kind.Load() == mNone || m.curG == nil {
			continue
		}
		// The thread runs a goroutine: its scheduler stack keeps the
		// frames that switched to it.
		from := m.sysSP
		if from < m.sysLo || from > m.sysHi {
			from = m.sysLo
		}
		if !addRange(from, m.sysHi) {
			return false
		}
	}
	for index := range gs {
		g := &gs[index]
		status := g.status.Load()
		if status == gFree || status == gDead || plan.covered[index] {
			continue
		}
		if g.sp < g.lo || g.sp > g.hi {
			return false
		}
		if !addRange(g.sp, g.hi) {
			return false
		}
	}
	return true
}

// markPlanned marks every range plan chose.
func markPlanned(mark func(from, to uintptr)) {
	for index := 0; index < plan.count; index++ {
		mark(plan.ranges[index].lo, plan.ranges[index].hi)
	}
}
