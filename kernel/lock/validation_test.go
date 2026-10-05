//go:build goos_validation

package lock

import "testing"

func expectFatal(t *testing.T, name string, run func()) {
	t.Helper()
	var message string
	Install(Hooks{Fatal: func(text string) { message = text; panic(text) }})
	defer Install(Hooks{})
	defer func() {
		// The panic left whatever run took recorded as held.
		for index := range held {
			held[index].Store(0)
			heldCount[index] = [levelCount]uint8{}
		}
		if recover() == nil || message == "" {
			t.Fatalf("%s was accepted", name)
		}
	}()
	run()
}

func TestRecursiveAcquisitionIsFatal(t *testing.T) {
	spin := New(LevelKernel)
	expectFatal(t, "recursive acquisition", func() {
		spin.Lock()
		spin.Lock()
	})
}

func TestOutOfOrderAcquisitionIsFatal(t *testing.T) {
	scheduler := New(LevelScheduler)
	kernel := New(LevelKernel)
	expectFatal(t, "kernel lock under the scheduler lock", func() {
		scheduler.Lock()
		kernel.Lock()
	})
}

func TestInOrderAcquisitionAndOwnership(t *testing.T) {
	Install(Hooks{})
	kernel, memory, scheduler := New(LevelKernel), New(LevelMemory), New(LevelScheduler)
	kernel.Lock()
	memory.Lock()
	scheduler.Lock()
	if !kernel.HeldByCaller() || !scheduler.HeldByCaller() {
		t.Fatal("holder not recorded")
	}
	scheduler.Unlock()
	memory.Unlock()
	kernel.Unlock()
	if kernel.HeldByCaller() || held[0].Load() != 0 {
		t.Fatal("release not recorded")
	}
	// A lower level is free again once the higher one is released.
	scheduler.Lock()
	scheduler.Unlock()
	kernel.Lock()
	kernel.Unlock()
}

func TestSameLevelLocksTakeAddressOrder(t *testing.T) {
	Install(Hooks{})
	var buckets [2]Spin
	buckets[0].SetLevel(LevelFutex)
	buckets[1].SetLevel(LevelFutex)
	buckets[0].SetRank(1)
	buckets[1].SetRank(2)
	buckets[0].Lock()
	buckets[1].LockAfter(&buckets[0])
	buckets[0].Unlock()
	if HeldLevels() != 1<<LevelFutex {
		t.Fatalf("held = %#x after releasing one of two", HeldLevels())
	}
	buckets[1].Unlock()
	if HeldLevels() != 0 {
		t.Fatalf("held = %#x after releasing both", HeldLevels())
	}
	expectFatal(t, "a second bucket below the first", func() {
		buckets[1].Lock()
		buckets[0].LockAfter(&buckets[1])
	})
	expectFatal(t, "a second lock of another level", func() {
		process := New(LevelProcess)
		buckets[0].Lock()
		process.LockAfter(&buckets[0])
	})
}

func TestAssertHeld(t *testing.T) {
	spin := New(LevelChannel)
	// Without hooks, as in host tests, nothing is checked.
	spin.AssertHeld()
	Install(Hooks{CPU: func() uint32 { return 0 }})
	defer Install(Hooks{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("state used without its lock was accepted")
			}
		}()
		spin.AssertHeld()
	}()
	spin.Lock()
	spin.AssertHeld()
	spin.Unlock()
}
