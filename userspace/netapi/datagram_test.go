package netapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

var server = [4]byte{10, 0, 2, 3}

func datagrams() []Datagram {
	return []Datagram{
		{Opcode: DatagramSend, Slot: 7, Address: server, Length: DatagramMaxPayload, Port: DatagramPort, TimeoutMS: 3000, Tag: 9},
		{Opcode: DatagramAccepted, Slot: 7, Tag: 9},
		{Opcode: DatagramAccepted, Result: ResultNoAddress, Slot: 1, Tag: 9},
		{Opcode: DatagramResult, Slot: 3, Address: server, Length: 40, Port: DatagramPort, Tag: 9},
		{Opcode: DatagramResult, Result: ResultTimeout, Tag: 9},
		{Opcode: DatagramRelease, Slot: 3},
		{Opcode: DatagramServers, Count: 2, Generation: 4, Servers: [MaxServers][4]byte{server, {10, 0, 2, 4}}},
		{Opcode: DatagramServers, Generation: 5},
	}
}

func TestDatagramRoundTrip(t *testing.T) {
	for index, datagram := range datagrams() {
		var wire ipc.Message
		if !EncodeDatagram(&datagram, &wire) {
			t.Fatalf("%d: encode", index)
		}
		// The kernel's own rules must accept it: a notification carries no
		// correlation.
		var raw [ipc.WireMaxSize]byte
		size, ok := ipc.EncodeFrom(&wire, raw[:])
		var copied ipc.Message
		if !ok || !ipc.DecodeInto(raw[:size], &copied) {
			t.Fatalf("%d: not a valid IPC message", index)
		}
		var decoded Datagram
		if !DecodeDatagram(&copied, &decoded) || decoded != datagram {
			t.Fatalf("%d: decoded %+v", index, decoded)
		}
	}
}

func TestDatagramRefused(t *testing.T) {
	for index, datagram := range []Datagram{
		{},
		{Opcode: lastDatagramOpcode + 1},
		{Opcode: DatagramSend, Address: server, Length: 1, Port: 54, TimeoutMS: 1, Tag: 1},
		{Opcode: DatagramSend, Address: server, Length: DatagramMaxPayload + 1, Port: DatagramPort, TimeoutMS: 1, Tag: 1},
		{Opcode: DatagramSend, Address: server, Length: 1, Port: DatagramPort, TimeoutMS: MaxDatagramTimeoutMS + 1, Tag: 1},
		{Opcode: DatagramSend, Address: server, Length: 1, Port: DatagramPort, TimeoutMS: 1},
		{Opcode: DatagramSend, Slot: DatagramSlots, Address: server, Length: 1, Port: DatagramPort, TimeoutMS: 1, Tag: 1},
		{Opcode: DatagramAccepted},
		{Opcode: DatagramResult, Result: ResultTimeout, Slot: 1, Tag: 1},
		{Opcode: DatagramResult, Tag: 1, Address: server, Port: DatagramPort},
		{Opcode: DatagramRelease, Tag: 1},
		{Opcode: DatagramServers},
		{Opcode: DatagramServers, Count: MaxServers + 1, Generation: 1},
		{Opcode: DatagramServers, Count: 1, Generation: 1, Servers: [MaxServers][4]byte{server, server}},
	} {
		var wire ipc.Message
		if EncodeDatagram(&datagram, &wire) {
			t.Fatalf("%d: encoded %+v", index, datagram)
		}
	}
	valid := datagrams()[0]
	var wire ipc.Message
	EncodeDatagram(&valid, &wire)
	for name, mutate := range map[string]func(*ipc.Message){
		"protocol":    func(m *ipc.Message) { m.ProtocolID++ },
		"version":     func(m *ipc.Message) { m.Version++ },
		"kind":        func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		"size":        func(m *ipc.Message) { m.PayloadSize-- },
		"handle":      func(m *ipc.Message) { m.HandleCount = 1 },
		"correlation": func(m *ipc.Message) { m.Correlation = 1 },
		"reserved":    func(m *ipc.Message) { m.Payload[datagramReserved] = 1 },
	} {
		bad := wire
		mutate(&bad)
		var decoded Datagram
		if DecodeDatagram(&bad, &decoded) {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, ok := DatagramSlotOffset(DatagramSlots); ok {
		t.Fatal("slot past the pool")
	}
	if DatagramSlots*DatagramSlotSize > DatagramPoolPages*4096 || DatagramSlotSize < DatagramMaxPayload {
		t.Fatal("pool layout does not fit")
	}
}

func TestSetTransportControl(t *testing.T) {
	var wire ipc.Message
	request := ControlRequest{Opcode: ControlSetTransport, Generation: 3}
	if !EncodeControlRequest(&request, []handle.ID{1, 2, 3}[:], 5, &wire) {
		t.Fatal("encode")
	}
	var decoded ControlRequest
	if correlation, ok := DecodeControlRequest(&wire, &decoded); !ok || correlation != 5 || decoded != request {
		t.Fatalf("decoded %+v", decoded)
	}
	session := ControlRequest{Opcode: ControlAddSession, Rights: RightsResolveStatus}
	if !EncodeControlRequest(&session, []handle.ID{4}[:], 6, &wire) {
		t.Fatal("resolve session refused")
	}
	session.Rights = RightsResolveStatus + 1
	if EncodeControlRequest(&session, []handle.ID{4}[:], 7, &wire) {
		t.Fatal("unknown rights class accepted")
	}
}
