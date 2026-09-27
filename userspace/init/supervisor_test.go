package supervisor

import (
	"fmt"
	"goos/internal/capability"
	"goos/kernel/handle"
	"testing"
)

func TestStartsServicesInDependencyOrder(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "ramfs", Executable: "/system/ramfs", Restart: RestartAlways}) != StatusOK ||
		manager.Register(Definition{Name: "vfs", Executable: "/system/vfs", Dependencies: []string{"ramfs"}, Capabilities: []Capability{{Name: "session", Handle: 7, Type: handle.TypeChannel, Rights: capability.Read}}, Restart: RestartOnFailure}) != StatusOK {
		t.Fatal("registration failed")
	}
	var order [2]string
	nextPID := uint64(10)
	started := manager.StartReady(0, func(spec LaunchSpec) (uint64, bool) {
		order[nextPID-10] = string(spec.Name[:spec.NameSize])
		nextPID++
		return nextPID, true
	})
	if started != 2 || order != [2]string{"ramfs", "vfs"} {
		t.Fatalf("start order = %q, count = %d", order, started)
	}
	vfs, _ := manager.Snapshot("vfs")
	if vfs.State != StateRunning || vfs.Health != HealthHealthy {
		t.Fatalf("vfs state = %#v", vfs)
	}
}

func TestRestartPoliciesAndRateLimit(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "service", Executable: "/service", Restart: RestartOnFailure}) != StatusOK {
		t.Fatal("registration failed")
	}
	pid := uint64(1)
	launch := func(LaunchSpec) (uint64, bool) { pid++; return pid, true }
	manager.StartReady(0, launch)
	for restart := 0; restart < MaxRestarts; restart++ {
		if manager.Exit(pid, 1, uint64(restart*20)) != StatusOK {
			t.Fatal("exit not detected")
		}
		manager.StartReady(uint64(restart*20)+RestartBackoff, launch)
	}
	if manager.Exit(pid, 1, 100) != StatusOK {
		t.Fatal("rate-limited exit not detected")
	}
	snapshot, _ := manager.Snapshot("service")
	if snapshot.State != StateFailed || snapshot.Health != HealthFailed || snapshot.Restarts != MaxRestarts {
		t.Fatalf("rate limit state = %#v", snapshot)
	}
	event, ok := manager.Event(manager.EventCount() - 1)
	if !ok || event.Kind != EventRateLimited {
		t.Fatalf("missing rate-limit event: %#v", event)
	}
}

func TestNeverAndAlwaysPolicies(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "once", Executable: "/once", Restart: RestartNever})
	manager.Register(Definition{Name: "daemon", Executable: "/daemon", Restart: RestartAlways})
	pid := uint64(20)
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { pid++; return pid, true })
	if manager.Exit(21, 0, 1) != StatusOK || manager.Exit(22, 0, 1) != StatusOK {
		t.Fatal("exit handling failed")
	}
	once, _ := manager.Snapshot("once")
	daemon, _ := manager.Snapshot("daemon")
	if once.State != StateStopped || daemon.State != StateRegistered {
		t.Fatalf("policy states: once=%#v daemon=%#v", once, daemon)
	}
}

func TestOnFailureStopsAfterSuccessfulExit(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "worker", Executable: "/worker", Restart: RestartOnFailure})
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 41, true })
	if manager.Exit(41, 0, 1) != StatusOK || manager.StartReady(100, func(LaunchSpec) (uint64, bool) { return 42, true }) != 0 {
		t.Fatal("on-failure service restarted after success")
	}
	snapshot, _ := manager.Snapshot("worker")
	if snapshot.State != StateStopped || snapshot.Health != HealthStopped {
		t.Fatalf("successful state = %#v", snapshot)
	}
}

