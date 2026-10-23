package arena

import "testing"

func TestRegionChangeWindowRejectsTornCursorSnapshots(t *testing.T) {
	outer := allocatorPosition{start: 0x1000, next: 0x1800, end: 0x2000, high: 0x1800}
	inner := allocatorPosition{start: 0x3000, next: 0x3400, end: 0x4000, high: 0x3400}
	for _, sample := range []struct {
		name     string
		from, to allocatorPosition
	}{
		{"nested enter", outer, inner},
		{"nested leave", inner, outer},
		{"outer leave", outer, allocatorPosition{}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// LLVM may copy a cursor using individual word stores or SIMD
			// pairs. Every prefix of every word-store order must remain
			// outside the collector's accepted rendezvous states.
			for first := 0; first < 4; first++ {
				for second := 0; second < 4; second++ {
					for third := 0; third < 4; third++ {
						for fourth := 0; fourth < 4; fourth++ {
							order := [4]int{first, second, third, fourth}
							mask := 0
							for _, index := range order {
								mask |= 1 << index
							}
							if mask != 15 {
								continue
							}
							var owner threadAlloc
							cursor := sample.from
							if owner.inWindow() {
								t.Fatal("stable pre-transition state rejected")
							}
							owner.beginRegionChange()
							source := [4]uintptr{sample.to.start, sample.to.next, sample.to.end, sample.to.high}
							target := [4]*uintptr{&cursor.start, &cursor.next, &cursor.end, &cursor.high}
							for _, index := range order {
								*target[index] = source[index]
								if !owner.inWindow() {
									t.Fatalf("collector accepted intermediate cursor %+v", cursor)
								}
							}
							// innermost/depth/enteredBy publication also finishes
							// before the release store, including Leave to zero.
							if !owner.inWindow() || cursor != sample.to {
								t.Fatal("region identity publication window lost")
							}
							owner.endRegionChange()
							if owner.inWindow() {
								t.Fatal("stable post-transition state rejected")
							}
						}
					}
				}
			}
		})
	}
}

func TestRegionChangeWindowSurvivesAllocatorAndUnwinds(t *testing.T) {
	var owner threadAlloc
	func() {
		defer func() { _ = recover() }()
		owner.beginRegionChange()
		defer owner.endRegionChange()
		owner.fast.Store(1)
		owner.fast.Store(0)
		if !owner.inWindow() {
			t.Fatal("allocator window release exposed a region transition")
		}
		panic("test region validation failure")
	}()
	if owner.inWindow() {
		t.Fatal("unwinding left the region transition active")
	}
}

func TestWindowNamesRegionTransitionsBeforeAllocatorWindows(t *testing.T) {
	var owner threadAlloc
	if got := owner.window(); got != windowNone {
		t.Fatalf("idle thread reported window %d", got)
	}
	owner.fast.Store(1)
	if got := owner.window(); got != windowAlloc {
		t.Fatalf("allocator window reported as %d", got)
	}
	owner.beginRegionChange()
	if got := owner.window(); got != windowRegion {
		t.Fatalf("allocator window inside a region transition reported as %d", got)
	}
	owner.fast.Store(0)
	if got := owner.window(); got != windowRegion {
		t.Fatalf("region transition reported as %d", got)
	}
	owner.endRegionChange()
	if got := owner.window(); got != windowNone || owner.inWindow() {
		t.Fatalf("closed windows reported as %d", got)
	}
}
