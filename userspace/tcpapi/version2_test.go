package tcpapi

import (
	"testing"

	"goos/kernel/ipc"
)

func wireRoundTrip(t *testing.T, message *ipc.Message) ipc.Message {
	t.Helper()
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	var back ipc.Message
	if !ok || !ipc.DecodeInto(wire[:size], &back) {
		t.Fatalf("wire round trip of %+v", message)
	}
	return back
}

func TestVersion2RequestRoundTrip(t *testing.T) {
	for _, request := range []Request{
		{Opcode: OpBind, Endpoint: 1, Port: 80, Flags: FlagReuseAddr},
		{Opcode: OpAccept, Endpoint: 1, Flags: FlagNoWait},
		{Opcode: OpConnect, Endpoint: 1, Address: [4]byte{10, 0, 0, 1}, Port: 80, Flags: FlagNoWait},
		{Opcode: OpRead, Endpoint: 1, Length: 10, Flags: FlagNoWait | FlagPeek},
		{Opcode: OpWrite, Endpoint: 1, Slot: 3, Length: 10, Flags: FlagNoWait},
		{Opcode: OpReady, Endpoint: 1},
		{Opcode: OpWaitReady, Endpoint: 1, Mask: ReadyConnected | ReadyWritable},
		{Opcode: OpWaitReady, Endpoint: 1},
		{Opcode: OpError, Endpoint: 1},
		{Opcode: OpOption, Endpoint: 1, Option: OptionTTL},
	} {
		var message ipc.Message
		if !EncodeRequest(&request, 3, &message) {
			t.Fatalf("encode %+v", request)
		}
		back := wireRoundTrip(t, &message)
		var decoded Request
		if correlation, ok := DecodeRequest(&back, &decoded); !ok || correlation != 3 || decoded != request {
			t.Fatalf("decode %+v: %+v %v", request, decoded, ok)
		}
	}
}

func TestVersion2RequestRejectsForeignFields(t *testing.T) {
	for _, request := range []Request{
		{Opcode: OpOpen, Flags: FlagNoWait},
		{Opcode: OpBind, Endpoint: 1, Flags: FlagNoWait},
		{Opcode: OpAccept, Endpoint: 1, Flags: FlagPeek},
		{Opcode: OpWrite, Endpoint: 1, Flags: FlagPeek},
		{Opcode: OpRead, Endpoint: 1, Length: 1, Flags: FlagReuseAddr},
		{Opcode: OpRead, Endpoint: 1, Length: 1, Flags: 1 << 7},
		{Opcode: OpClose, Endpoint: 1, Flags: FlagNoWait},
		{Opcode: OpReady, Endpoint: 1, Mask: 1},
		{Opcode: OpReady},
		{Opcode: OpWaitReady, Endpoint: 1, Mask: ReadyAll + 1},
		{Opcode: OpOption, Endpoint: 1},
		{Opcode: OpOption, Endpoint: 1, Option: lastOption + 1},
		{Opcode: OpError, Endpoint: 1, Option: OptionTTL},
		{Opcode: OpError, Endpoint: 1, Length: 1},
		{Opcode: lastOpcode + 1, Endpoint: 1},
	} {
		var message ipc.Message
		if EncodeRequest(&request, 3, &message) {
			t.Fatalf("encoded %+v", request)
		}
	}
	var message ipc.Message
	EncodeRequest(&Request{Opcode: OpReady, Endpoint: 1}, 3, &message)
	for _, at := range []int{2, 3, 21, 22, 23} {
		bad := message
		bad.Payload[at] = 1
		var decoded Request
		if _, ok := DecodeRequest(&bad, &decoded); ok {
			t.Fatalf("reserved byte %d accepted", at)
		}
	}
}

