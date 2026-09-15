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
		manager.Register(Definition{Name: "vfs", Executable: "/system/vfs", Dependencies: []string{"ramfs"}, Capabilities: []Capability{{Object: 7, Type: handle.TypeChannel, Rights: capability.Read}}, Restart: RestartOnFailure}) != StatusOK {
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