func TestRejectsInvalidConfigurationAndDuplicatePID(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "bad/name", Executable: "/bad"}) != StatusInvalidArgument ||
		manager.Register(Definition{Name: "bad", Executable: "relative"}) != StatusInvalidArgument ||
		manager.Register(Definition{Name: "bad", Executable: "/bad", Capabilities: []Capability{{}}}) != StatusInvalidArgument ||
		manager.Register(Definition{Name: "bad", Executable: "/bad", Profile: ProfileLarge + 1}) != StatusInvalidArgument ||
		manager.Register(Definition{Name: "child", Executable: "/child", Dependencies: []string{"missing"}}) != StatusInvalidConfiguration ||
		manager.Register(Definition{Name: "root", Executable: "/root"}) != StatusOK ||
		manager.Register(Definition{Name: "root", Executable: "/other"}) != StatusAlreadyExists {
		t.Fatal("invalid configuration accepted")
	}
	manager.Register(Definition{Name: "peer", Executable: "/peer"})
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 7, true })
	peer, _ := manager.Snapshot("peer")
	if peer.State != StateFailed {
		t.Fatalf("duplicate PID accepted: %#v", peer)
	}
}

func TestDependencyHealthTracksCrashAndRecovery(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "parent", Executable: "/parent", Restart: RestartAlways})
	manager.Register(Definition{Name: "child", Executable: "/child", Dependencies: []string{"parent"}})
	pid := uint64(30)
	launch := func(LaunchSpec) (uint64, bool) { pid++; return pid, true }
	manager.StartReady(0, launch)
	if manager.Exit(31, 1, 1) != StatusOK {
		t.Fatal("parent exit failed")
	}
	child, _ := manager.Snapshot("child")
	if child.State != StateRunning || child.Health != HealthBlocked {
		t.Fatalf("child did not degrade: %#v", child)
	}
	manager.StartReady(1+RestartBackoff, launch)
	child, _ = manager.Snapshot("child")
	if child.Health != HealthHealthy {
		t.Fatalf("child did not recover: %#v", child)
	}
}

func TestEventLogIsBounded(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "daemon", Executable: "/daemon", Restart: RestartAlways})
	pid := uint64(1)
	for cycle := 0; cycle < 20; cycle++ {
		manager.StartReady(uint64(cycle)*RestartWindow, func(LaunchSpec) (uint64, bool) { pid++; return pid, true })
		manager.Exit(pid, 1, uint64(cycle)*RestartWindow+1)
	}
	if manager.EventCount() != MaxEvents {
		t.Fatalf("event count = %d", manager.EventCount())
	}
}

func TestServiceTableIsBounded(t *testing.T) {
	var manager Manager
	for index := 0; index < MaxServices; index++ {
		name := fmt.Sprintf("service-%d", index)
		if manager.Register(Definition{Name: name, Executable: "/" + name}) != StatusOK {
			t.Fatalf("service %d rejected", index)
		}
	}
	if manager.Register(Definition{Name: "overflow", Executable: "/overflow"}) != StatusResourceExhausted {
		t.Fatal("service capacity exceeded")
	}
}

func TestAtListsRegisteredServicesInOrder(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "vfs", Executable: "/system/vfs"})
	manager.Register(Definition{Name: "shell", Executable: "/system/shell", Dependencies: []string{"vfs"}})
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 9, true })
	name, snapshot, ok := manager.At(0)
	if !ok || string(name) != "vfs" || snapshot.State != StateRunning || snapshot.PID != 9 {
		t.Fatalf("first service %q %+v %v", name, snapshot, ok)
	}
	if name, _, ok := manager.At(1); !ok || string(name) != "shell" {
		t.Fatalf("second service %q %v", name, ok)
	}
	if _, _, ok := manager.At(2); ok {
		t.Fatal("unregistered index listed")
	}
}

