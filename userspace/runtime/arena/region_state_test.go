package arena

import "testing"

func expectPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
func TestRegionRestoresOuterAllocator(t *testing.T) {
	outer := allocatorPosition{start: 100, next: 200, end: 1000, high: 300}
	region := regionPosition{position: allocatorPosition{start: 400, next: 400, end: 500, high: 400}}
	stack := regionStack{}
	active, scope := enterRegion(outer, &region, &stack)
	active.next = 440
	active.high = 450
	got := leaveRegion(active, scope, &stack)
	if got != outer || region.position.next != 440 || stack.depth != 0 {
		t.Fatalf("restore: %+v %+v", got, region)
	}
	active, scope = enterRegion(got, &region, &stack)
	if active.next != 440 {
		t.Fatal("region allocations did not persist")
	}
	leaveRegion(active, scope, &stack)
}
func TestRegionsEnforceLIFOAndNoReentry(t *testing.T) {
	outer := allocatorPosition{start: 1, next: 2, end: 100, high: 2}
	a := regionPosition{position: allocatorPosition{start: 10, next: 10, end: 20, high: 10}}
	b := regionPosition{position: allocatorPosition{start: 30, next: 30, end: 40, high: 30}}
	stack := regionStack{}
	pa, sa := enterRegion(outer, &a, &stack)
	expectPanic(t, func() { enterRegion(pa, &a, &stack) })
	pb, sb := enterRegion(pa, &b, &stack)
	expectPanic(t, func() { leaveRegion(pb, sa, &stack) })
	pa = leaveRegion(pb, sb, &stack)
	restored := leaveRegion(pa, sa, &stack)
	if restored != outer {
		t.Fatal("outer position changed")
	}
	expectPanic(t, func() { leaveRegion(pa, sa, &stack) })
}
func TestRegionRejectsMalformedCursor(t *testing.T) {
	stack := regionStack{}
	r := regionPosition{position: allocatorPosition{start: 100, next: 301, end: 300}}
	expectPanic(t, func() { enterRegion(allocatorPosition{}, &r, &stack) })
}
