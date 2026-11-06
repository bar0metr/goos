package linux

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

// readinessPool is a session pool's readiness pages for the scan.
func readinessPool(session uint32) ([]byte, *ReadinessView) {
	pool := make([]byte, linuxsockapi.ReadinessPages*4096)
	linuxsockapi.InitReadiness(pool, session)
	var view ReadinessView
	for index := range view {
		view[index] = pool[index*4096 : (index+1)*4096]
	}
	return pool, &view
}

func scanAll(t *testing.T, endpoint NetEndpoint, view *ReadinessView) []uint64 {
	t.Helper()
	var out []uint64
	var chunk [3]uint64
	for cursor := 0; cursor >= 0; {
		var count int
		count, cursor = ScanSocketWakes(testFamily, endpoint.Session, view, cursor, chunk[:])
		out = append(out, chunk[:count]...)
	}
	return out
}

func TestWakeScanNamesOnlyChangedSockets(t *testing.T) {
	_, endpoint := newTestSession(t)
	pool, view := readinessPool(endpoint.Session)
	var objects, identities [5]uint64
	for index := range objects {
		identities[index] = linuxsockapi.MakeSocket(endpoint.Session, 1, index)
		object, ok := NewSocket(testFamily, endpoint.Session, identities[index], false)
		if !ok {
			t.Fatal("new socket")
		}
		objects[index] = object
	}
	// Entries the service never wrote list nothing.
	if got := scanAll(t, endpoint, view); len(got) != 0 {
		t.Fatalf("untouched entries listed %d sockets", len(got))
	}
	NoteReadyChanged(testFamily)
	if !TakeRescan(testFamily) || TakeRescan(testFamily) {
		t.Fatal("rescan request not taken once")
	}
	scanAll(t, endpoint, view)
	// One entry changes: only its socket wakes, and its epoch moves.
	before, _, _, _ := SocketOf(objects[3])
	linuxsockapi.Publish(pool, 3, linuxsockapi.Readiness{Socket: identities[3], Ready: linuxsockapi.ReadyIn, Readable: 1})
	got := scanAll(t, endpoint, view)
	if len(got) != 1 || got[0] != objects[3] {
		t.Fatalf("scan listed %x, want %x", got, objects[3])
	}
	after, _, _, _ := SocketOf(objects[3])
	if after.WakeEpoch() == before.WakeEpoch() {
		t.Fatal("wake epoch did not move")
	}
	if got := scanAll(t, endpoint, view); len(got) != 0 {
		t.Fatalf("an unchanged pool listed %x", got)
	}
	// A ring entry for a socket wakes it though its entry stayed.
	tablesLock.Lock()
	context, _, _ := netContextHeld(testFamily)
	context.touchHeld(identities[1])
	tablesLock.Unlock()
	if got := scanAll(t, endpoint, view); len(got) != 1 || got[0] != objects[1] {
		t.Fatalf("touched scan listed %x", got)
	}
	// Every socket of the family, for a failed session.
	var all [8]uint64
	if count, next := FamilySocketObjects(testFamily, 0, all[:]); count != 5 || next != -1 {
		t.Fatalf("family sockets %d next %d", count, next)
	}
}

func TestPumpClaimRunsAgainForALateCaller(t *testing.T) {
	newTestSession(t)
	if !BeginPump(testFamily) {
		t.Fatal("first claim refused")
	}
	if BeginPump(testFamily) {
		t.Fatal("second claim granted while the first drains")
	}
	if !EndPump(testFamily) {
		t.Fatal("the drain did not go round again for the late caller")
	}
	if EndPump(testFamily) {
		t.Fatal("the drain went round twice")
	}
	if !BeginPump(testFamily) {
		t.Fatal("claim after the drain ended refused")
	}
	EndPump(testFamily)
}

func TestChannelMirrorFollowsTheSession(t *testing.T) {
	_, endpoint := newTestSession(t)
	family, found := uint64(0), false
	if family, _, found = NetFamilyOfChannel(endpoint.Channel, endpoint.Side); !found || family != testFamily {
		t.Fatal("live session not found by its channel")
	}
	if _, _, found = NetFamilyOfChannel(endpoint.Channel, endpoint.Side^1); found {
		t.Fatal("the service's side found")
	}
	if _, _, failed := FailNet(testFamily, endpoint.Session, nil); !failed {
		t.Fatal("fail")
	}
	if _, _, found = NetFamilyOfChannel(endpoint.Channel, endpoint.Side); found {
		t.Fatal("a failed session still found")
	}
}
