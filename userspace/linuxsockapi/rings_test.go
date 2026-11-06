package linuxsockapi

import "testing"

func TestRingsCarryEntriesInOrder(t *testing.T) {
	page := make([]byte, pageSize)
	InitRings(page)
	var out RingEntry
	if PopSubmit(page, &out) != RingEmpty || PopComplete(page, &out) != RingEmpty || SubmitPending(page) || CompletePending(page) {
		t.Fatal("fresh rings not empty")
	}
	// Run the indices past a wrap of the entry array several times.
	for round := 0; round < 5*RingEntries; round++ {
		in := RingEntry{Kind: RingTX, Slot: uint8(round % TXSlots), Flags: RingDestination, Length: uint32(round),
			Socket: socket, Generation: uint32(round), Address: peer, Port: 53}
		if PushSubmit(page, &in) != RingOK || !SubmitPending(page) {
			t.Fatalf("push %d", round)
		}
		if PopSubmit(page, &out) != RingOK || out != in {
			t.Fatalf("pop %d: %+v", round, out)
		}
		done := RingEntry{Kind: RingRXData, Slot: uint8(round % RXSlots), Length: SlotSize, Socket: socket, Generation: 9}
		if PushComplete(page, &done) != RingOK || PopComplete(page, &out) != RingOK || out != done {
			t.Fatalf("complete %d: %+v", round, out)
		}
	}
}

func TestRingsRefuseWhatBreaksTheLayout(t *testing.T) {
	page := make([]byte, pageSize)
	InitRings(page)
	bad := []RingEntry{
		{Kind: RingTXDone, Slot: 0},              // wrong ring
		{Kind: RingTX, Slot: TXSlots, Socket: 1}, // slot out of range
		{Kind: RingTX, Slot: 0, Length: SlotSize + 1, Socket: 1},
		{Kind: RingTX, Slot: 0},                 // no socket
		{Kind: RingRelease, Slot: 0, Length: 1}, // a release carries no length
		{Kind: RingRelease, Slot: 0, Flags: 1},  // nor flags
		{Kind: 0, Slot: 0, Socket: 1},
	}
	for _, entry := range bad {
		if PushSubmit(page, &entry) != RingBroken {
			t.Fatalf("pushed %+v", entry)
		}
	}
	entry := RingEntry{Kind: RingRelease, Slot: 1}
	for count := 0; count < RingEntries; count++ {
		if PushSubmit(page, &entry) != RingOK {
			t.Fatalf("push %d", count)
		}
	}
	if PushSubmit(page, &entry) != RingFull {
		t.Fatal("pushed past the ring")
	}
	// A peer that moves the tail past the ring breaks it.
	*word(page, offCompleteTail) = RingEntries + 1
	var out RingEntry
	if PopComplete(page, &out) != RingBroken {
		t.Fatal("popped from a broken ring")
	}
	// An entry with a reserved byte set breaks it too.
	InitRings(page)
	done := RingEntry{Kind: RingTXDone, Slot: 2, Generation: 4}
	PushComplete(page, &done)
	page[completeBase+3] = 1
	if PopComplete(page, &out) != RingBroken {
		t.Fatal("popped an entry with a reserved byte")
	}
}

func TestDoorbellToken(t *testing.T) {
	page := make([]byte, pageSize)
	InitRings(page)
	if !ClaimDoorbell(page) || ClaimDoorbell(page) {
		t.Fatal("claim")
	}
	TakeDoorbell(page)
	if !ClaimDoorbell(page) {
		t.Fatal("claim after take")
	}
	DropDoorbell(page)
	if !ClaimDoorbell(page) {
		t.Fatal("claim after drop")
	}
}

func TestServiceCountersRoundTrip(t *testing.T) {
	page := make([]byte, pageSize)
	in := ServiceCounters{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}
	StoreServiceCounters(page, &in)
	if LoadServiceCounters(page) != in {
		t.Fatal("counters")
	}
}

