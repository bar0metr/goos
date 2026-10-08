package tcpapi

import (
	"goos/kernel/ipc"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	tests := []Request{
		{Opcode: OpOpen},
		{Opcode: OpBind, Endpoint: 1, Address: [4]byte{127, 0, 0, 1}, Port: 80},
		{Opcode: OpListen, Endpoint: 1, Backlog: 4},
		{Opcode: OpAccept, Endpoint: 1},
		{Opcode: OpConnect, Endpoint: 2, Address: [4]byte{192, 0, 2, 1}, Port: 443},
		{Opcode: OpWrite, Endpoint: 2, Slot: 1, Length: SlotSize},
		{Opcode: OpRead, Endpoint: 2, Length: 100},
		{Opcode: OpShutdownWrite, Endpoint: 2},
		{Opcode: OpClose, Endpoint: 2},
		{Opcode: OpRelease, Slot: 3},
	}
	for _, request := range tests {
		correlation := uint64(9)
		if request.Opcode == OpRelease {
			correlation = 0
		}
		var message ipc.Message
		if !EncodeRequest(&request, correlation, &message) {
			t.Fatalf("encode opcode %d", request.Opcode)
		}
		var decoded Request
		got, ok := DecodeRequest(&message, &decoded)
		if !ok || got != correlation || decoded != request {
			t.Fatalf("decode opcode %d: %+v %d %v", request.Opcode, decoded, got, ok)
		}
	}
}

func TestReplyRoundTrip(t *testing.T) {
	replies := []Reply{
		{Opcode: OpOpen, Result: ResultOK, Endpoint: 1},
		{Opcode: OpAccept, Result: ResultOK, Endpoint: 2, Address: [4]byte{127, 0, 0, 1}, Port: 50000, Local: [4]byte{127, 0, 0, 2}, LocalPort: 80},
		{Opcode: OpWrite, Result: ResultBusy, Endpoint: 2, Slot: 1},
		{Opcode: OpRead, Result: ResultOK, Endpoint: 2, Slot: 2, Length: 12},
		{Opcode: OpRead, Result: ResultEOF, Endpoint: 2},
		{Opcode: OpOpen, Result: ResultBusy},
		{Opcode: OpOpen, Result: ResultInvalidArgument},
	}
	for _, reply := range replies {
		var message ipc.Message
		if !EncodeReply(&reply, 7, &message) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded Reply
		correlation, ok := DecodeReply(&message, &decoded)
		if !ok || correlation != 7 || decoded != reply {
			t.Fatalf("decode %+v %d %v", decoded, correlation, ok)
		}
	}
}

func TestReplyEndpointRules(t *testing.T) {
	for _, reply := range []Reply{
		{Opcode: OpOpen, Result: ResultOK},
		{Opcode: OpOpen, Result: ResultBusy, Endpoint: 1},
		{Opcode: OpBind, Result: ResultBusy},
		{Opcode: OpRead, Result: ResultEOF},
		{Opcode: OpWrite, Result: ResultBusy},
	} {
		var message ipc.Message
		if EncodeReply(&reply, 7, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
}
