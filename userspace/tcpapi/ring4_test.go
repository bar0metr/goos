package tcpapi

import (
	"sync"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/shmring"
)

func TestLayout4Pools(t *testing.T) {
	if Layout4.TXPool() != 16*16384+4096 || Layout4.RXPool() != 16*16384+4096 || Layout4.TXPoolPages() != 65 {
		t.Fatalf("GTCP 4 pools %d %d", Layout4.TXPool(), Layout4.RXPool())
	}
	if Layout3.TXPool() != 128<<10 || Layout2.RXPool() != RXSlots*SlotSize {
		t.Fatal("older pools changed")
	}
	if known, ok := LayoutFor(Version4); !ok || known != Layout4 {
		t.Fatal("LayoutFor(4)")
	}
}

func TestVersion4Requests(t *testing.T) {
	var message ipc.Message
	for _, request := range []Request{
		{Opcode: OpOpenConnect, Address: [4]byte{10, 0, 0, 1}, Port: 80, Flags: FlagNoWait},
		{Opcode: OpAcceptAhead, Endpoint: 3, Backlog: 8},
		{Opcode: OpOpen}, {Opcode: OpClose, Endpoint: 3}, {Opcode: OpWaitReady, Endpoint: 3, Mask: ReadyReadable},
	} {
		if !Layout4.EncodeRequest(&request, 9, &message) {
			t.Fatalf("encode %+v", request)
		}
		var decoded Request
		if correlation, ok := Layout4.DecodeRequest(&message, &decoded); !ok || correlation != 9 || decoded != request {
			t.Fatalf("round trip %+v -> %+v", request, decoded)
		}
		if _, ok := Layout3.DecodeRequest(&message, &decoded); ok {
			t.Fatal("GTCP 3 decoded a GTCP 4 request")
		}
	}
	for _, request := range []Request{
		{Opcode: OpWrite, Endpoint: 1, Length: 1}, {Opcode: OpRead, Endpoint: 1, Length: 1}, {Opcode: OpRelease},
		{Opcode: OpOpen, Release: 1}, {Opcode: OpOpenConnect, Port: 80}, {Opcode: OpOpenConnect, Endpoint: 1, Address: [4]byte{1}, Port: 1},
		{Opcode: OpAcceptAhead, Endpoint: 1, Backlog: Endpoints4 + 1}, {Opcode: OpDoorbell, Endpoint: 1},
	} {
		if Layout4.EncodeRequest(&request, 9, &message) || Layout4.EncodeRequest(&request, 0, &message) {
			t.Fatalf("encoded %+v", request)
		}
	}
	for _, request := range []Request{{Opcode: OpOpenConnect, Address: [4]byte{1}, Port: 1}, {Opcode: OpAcceptAhead, Endpoint: 1}} {
		if Layout3.EncodeRequest(&request, 9, &message) {
			t.Fatalf("GTCP 3 encoded %+v", request)
		}
	}
	if !EncodeDoorbell4(&message) || message.Kind != ipc.KindNotification || message.Correlation != 0 || !IsDoorbell4(&message) {
		t.Fatal("doorbell")
	}
	message.Payload[4] = 1
	if IsDoorbell4(&message) {
		t.Fatal("doorbell naming an endpoint")
	}
}

