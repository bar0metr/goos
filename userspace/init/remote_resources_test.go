package supervisor

import (
	"goos/kernel/handle"
	"goos/userspace/remoteapi"
	"testing"
)

func TestRemoteAggregateJobAndTaskLimits(t *testing.T) {
	tests := []struct {
		jobs, tasks, stages int
		child, want         bool
	}{
		{0, 0, 1, false, true}, {3, 52, 4, false, true}, {3, 52, 4, true, false},
		{4, 0, 1, false, false}, {0, 56, 1, false, false}, {0, 55, 1, false, true},
		{0, 0, 0, false, false}, {0, 0, 5, false, false}, {-1, 0, 1, false, false}, {0, -1, 1, false, false},
	}
	for _, test := range tests {
		if got := RemoteJobAllowed(test.jobs, test.tasks, test.stages, test.child); got != test.want {
			t.Errorf("RemoteJobAllowed(%+v)=%v", test, got)
		}
	}
}

type remoteReleaseTest struct {
	calls   []string
	handles []handle.ID
	fail    handle.ID
	failMap bool
}

func (runner *remoteReleaseTest) Close(id handle.ID) bool {
	runner.calls = append(runner.calls, "close")
	runner.handles = append(runner.handles, id)
	return id != runner.fail
}
func (runner *remoteReleaseTest) Unmap(base, size uint64) bool {
	runner.calls = append(runner.calls, "unmap")
	return base == 0x400000 && size == 4096 && !runner.failMap
}
func TestRemoteResourcesReleaseAndRetry(t *testing.T) {
	resources := RemoteResources{Mapping: 0x400000, MappingSize: 4096, Handles: [10]handle.ID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
	runner := remoteReleaseTest{fail: 4, failMap: true}
	if resources.Release(&runner) {
		t.Fatal("failed resources reported released")
	}
	if len(runner.calls) != 11 || runner.calls[0] != "unmap" || len(runner.handles) != 10 {
		t.Fatalf("release abandoned resources: %+v", runner)
	}
	if resources.Handles[3] != 4 || resources.Mapping == 0 {
		t.Fatal("failed resources not retained")
	}
	runner = remoteReleaseTest{}
	if !resources.Release(&runner) || resources != (RemoteResources{}) {
		t.Fatal("retry did not finish release")
	}
	if len(runner.calls) != 2 || runner.calls[0] != "unmap" || len(runner.handles) != 1 || runner.handles[0] != 4 {
		t.Fatalf("successful releases repeated: %+v", runner)
	}
	if !resources.Release(&runner) || len(runner.calls) != 2 {
		t.Fatal("empty release was not idempotent")
	}
}
func TestRemoteResourcesRetainAdmissionUntilRelease(t *testing.T) {
	var registry RemoteRegistry
	first, _ := registry.Admit(1, 1)
	if _, result := registry.Admit(2, 1); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	registry.BeginClose(first, 1)
	resources := RemoteResources{Handles: [10]handle.ID{3}}
	runner := remoteReleaseTest{fail: 3}
	if resources.Release(&runner) {
		t.Fatal("release unexpectedly complete")
	}
	if _, result := registry.Admit(3, 1); result != remoteapi.ResultLimit {
		t.Fatalf("closing resources lost handshake capacity: %v", result)
	}
	runner.fail = 0
	if !resources.Release(&runner) || registry.FinishClose(first, 1) != remoteapi.ResultOK {
		t.Fatal("cleanup failed")
	}
	next, result := registry.Admit(3, 1)
	if result != remoteapi.ResultOK || next == first {
		t.Fatal("slot not safely reused")
	}
}
func TestRemoteResourcesMoreThanSupervisorCapacity(t *testing.T) {
	var registry RemoteRegistry
	for owner := uint64(1); owner <= 128; owner++ {
		id, result := registry.Admit(owner, 1)
		if result != remoteapi.ResultOK {
			t.Fatal(result)
		}
		resources := RemoteResources{Mapping: 0x400000, MappingSize: 4096, Handles: [10]handle.ID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
		runner := remoteReleaseTest{}
		if registry.BeginClose(id, owner) != remoteapi.ResultOK || !resources.Release(&runner) || registry.FinishClose(id, owner) != remoteapi.ResultOK {
			t.Fatalf("cleanup %d", owner)
		}
		if len(runner.handles) != 10 || resources != (RemoteResources{}) {
			t.Fatal("leaked references")
		}
	}
}
