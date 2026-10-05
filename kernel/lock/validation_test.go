//go:build goos_validation

package lock

import "testing"

func expectFatal(t *testing.T, name string, run func()) {
	t.Helper()
	var message string
	Install(Hooks{Fatal: func(text string) { message = text; panic(text) }})
	defer Install(Hooks{})
	defer func() {
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
