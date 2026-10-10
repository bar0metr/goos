package tcpdispatch

import (
	"goos/kernel/ipc"
	"goos/userspace/tcpapi"
	"testing"
)

type fixture struct {
	session  Session
	tx       [tcpapi.TXSlots * tcpapi.SlotSize]byte
	rx       [tcpapi.RXSlots * tcpapi.SlotSize]byte
	messages []ipc.Message
	blocked  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := new(fixture)
	if !f.session.Init(f.tx[:], f.rx[:], func(message *ipc.Message) bool {
		if f.blocked {
			return false
		}
		f.messages = append(f.messages, *message)
		return true
	}) {
		t.Fatal("init")
	}
	return f
}

func (f *fixture) complete(t *testing.T, correlation uint64, reply tcpapi.Reply) {
	t.Helper()
	var message ipc.Message
	if !tcpapi.EncodeReply(&reply, correlation, &message) || !f.session.Receive(&message) {
		t.Fatal("completion rejected", reply)
	}
}

func TestUnrelatedCompletionsRetained(t *testing.T) {
	f := newFixture(t)
	first, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 10}, nil)
	second, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 2, Length: 10}, nil)
	copy(f.rx[tcpapi.SlotSize:], "second")
	copy(f.rx[:], "first")
	f.complete(t, second, tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultOK, Endpoint: 2, Slot: 1, Length: 6})
	f.complete(t, first, tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultOK, Endpoint: 1, Slot: 0, Length: 5})
	var reply tcpapi.Reply
	var data [10]byte
	if !f.session.Take(first, data[:], &reply) || string(data[:reply.Length]) != "first" {
		t.Fatal("first lost")
	}
	if f.session.Take(second, data[:2], &reply) {
		t.Fatal("read lease lost on small buffer")
	}
	if !f.session.Take(second, data[:], &reply) || string(data[:reply.Length]) != "second" {
		t.Fatal("second lost")
	}
	if !f.session.Flush() || f.session.Pending() != 0 || f.session.rxOwned != 0 || f.session.release != 0 {
		t.Fatal("leases leaked")
	}
}

func TestTXSlotsAreExclusive(t *testing.T) {
	f := newFixture(t)
	var correlations [tcpapi.TXSlots]uint64
	for slot := range correlations {
		id, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: uint32(slot + 1)}, []byte{byte(slot + 1)})
		if !ok {
			t.Fatal("start", slot)
		}
		correlations[slot] = id
		if f.tx[slot*tcpapi.SlotSize] != byte(slot+1) {
			t.Fatal("slot overwritten")
		}
	}
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: 99}, []byte("x")); ok {
		t.Fatal("TX slot reused early")
	}
	f.complete(t, correlations[2], tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: tcpapi.ResultOK, Endpoint: 3, Slot: 2, Length: 1})
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: 99}, []byte("x")); !ok || f.tx[2*tcpapi.SlotSize] != 'x' {
		t.Fatal("returned TX slot unavailable")
	}
}

func TestReleaseBackpressureRetainsLease(t *testing.T) {
	f := newFixture(t)
	id, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 1}, nil)
	f.complete(t, id, tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultOK, Endpoint: 1, Length: 1})
	var reply tcpapi.Reply
	var data [1]byte
	f.session.Take(id, data[:], &reply)
	f.blocked = true
	if f.session.Flush() || f.session.release != 1 {
		t.Fatal("failed release forgotten")
	}
	f.blocked = false
	if !f.session.Flush() || f.session.release != 0 {
		t.Fatal("lease not released")
	}
}

func TestWriteSendFailureReturnsSlot(t *testing.T) {
	f := newFixture(t)
	f.blocked = true
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: 1}, []byte("x")); ok {
		t.Fatal("failed send accepted")
	}
	if f.session.txOwned != 0 || f.session.Pending() != 0 {
		t.Fatal("failed send leaked")
	}
}

func TestWrongCompletionFailsClosed(t *testing.T) {
	f := newFixture(t)
	id, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 1}, nil)
	var message ipc.Message
	tcpapi.EncodeReply(&tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultOK, Endpoint: 2, Length: 1}, id, &message)
	if f.session.Receive(&message) || !f.session.Failed() {
		t.Fatal("wrong endpoint accepted")
	}
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil); ok {
		t.Fatal("failed session reused")
	}
}

func TestStaleReadDoesNotReleaseReusedSlot(t *testing.T) {
	f := newFixture(t)
	first, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 1}, nil)
	completion := tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultOK, Endpoint: 1, Slot: 0, Length: 1}
	f.complete(t, first, completion)
	var reply tcpapi.Reply
	var data [1]byte
	f.session.Take(first, data[:], &reply)
	f.session.Flush()
	f.session.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 1}, nil)
	var message ipc.Message
	tcpapi.EncodeReply(&completion, first, &message)
	if f.session.Receive(&message) || !f.session.Failed() || f.session.release != 0 {
		t.Fatal("stale reply released a newer lease")
	}
}

func TestReinitializeKeepsCorrelationsUnique(t *testing.T) {
	f := newFixture(t)
	first, _ := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil)
	f.session.Init(f.tx[:], f.rx[:], func(*ipc.Message) bool { return true })
	second, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil)
	if !ok || second <= first {
		t.Fatal("reused correlation", first, second)
	}
}

func TestPendingAndCorrelationBounds(t *testing.T) {
	f := newFixture(t)
	for index := 0; index < MaxPending; index++ {
		if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil); !ok {
			t.Fatal(index)
		}
	}
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil); ok {
		t.Fatal("pending limit bypass")
	}
	f = newFixture(t)
	f.session.next = ^uint64(0)
	if _, ok := f.session.Start(tcpapi.Request{Opcode: tcpapi.OpOpen}, nil); ok {
		t.Fatal("correlation wrap")
	}
}
