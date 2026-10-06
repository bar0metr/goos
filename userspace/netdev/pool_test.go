package netdev

import "testing"

func TestPoolResetViews(t *testing.T) {
	var p Pool
	if p.Outstanding() != 0 || !p.Owned(0) {
		t.Fatal("zero pool is not all local")
	}
	p.Reset(false)
	for slot := uint16(0); slot < Slots; slot++ {
		if p.Owned(slot) || p.InUse(slot) {
			t.Fatalf("peer view owns slot %d", slot)
		}
	}
	if p.Outstanding() != Slots {
		t.Fatal("peer view outstanding")
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("acquired a peer slot")
	}
	p.Reset(true)
	if p.Outstanding() != 0 {
		t.Fatal("local view outstanding")
	}
}

func TestPoolTransitions(t *testing.T) {
	type step struct {
		name string
		do   func(*Pool) bool
		want bool
	}
	slot := uint16(2)
	acquire := func(p *Pool) bool { got, ok := p.Acquire(); return ok && got == 0 }
	steps := []step{
		{"release free", func(p *Pool) bool { return p.Release(0) }, false},
		{"give free", func(p *Pool) bool { return p.Give(0) }, false},
		{"take local", func(p *Pool) bool { return p.Take(0) }, false},
		{"acquire", acquire, true},
		{"release in use", func(p *Pool) bool { return p.Release(0) }, true},
		{"release twice", func(p *Pool) bool { return p.Release(0) }, false},
		{"acquire again", acquire, true},
		{"take in use", func(p *Pool) bool { return p.Take(0) }, false},
		{"give", func(p *Pool) bool { return p.Give(0) }, true},
		{"give twice", func(p *Pool) bool { return p.Give(0) }, false},
		{"release peer", func(p *Pool) bool { return p.Release(0) }, false},
		{"owned peer", func(p *Pool) bool { return p.Owned(0) }, false},
		{"outstanding one", func(p *Pool) bool { return p.Outstanding() == 1 }, true},
		{"take back", func(p *Pool) bool { return p.Take(0) }, true},
		{"duplicate take", func(p *Pool) bool { return p.Take(0) }, false},
		{"in use after take", func(p *Pool) bool { return p.InUse(0) }, true},
		{"orphan take", func(p *Pool) bool { return p.Take(slot) }, false},
		{"out of range release", func(p *Pool) bool { return p.Release(Slots) }, false},
		{"out of range give", func(p *Pool) bool { return p.Give(Slots) }, false},
		{"out of range take", func(p *Pool) bool { return p.Take(0xffff) }, false},
		{"out of range owned", func(p *Pool) bool { return p.Owned(Slots) }, false},
		{"out of range in use", func(p *Pool) bool { return p.InUse(Slots) }, false},
	}
	var p Pool
	p.Reset(true)
	for _, s := range steps {
		if got := s.do(&p); got != s.want {
			t.Fatalf("%s: got %v", s.name, got)
		}
	}
}

func TestPoolExhaustion(t *testing.T) {
	var p Pool
	p.Reset(true)
	for want := uint16(0); want < Slots; want++ {
		slot, ok := p.Acquire()
		if !ok || slot != want || !p.Give(slot) {
			t.Fatalf("slot %d: %d %v", want, slot, ok)
		}
	}
	if _, ok := p.Acquire(); ok || p.Outstanding() != Slots {
		t.Fatal("acquired past capacity")
	}
	if !p.Take(1) || !p.Release(1) {
		t.Fatal("return slot 1")
	}
	if slot, ok := p.Acquire(); !ok || slot != 1 {
		t.Fatalf("reacquired %d", slot)
	}
}

// Both views of one pool stay complementary across a full exchange: the
// driver's RX view and the stack's RX view never both own a slot.
func TestPoolPairedViews(t *testing.T) {
	var driver, stack Pool
	driver.Reset(true)
	stack.Reset(false)
	check := func() {
		t.Helper()
		for slot := uint16(0); slot < Slots; slot++ {
			if driver.Owned(slot) == stack.Owned(slot) {
				t.Fatalf("slot %d owned by both or neither", slot)
			}
		}
	}
	for round := 0; round < 3*Slots; round++ {
		slot, ok := driver.Acquire()
		if !ok {
			t.Fatal("driver out of slots")
		}
		// RxReady sent.
		if !driver.Give(slot) || !stack.Take(slot) {
			t.Fatal("handoff")
		}
		check()
		if stack.Take(slot) {
			t.Fatal("duplicate RxReady accepted")
		}
		// RxRelease sent.
		if !stack.Give(slot) || !driver.Take(slot) || !driver.Release(slot) {
			t.Fatal("release")
		}
		check()
		if driver.Take(slot) {
			t.Fatal("duplicate RxRelease accepted")
		}
	}
}