func TestVersion4Replies(t *testing.T) {
	var message ipc.Message
	tuple := Reply{Address: [4]byte{10, 0, 0, 2}, Port: 80, Local: [4]byte{10, 0, 0, 1}, LocalPort: 4000}
	for _, reply := range []Reply{
		{Opcode: OpOpen, Endpoint: 7, Slot: 63},
		{Opcode: OpOpen, Result: ResultBusy},
		{Opcode: OpOpenConnect, Result: ResultInProgress, Endpoint: 7, Slot: 2, Address: tuple.Address, Port: 80, Local: tuple.Local, LocalPort: 4000},
		{Opcode: OpOpenConnect, Result: ResultConnectionRefused},
		{Opcode: OpAccept, Endpoint: 8, Slot: 5, Address: tuple.Address, Port: 80, Local: tuple.Local, LocalPort: 4000},
		{Opcode: OpAcceptAhead, Endpoint: 3},
		{Opcode: OpClose, Endpoint: 3},
	} {
		if !Layout4.EncodeReply(&reply, 4, &message) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded Reply
		if _, ok := Layout4.DecodeReply(&message, &decoded); !ok || decoded != reply {
			t.Fatalf("round trip %+v", reply)
		}
	}
	for _, reply := range []Reply{
		{Opcode: OpOpen, Endpoint: 7, Slot: Endpoints4},
		{Opcode: OpClose, Endpoint: 3, Slot: 1},
		{Opcode: OpOpenConnect, Result: ResultConnectionRefused, Endpoint: 7},
		{Opcode: OpOpenConnect, Endpoint: 7, Slot: 1},
		{Opcode: OpWrite, Endpoint: 1},
		{Opcode: OpAccept, Result: ResultBusy, Endpoint: 1, Slot: 2},
	} {
		if Layout4.EncodeReply(&reply, 4, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
	if Layout3.EncodeReply(&Reply{Opcode: OpOpen, Endpoint: 7, Slot: 3}, 4, &message) {
		t.Fatal("GTCP 3 reply with a table index")
	}
}

func control(t *testing.T) Control {
	t.Helper()
	c, ok := ControlOf(make([]byte, Layout4.TXPool()), make([]byte, Layout4.RXPool()))
	if !ok {
		t.Fatal("control")
	}
	c.InitService()
	return c
}

func TestControlRingsCarryTheirKinds(t *testing.T) {
	c := control(t)
	if _, ok := ControlOf(make([]byte, Layout3.TXPool()), make([]byte, Layout4.RXPool())); ok {
		t.Fatal("control of a short pool")
	}
	write := Entry{Kind: EntryWrite, Slot: 15, Index: 63, Endpoint: 9, Length: SlotSize4, Generation: 3, Offset: 0xfffffff0}
	credit := Entry{Kind: EntryCredit, Index: 2, Endpoint: 9, Length: MaxCredit4}
	done := Entry{Kind: EntryWriteDone, Slot: 15, Index: 63, Endpoint: 9, Length: 10, Generation: 3, Result: ResultReset}
	accepted := Entry{Kind: EntryAccepted, Index: 4, Endpoint: 11, Slot: 1, Generation: 9, Address: [4]byte{1, 2, 3, 4}, Port: 5,
		Local: [4]byte{6, 7, 8, 9}, LocalPort: 10}
	if c.PushTX(&write) != shmring.OK || c.PushRelease(&credit) != shmring.OK || c.PushTXDone(&done) != shmring.OK || c.PushRX(&accepted) != shmring.OK {
		t.Fatal("push")
	}
	if !c.ServicePending() || !c.ClientPending() {
		t.Fatal("pending")
	}
	var e Entry
	if c.PopTX(&e) != shmring.OK || e != write || c.PopRelease(&e) != shmring.OK || e != credit ||
		c.PopTXDone(&e) != shmring.OK || e != done || c.PopRX(&e) != shmring.OK || e != accepted {
		t.Fatalf("pop %+v", e)
	}
	// Each ring refuses the kinds of the others.
	if c.PushTX(&credit) != shmring.Broken || c.PushRelease(&write) != shmring.Broken || c.PushTXDone(&accepted) != shmring.Broken ||
		c.PushRX(&done) != shmring.Broken {
		t.Fatal("wrong kind pushed")
	}
	for _, bad := range []Entry{
		{Kind: EntryWrite, Slot: TXSlots4, Index: 1, Endpoint: 1},
		{Kind: EntryWrite, Index: Endpoints4, Endpoint: 1},
		{Kind: EntryWrite, Index: 1, Endpoint: 1, Length: SlotSize4 + 1},
		{Kind: EntryCredit, Index: 1, Endpoint: 1},
		{Kind: EntryCredit, Index: 1, Endpoint: 1, Length: MaxCredit4 + 1},
		{Kind: EntryRelease, Slot: RXSlots4},
		{Kind: EntryRelease, Slot: 1, Endpoint: 4},
		{Kind: EntryData, Slot: 1, Index: 1, Endpoint: 1},
		{Kind: EntryEnd, Index: 1, Endpoint: 1},
		{Kind: EntryAccepted, Index: 1, Endpoint: 1, Generation: 1},
		{Kind: EntryForget, Index: 1, Endpoint: 1, Address: [4]byte{1}},
		{Kind: lastEntry + 1, Index: 1, Endpoint: 1},
	} {
		if ValidEntry(&bad) {
			t.Fatalf("valid %+v", bad)
		}
	}
	// The TX ring holds every TX slot with room to spare.
	for range TXEntries4 {
		if c.PushTX(&write) != shmring.OK {
			t.Fatal("TX ring smaller than its bound")
		}
	}
	if c.PushTX(&write) != shmring.Full {
		t.Fatal("full TX ring")
	}
}

func TestControlDoorbellRule(t *testing.T) {
	c := control(t)
	if c.ClientShouldRing() || c.ServiceShouldRing() {
		t.Fatal("rang before an arm")
	}
	c.ServiceArm()
	if !c.ClientShouldRing() || c.ClientShouldRing() {
		t.Fatal("one arm rings once")
	}
	c.ClientArm()
	c.ClientArm()
	if !c.ServiceShouldRing() || c.ServiceShouldRing() {
		t.Fatal("two arms before a ring still ring once")
	}
}

func TestReadinessUnderConcurrentWrites(t *testing.T) {
	c := control(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := uint32(1); round <= 20000; round++ {
			c.StoreReadiness(5, &Readiness{Endpoint: round, Mask: uint16(round), Readable: round, Room: round, Credit: round})
		}
	}()
	for range 20000 {
		r, ok := c.LoadReadiness(5)
		if ok && (uint32(r.Mask) != r.Endpoint&0xffff || r.Readable != r.Endpoint || r.Room != r.Endpoint || r.Credit != r.Endpoint) {
			t.Fatalf("torn entry %+v", r)
		}
	}
	wg.Wait()
	if r, ok := c.LoadReadiness(5); !ok || r.Endpoint != 20000 {
		t.Fatalf("last %+v", r)
	}
	if _, ok := c.LoadReadiness(Endpoints4); ok {
		t.Fatal("entry past the table")
	}
}

// FuzzControlPages fills the client page with arbitrary bytes, as a broken
// client could: netd's pops must not panic and must hand out only valid
// entries of their ring.
func FuzzControlPages(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 2, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		tx, rx := make([]byte, Layout4.TXPool()), make([]byte, Layout4.RXPool())
		c, _ := ControlOf(tx, rx)
		page := tx[Layout4.Offset(TXSlots4):]
		copy(page, data)
		var e Entry
		for range 2 * ReleaseEntries4 {
			if status := c.PopTX(&e); status == shmring.OK && (!ValidEntry(&e) || e.Kind != EntryWrite) {
				t.Fatalf("TX handed out %+v", e)
			}
			if status := c.PopRelease(&e); status == shmring.OK && (!ValidEntry(&e) || e.Kind == EntryWrite) {
				t.Fatalf("Release handed out %+v", e)
			}
		}
	})
}
