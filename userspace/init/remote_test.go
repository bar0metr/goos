package supervisor

import (
	"goos/userspace/remoteapi"
	"testing"
)

func remoteGeometry(t *testing.T) remoteapi.Geometry {
	t.Helper()
	geometry, ok := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	if !ok {
		t.Fatal("geometry")
	}
	return geometry
}

func TestRemoteHandshakeCapacity(t *testing.T) {
	var registry RemoteRegistry
	for owner := uint64(1); owner <= remoteapi.MaxHandshakes; owner++ {
		if _, result := registry.Admit(owner, 1); result != remoteapi.ResultOK {
			t.Fatal("admit", result)
		}
	}
	if _, result := registry.Admit(99, 1); result != remoteapi.ResultLimit {
		t.Fatal("unbounded handshakes", result)
	}
}

func TestRemoteCapacityIncludesClosingSessions(t *testing.T) {
	var registry RemoteRegistry
	var ids [remoteapi.MaxSessions]uint64
	for index := range ids {
		owner := uint64(index + 1)
		id, result := registry.Admit(owner, 9)
		if result != remoteapi.ResultOK {
			t.Fatal(result)
		}
		ids[index] = id
		if result = registry.Authenticate(id, owner, 4); result != remoteapi.ResultOK {
			t.Fatal(result)
		}
	}
	if result := registry.BeginClose(ids[0], 1); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if registry.Authenticated() != 4 {
		t.Fatal("closing session freed capacity")
	}
	fifth, result := registry.Admit(5, 9)
	if result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result = registry.Authenticate(fifth, 5, 4); result != remoteapi.ResultLimit {
		t.Fatal("fifth session admitted", result)
	}
	if result = registry.FinishClose(ids[0], 1); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result = registry.Authenticate(fifth, 5, 4); result != remoteapi.ResultOK {
		t.Fatal("capacity not reclaimed", result)
	}
}

func TestRemoteGenerationAndOwnership(t *testing.T) {
	var registry RemoteRegistry
	id, _ := registry.Admit(1, 3)
	if result := registry.Authenticate(id, 2, 4); result != remoteapi.ResultPermissionDenied {
		t.Fatal("owner bypass", result)
	}
	if result := registry.FinishClose(id, 1); result != remoteapi.ResultInvalidArgument {
		t.Fatal("freed live record", result)
	}
	registry.BeginClose(id, 1)
	registry.BeginClose(id, 1)
	if result := registry.FinishClose(id, 1); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	next, _ := registry.Admit(1, 4)
	if next == id || uint32(next) != uint32(id) {
		t.Fatal("slot generation", id, next)
	}
	if result := registry.BeginClose(id, 1); result != remoteapi.ResultNotFound {
		t.Fatal("stale close", result)
	}
	if snapshot, result := registry.Snapshot(next, 1); result != remoteapi.ResultOK || snapshot.State != RemoteHandshake || snapshot.Policy != 4 {
		t.Fatal("new record corrupted", snapshot, result)
	}
}

func TestRemoteOpenResizeAndFailureCleanup(t *testing.T) {
	var registry RemoteRegistry
	id, _ := registry.Admit(1, 8)
	geometry := remoteGeometry(t)
	if result := registry.BeginOpen(id, 1, geometry); result != remoteapi.ResultInvalidArgument {
		t.Fatal("unauthenticated open", result)
	}
	registry.Authenticate(id, 1, 1)
	if result := registry.BeginOpen(id, 1, geometry); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result := registry.BeginOpen(id, 1, geometry); result != remoteapi.ResultInvalidArgument {
		t.Fatal("duplicate open", result)
	}
	if result := registry.Resize(id, 1, geometry); result != remoteapi.ResultInvalidArgument {
		t.Fatal("resize before commit", result)
	}
	registry.CommitOpen(id, 1)
	geometry.Columns, geometry.Rows = 120, 40
	geometry.Sequence++
	if result := registry.Resize(id, 1, geometry); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	snapshot, _ := registry.Snapshot(id, 1)
	if snapshot.Geometry != geometry {
		t.Fatal("stale geometry")
	}
	registry.BeginClose(id, 1)
	registry.FinishClose(id, 1)
	if registry.Authenticated() != 0 {
		t.Fatal("session leaked")
	}
	failed, _ := registry.Admit(2, 8)
	registry.Authenticate(failed, 2, 1)
	registry.BeginOpen(failed, 2, geometry)
	registry.BeginClose(failed, 2)
	registry.FinishClose(failed, 2)
	if registry.Authenticated() != 0 {
		t.Fatal("failed opening leaked")
	}
}

func TestRemoteGenerationWrapRetiresSlot(t *testing.T) {
	var registry RemoteRegistry
	registry.slots[0].generation = ^uint32(0)
	id, result := registry.Admit(1, 1)
	if result != remoteapi.ResultOK || uint32(id) == 1 || !registry.slots[0].retired {
		t.Fatal("wrapped ID reused", id, result)
	}
}

func TestRemoteInvalidAdmission(t *testing.T) {
	var registry RemoteRegistry
	for _, values := range [][2]uint64{{0, 1}, {1, 0}} {
		if _, result := registry.Admit(values[0], values[1]); result != remoteapi.ResultInvalidArgument {
			t.Fatal(result)
		}
	}
	id, _ := registry.Admit(1, 1)
	if _, result := registry.Admit(1, 1); result != remoteapi.ResultInvalidArgument {
		t.Fatal("duplicate owner")
	}
	for _, limit := range []int{0, 5} {
		if result := registry.Authenticate(id, 1, limit); result != remoteapi.ResultInvalidArgument {
			t.Fatal("invalid limit", limit, result)
		}
	}
}