func TestSetCapabilityRefreshesOneHandle(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "driver", Executable: "/system/driver", Profile: ProfileLarge,
		Capabilities: []Capability{
			{Name: "ports", Handle: 1, Type: handle.TypeDevice, Rights: capability.Read},
			{Name: "service", Handle: 2, Type: handle.TypeChannel, Rights: capability.Read | capability.Write},
		}}) != StatusOK {
		t.Fatal("register failed")
	}
	if manager.SetCapability("driver", "service", 9) != StatusOK {
		t.Fatal("refresh refused")
	}
	var launched LaunchSpec
	if manager.StartReady(0, func(request LaunchSpec) (uint64, bool) {
		launched = request
		return 7, true
	}) != 1 {
		t.Fatal("start failed")
	}
	if launched.Capabilities[1].Handle != 9 {
		t.Fatalf("stale handle launched: %d", launched.Capabilities[1].Handle)
	}
	if launched.Profile != ProfileLarge {
		t.Fatalf("profile = %d", launched.Profile)
	}
	// The other capabilities are untouched.
	if launched.Capabilities[0].Handle != 1 {
		t.Fatalf("an unrelated capability changed: %d", launched.Capabilities[0].Handle)
	}
	if manager.SetCapability("driver", "missing", 3) != StatusNotFound ||
		manager.SetCapability("absent", "service", 3) != StatusNotFound ||
		manager.SetCapability("driver", "service", 0) != StatusInvalidArgument {
		t.Fatal("an invalid refresh was accepted")
	}
}

func TestScheduledRestartIsVisibleAsPending(t *testing.T) {
	var manager Manager
	if manager.Register(Definition{Name: "driver", Executable: "/system/driver", Restart: RestartOnFailure,
		Capabilities: []Capability{{Name: "ports", Handle: 1, Type: handle.TypeDevice, Rights: capability.Read}}}) != StatusOK {
		t.Fatal("register failed")
	}
	if !manager.RestartPending() {
		t.Fatal("a registered service is not reported as wanting to start")
	}
	if manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 5, true }) != 1 {
		t.Fatal("start failed")
	}
	if manager.RestartPending() {
		t.Fatal("a running service is reported as pending")
	}
	// A crash schedules a restart that nothing else will trigger.
	if manager.Exit(5, 1, 10) != StatusOK {
		t.Fatal("exit not recorded")
	}
	if !manager.RestartPending() {
		t.Fatal("a scheduled restart is invisible")
	}
	if manager.StartReady(10+RestartBackoff, func(LaunchSpec) (uint64, bool) { return 6, true }) != 1 {
		t.Fatal("the scheduled restart did not run")
	}
	if manager.RestartPending() {
		t.Fatal("the restarted service is still pending")
	}
	// A service that exhausts its restarts stops being pending, or the
	// supervisor would spin on a deadline that can never be met.
	pid := uint64(6)
	for attempt := 0; attempt < MaxRestarts+2; attempt++ {
		manager.Exit(pid, 1, uint64(20+attempt*int(RestartBackoff)))
		pid++
		manager.StartReady(uint64(20+attempt*int(RestartBackoff))+RestartBackoff, func(LaunchSpec) (uint64, bool) { return pid, true })
	}
	if snapshot, _ := manager.Snapshot("driver"); snapshot.State == StateFailed && manager.RestartPending() {
		t.Fatal("a failed service is still pending")
	}
}

func TestPlannedStopRequiresExplicitResume(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "fat32", Executable: "/system/fat32", Restart: RestartOnFailure})
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 7, true })
	if manager.PlanStop("fat32") != StatusOK || manager.Exit(7, 2, 1) != StatusOK {
		t.Fatal("planned stop failed")
	}
	if snapshot, _ := manager.Snapshot("fat32"); snapshot.State != StateStopped || manager.RestartPending() {
		t.Fatalf("planned stop scheduled a restart: %+v", snapshot)
	}
	if manager.Resume("fat32", 2) != StatusOK || manager.StartReady(2, func(LaunchSpec) (uint64, bool) { return 8, true }) != 1 {
		t.Fatal("resume failed")
	}
}

func TestCancelledStopKeepsFailureRestartPolicy(t *testing.T) {
	var manager Manager
	manager.Register(Definition{Name: "fat32", Executable: "/system/fat32", Restart: RestartOnFailure})
	manager.StartReady(0, func(LaunchSpec) (uint64, bool) { return 7, true })
	if manager.PlanStop("fat32") != StatusOK || manager.CancelStop("fat32") != StatusOK || manager.Exit(7, 2, 1) != StatusOK {
		t.Fatal("cancelled stop failed")
	}
	if !manager.RestartPending() {
		t.Fatal("cancelled stop suppressed a real failure")
	}
}