func TestReadinessCarriesState(t *testing.T) {
	page := make([]byte, ReadinessPages*pageSize)
	InitReadiness(page, 3)
	state := Readiness{Socket: socket, Ready: ReadyOut, State: EntrySend | EntryConnected}
	Publish(page, 5, state)
	if got, ok := Read(page, 5, 4); !ok || got != state {
		t.Fatalf("entry %+v", got)
	}
	*word(page, entryOffset(5)+offEntryState) = 1 << 5
	if _, status := ReadEntry(page, 5, 4); status != EntryInvalid {
		t.Fatal("unknown state bit accepted")
	}
}

func TestSnapshotsFitAndRoundTrip(t *testing.T) {
	if SnapshotEnd > pageSize {
		t.Fatalf("snapshot ends at %d", SnapshotEnd)
	}
	page := make([]byte, pageSize)
	f := FamilySnapshot{1, 2, 3, 4, 5, 6, 7, 8, true, 500, 512, 32, 900, 1024}
	StoreFamilySnapshot(page, &f)
	if LoadFamilySnapshot(page) != f {
		t.Fatal("family snapshot")
	}
	pool := make([]byte, PoolBytes)
	for _, index := range []int{0, SnapshotsPerPage - 1, SnapshotsPerPage, MaxSockets - 1} {
		s := SocketSnapshot{Socket: uint64(index + 1), Endpoint: 10, State: 1, TXQueue: 2, WritesOut: 3, ReadsOut: 4,
			Flags: SnapTXBlocked | SnapPending, TXAccepted: 5, TXSent: 6, TXStream: 7, Mask: 8, Readable: 11}
		number := SocketSnapshotPage(index)
		if number < SnapshotPage || number >= SnapshotPage+SnapshotPages {
			t.Fatalf("socket %d in page %d", index, number)
		}
		page := pool[number*pageSize : (number+1)*pageSize]
		StoreSocketSnapshot(page, index, &s)
		if LoadSocketSnapshot(page, index) != s {
			t.Fatalf("socket snapshot %d", index)
		}
	}
}

func TestPoolLayoutHoldsEveryIndex(t *testing.T) {
	if ReadinessBytes > ReadinessPages*pageSize || RingPage != ReadinessPage+ReadinessPages ||
		EntryPage(MaxSockets-1) != ReadinessPage+ReadinessPages-1 {
		t.Fatalf("readiness pages %d for %d bytes", ReadinessPages, ReadinessBytes)
	}
	if TXFirstPage != SnapshotPage+SnapshotPages || SocketSnapshotPage(MaxSockets-1) >= TXFirstPage {
		t.Fatal("snapshot pages overlap the TX slots")
	}
	pool := make([]byte, PoolBytes)
	InitReadiness(pool, 3)
	for _, index := range []int{0, 125, 126, 127, MaxSockets - 1} {
		state := Readiness{Socket: MakeSocket(3, 7, index), Ready: ReadyIn}
		Publish(pool, index, state)
		page := pool[EntryPage(index)*pageSize:][:pageSize]
		if got, status := ReadEntry(page, index, 4); status != EntrySettled || got != state {
			t.Fatalf("entry %d %+v", index, got)
		}
	}
}

func TestIdentityCarriesWideIndex(t *testing.T) {
	id := MakeSocket(9, GenerationMask, MaxSockets-1)
	if index, ok := SocketIndex(id); !ok || index != MaxSockets-1 || SessionOf(id) != 9 {
		t.Fatalf("identity %#x", id)
	}
	if index, _ := SocketIndex(MakeSocket(9, GenerationMask, 5)); index != 5 {
		t.Fatal("generation leaked into the index")
	}
	if _, ok := SocketIndex(MakeSocket(9, 1, MaxSockets)); ok {
		t.Fatal("index past MaxSockets accepted")
	}
}