func TestVersion2ReplyRoundTrip(t *testing.T) {
	local, remote := [4]byte{127, 0, 0, 1}, [4]byte{127, 0, 0, 2}
	for _, reply := range []Reply{
		{Opcode: OpRead, Result: ResultWouldBlock, Endpoint: 1},
		{Opcode: OpAccept, Result: ResultWouldBlock, Endpoint: 1},
		{Opcode: OpWrite, Result: ResultWouldBlock, Endpoint: 1, Slot: 2},
		{Opcode: OpWrite, Result: ResultOK, Endpoint: 1, Slot: 2, Length: 7},
		{Opcode: OpConnect, Result: ResultInProgress, Endpoint: 1, Address: remote, Port: 80, Local: local, LocalPort: 50000},
		{Opcode: OpReady, Result: ResultOK, Endpoint: 1, Mask: ReadyConnected | ReadyReadable | ReadyWritable, Value: 16384, Room: 1},
		{Opcode: OpReady, Result: ResultOK, Endpoint: 1},
		{Opcode: OpWaitReady, Result: ResultOK, Endpoint: 1, Mask: ReadyHangUp},
		{Opcode: OpWaitReady, Result: ResultClosed, Endpoint: 1},
		{Opcode: OpWaitReady, Result: ResultBusy, Endpoint: 1},
		{Opcode: OpError, Result: ResultOK, Endpoint: 1, Error: ResultConnectionRefused},
		{Opcode: OpError, Result: ResultOK, Endpoint: 1},
		{Opcode: OpOption, Result: ResultOK, Endpoint: 1, Value: 1460},
		{Opcode: OpOption, Result: ResultInvalidArgument, Endpoint: 1},
	} {
		var message ipc.Message
		if !EncodeReply(&reply, 5, &message) {
			t.Fatalf("encode %+v", reply)
		}
		back := wireRoundTrip(t, &message)
		var decoded Reply
		if correlation, ok := DecodeReply(&back, &decoded); !ok || correlation != 5 || decoded != reply {
			t.Fatalf("decode %+v: %+v %v", reply, decoded, ok)
		}
	}
}

func TestVersion2ReplyRejectsMisplacedResults(t *testing.T) {
	local := [4]byte{127, 0, 0, 1}
	for _, reply := range []Reply{
		{Opcode: OpConnect, Result: ResultWouldBlock, Endpoint: 1},
		{Opcode: OpRead, Result: ResultInProgress, Endpoint: 1},
		{Opcode: OpConnect, Result: ResultInProgress, Endpoint: 1},
		{Opcode: OpReady, Result: ResultEOF, Endpoint: 1},
		{Opcode: OpReady, Result: ResultOK, Endpoint: 1, Mask: ReadyAll + 1},
		{Opcode: OpReady, Result: ResultOK, Endpoint: 1, Error: ResultReset},
		{Opcode: OpWaitReady, Result: ResultClosed, Endpoint: 1, Mask: ReadyReadable},
		{Opcode: OpError, Result: ResultInvalidArgument, Endpoint: 1, Error: ResultReset},
		{Opcode: OpError, Result: ResultOK, Endpoint: 1, Error: lastResult + 1},
		{Opcode: OpOption, Result: ResultOK, Endpoint: 1, Mask: 1},
		{Opcode: OpRead, Result: ResultOK, Endpoint: 1, Length: 1, Value: 1},
		{Opcode: OpWrite, Result: ResultOK, Endpoint: 1, Room: 1},
		{Opcode: OpBind, Result: ResultOK, Endpoint: 1, Port: 1, Mask: 1},
		{Opcode: OpReady, Result: ResultOK, Endpoint: 1, Local: local},
		{Opcode: OpReady, Result: ResultOK},
	} {
		var message ipc.Message
		if EncodeReply(&reply, 5, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
}

func TestVersionMismatchFailsClosed(t *testing.T) {
	var message ipc.Message
	EncodeRequest(&Request{Opcode: OpOpen}, 1, &message)
	message.Version = 1
	var request Request
	if _, ok := DecodeRequest(&message, &request); ok {
		t.Fatal("version 1 request accepted")
	}
	EncodeReply(&Reply{Opcode: OpOpen, Endpoint: 1}, 1, &message)
	message.Version = 1
	var reply Reply
	if _, ok := DecodeReply(&message, &reply); ok {
		t.Fatal("version 1 reply accepted")
	}
	EncodeReply(&Reply{Opcode: OpOpen, Endpoint: 1}, 1, &message)
	message.PayloadSize = 24
	if _, ok := DecodeReply(&message, &reply); ok {
		t.Fatal("version 1 reply size accepted")
	}
}
