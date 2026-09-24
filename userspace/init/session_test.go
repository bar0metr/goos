package supervisor

import (
	"testing"

	"goos/internal/capability"
	"goos/kernel/handle"
)

// A restarted shell or terminal is launched with endpoints PID 1 renews for it
// (ADR 0089): some grants are replaced, and an optional one that could not be
// renewed is dropped rather than handed over dead.
func TestRenewedSessionReplacesAndDropsGrants(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "shell", Executable: "/system/shell", Restart: RestartAlways,
		Capabilities: []Capability{
			{Name: "stdout", Handle: 1, Type: handle.TypeStream, Rights: capability.Write},
			{Name: "tty", Handle: 2, Type: handle.TypeChannel, Rights: capability.Read | capability.Write, Consume: true},
			{Name: "config", Handle: 3, Type: handle.TypeChannel, Rights: capability.Read | capability.Write, Consume: true},
			{Name: "exec", Handle: 4, Type: handle.TypeChannel, Rights: capability.Read | capability.Write, Consume: true},
		}}) != StatusOK {
		t.Fatal("register failed")
	}
	if id, found := manager.Capability("shell", "tty"); !found || id != 2 {
		t.Fatalf("tty grant = %d, %v", id, found)
	}
	if _, found := manager.Capability("shell", "missing"); found {
		t.Fatal("a missing grant was found")
	}
	if _, found := manager.Capability("absent", "tty"); found {
		t.Fatal("a grant of an absent service was found")
	}
	if manager.SetCapability("shell", "tty", 12) != StatusOK || manager.RemoveCapability("shell", "config") != StatusOK {
		t.Fatal("renewal refused")
	}
	if manager.RemoveCapability("shell", "config") != StatusNotFound || manager.RemoveCapability("absent", "tty") != StatusNotFound {
		t.Fatal("an absent grant was removed")
	}
	var launched LaunchSpec
	if manager.StartReady(0, func(request LaunchSpec) (uint64, bool) {
		launched = request
		return 7, true
	}) != 1 {
		t.Fatal("start failed")
	}
	// The remaining grants keep their order, and the dropped one is gone.
	if launched.CapabilityCount != 3 || launched.Capabilities[0].Name != "stdout" || launched.Capabilities[1].Handle != 12 ||
		launched.Capabilities[2].Name != "exec" || launched.Capabilities[3] != (Capability{}) {
		t.Fatalf("launched %+v", launched.Capabilities[:4])
	}
}

func TestAlwaysRestartsAfterASuccessfulExitWithinTheLimit(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "tty", Executable: "/system/tty", Restart: RestartAlways,
		Capabilities: []Capability{{Name: "console", Handle: 1, Type: handle.TypeDevice, Rights: capability.Read}}}) != StatusOK {
		t.Fatal("register failed")
	}
	pid := uint64(10)
	launch := func(LaunchSpec) (uint64, bool) { pid++; return pid, true }
	now := uint64(0)
	if manager.StartReady(now, launch) != 1 {
		t.Fatal("start failed")
	}
	// A terminal that exits, even cleanly, is a terminal gone: it comes back
	// until the restart limit says the fault is not transient.
	for restart := 0; restart < MaxRestarts; restart++ {
		now += RestartBackoff
		if manager.Exit(pid, 0, now) != StatusOK {
			t.Fatal("exit not recorded")
		}
		if manager.StartReady(now+RestartBackoff, launch) != 1 {
			t.Fatalf("restart %d did not happen", restart)
		}
	}
	now += RestartBackoff
	manager.Exit(pid, 0, now)
	if manager.StartReady(now+RestartBackoff, launch) != 0 {
		t.Fatal("a crash loop was not rate limited")
	}
	if snapshot, _ := manager.Snapshot("tty"); snapshot.State != StateFailed {
		t.Fatalf("state after the limit = %d", snapshot.State)
	}
}
