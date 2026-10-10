package remoteapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func testGeometry(t *testing.T) Geometry {
	t.Helper()
	geometry, ok := MakeGeometry(80, 24, []byte("xterm-256color"))
	if !ok {
		t.Fatal("valid geometry rejected")
	}
	return geometry
}

func TestGeometryBounds(t *testing.T) {
	for _, size := range []uint32{0, 1001, 1 << 16} {
		if _, ok := MakeGeometry(size, 24, []byte("xterm")); ok {
			t.Fatal("invalid columns accepted", size)
		}
		if _, ok := MakeGeometry(80, size, []byte("xterm")); ok {
			t.Fatal("invalid rows accepted", size)
		}
	}
	for _, term := range []string{"", "xterm\n", "xterm\x7f", "\x1b[H"} {
		if _, ok := MakeGeometry(80, 24, []byte(term)); ok {
			t.Fatal("invalid TERM accepted", term)
		}
	}
	if _, ok := MakeGeometry(1, 1000, []byte("vt100")); !ok {
		t.Fatal("edge geometry rejected")
	}
}

func TestRequestRoundTrip(t *testing.T) {
	geometry := testGeometry(t)
	for _, request := range []Request{
		{Operation: OperationOpen, Geometry: geometry},
		{Operation: OperationClose, Session: 1<<32 | 1},
		{Operation: OperationGeometry, Session: 1<<32 | 1, Geometry: geometry},
	} {
		var message ipc.Message
		if !EncodeRequest(&request, 7, &message) {
			t.Fatal("encode", request.Operation)
		}
		var got Request
		if !DecodeRequest(&message, &got) || got != request {
			t.Fatal("round trip", got, request)
		}
	}
}

func TestRequestRejectsMalformed(t *testing.T) {
	request := Request{Operation: OperationOpen, Geometry: testGeometry(t)}
	var message ipc.Message
	EncodeRequest(&request, 7, &message)
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.Kind = ipc.KindReply },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.PayloadSize-- },
		func(m *ipc.Message) { m.Payload[1] = 255 },
		func(m *ipc.Message) { m.Payload[2] = 1 },
		func(m *ipc.Message) { m.Payload[20] = 1 },
		func(m *ipc.Message) { m.Payload[0] = 255 },
		func(m *ipc.Message) { m.Payload[8] = 1 },
	} {
		bad := message
		mutate(&bad)
		var got Request
		if DecodeRequest(&bad, &got) {
			t.Fatal("malformed request accepted", bad)
		}
	}
}

func TestReplyTransferBounds(t *testing.T) {
	reply := Reply{Operation: OperationOpen, Result: ResultOK, Session: 1<<32 | 1}
	var message ipc.Message
	if EncodeReply(&reply, nil, 1, &message) {
		t.Fatal("missing handles accepted")
	}
	if EncodeReply(&reply, []handle.ID{1, 1}, 1, &message) {
		t.Fatal("duplicate handles accepted")
	}
	if !EncodeReply(&reply, []handle.ID{1, 2}, 1, &message) {
		t.Fatal("encode")
	}
	var got Reply
	if !DecodeReply(&message, 1, OperationOpen, &got) || got != reply {
		t.Fatal("decode", got)
	}
	if DecodeReply(&message, 2, OperationOpen, &got) {
		t.Fatal("wrong correlation accepted")
	}
	message.HandleCount = 3
	if DecodeReply(&message, 1, OperationOpen, &got) {
		t.Fatal("extra handle accepted")
	}
	reply.Result, reply.Session = ResultLimit, 0
	if !EncodeReply(&reply, nil, 1, &message) || !DecodeReply(&message, 1, OperationOpen, &got) {
		t.Fatal("error reply rejected")
	}
}

func FuzzDecodeRequest(f *testing.F) {
	geometry, _ := MakeGeometry(80, 24, []byte("xterm"))
	var message ipc.Message
	EncodeRequest(&Request{Operation: OperationOpen, Geometry: geometry}, 1, &message)
	f.Add(append([]byte(nil), message.Payload[:message.PayloadSize]...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > len(message.Payload) {
			return
		}
		m := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 1, PayloadSize: uint16(len(data))}
		copy(m.Payload[:], data)
		var request Request
		if DecodeRequest(&m, &request) {
			var encoded ipc.Message
			if !EncodeRequest(&request, 1, &encoded) {
				t.Fatal("accepted unencodable request")
			}
		}
	})
}
