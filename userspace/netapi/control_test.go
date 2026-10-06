package netapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

type controlSample struct {
	request     ControlRequest
	attachments []handle.ID
}

func controlSamples() []controlSample {
	return []controlSample{
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsPingStatus}, []handle.ID{12}},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsAdminStatus}, []handle.ID{13}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: 1}, []handle.ID{20, 21, 22}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: ^uint64(0)}, []handle.ID{20, 21, 22}},
	}
}

func TestControlRequestRoundTrip(t *testing.T) {
	for _, sample := range controlSamples() {
		var wire ipc.Message
		if !EncodeControlRequest(&sample.request, sample.attachments, 31, &wire) {
			t.Fatalf("encode %+v", sample.request)
		}
		var decoded ControlRequest
		correlation, ok := DecodeControlRequest(&wire, &decoded)
		if !ok || correlation != 31 || decoded != sample.request || int(wire.HandleCount) != len(sample.attachments) {
			t.Fatalf("decoded %+v", decoded)
		}
		for index, attachment := range sample.attachments {
			if wire.Handles[index] != attachment {
				t.Fatal("attachment order")
			}
		}
		if opcode, correlation, ok := ControlRequestOpcode(&wire); !ok || opcode != sample.request.Opcode || correlation != 31 {
			t.Fatal("control opcode")
		}
	}
}

func TestControlRequestInvalid(t *testing.T) {
	invalid := []controlSample{
		{ControlRequest{}, nil},
		{ControlRequest{Opcode: ControlSetDevice + 1, Generation: 1}, []handle.ID{1}},
		{ControlRequest{Opcode: ControlAddSession}, []handle.ID{1}},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsAdminStatus + 1}, []handle.ID{1}},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsPingStatus, Generation: 1}, []handle.ID{1}},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsPingStatus}, nil},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsPingStatus}, []handle.ID{1, 2}},
		{ControlRequest{Opcode: ControlAddSession, Rights: RightsPingStatus}, []handle.ID{0}},
		{ControlRequest{Opcode: ControlSetDevice}, []handle.ID{1, 2, 3}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: 1, Rights: RightsPingStatus}, []handle.ID{1, 2, 3}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: 1}, []handle.ID{1, 2}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: 1}, []handle.ID{1, 2, 3, 4}},
		{ControlRequest{Opcode: ControlSetDevice, Generation: 1}, []handle.ID{1, 0, 3}},
	}
	for index, sample := range invalid {
		var wire ipc.Message
		if EncodeControlRequest(&sample.request, sample.attachments, 1, &wire) {
			t.Fatalf("invalid %d encoded", index)
		}
	}
	valid := controlSamples()[0]
	var wire ipc.Message
	if EncodeControlRequest(&valid.request, valid.attachments, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
}

func TestControlRequestRejects(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID = ProtocolID }},
		{"version", func(m *ipc.Message) { m.Version = 2 }},
		{"kind", func(m *ipc.Message) { m.Kind = ipc.KindNotification }},
		{"correlation", func(m *ipc.Message) { m.Correlation = 0 }},
		{"short", func(m *ipc.Message) { m.PayloadSize = ControlSize - 1 }},
		{"long", func(m *ipc.Message) { m.PayloadSize = ControlSize + 1 }},
		{"one fewer handle", func(m *ipc.Message) { m.HandleCount-- }},
		{"one more handle", func(m *ipc.Message) { m.Handles[m.HandleCount] = 9; m.HandleCount++ }},
		{"handle count overflow", func(m *ipc.Message) { m.HandleCount = 200 }},
		{"zero handle", func(m *ipc.Message) { m.Handles[0] = 0 }},
		{"opcode", func(m *ipc.Message) { m.Payload[0] = byte(ControlSetDevice + 1) }},
		{"rights", func(m *ipc.Message) { m.Payload[1] = byte(RightsAdminStatus + 1) }},
		{"generation", func(m *ipc.Message) {
			if m.Payload[0] == byte(ControlAddSession) {
				m.Payload[8] = 1
			} else {
				copy(m.Payload[controlGeneration:ControlSize], make([]byte, 8))
			}
		}},
	}
	for _, sample := range controlSamples() {
		for _, mutation := range mutations {
			var wire ipc.Message
			EncodeControlRequest(&sample.request, sample.attachments, 2, &wire)
			mutation.mutate(&wire)
			var decoded ControlRequest
			if _, ok := DecodeControlRequest(&wire, &decoded); ok || decoded != (ControlRequest{}) {
				t.Fatalf("%d: accepted %s", sample.request.Opcode, mutation.name)
			}
		}
		for index := controlReserved; index < controlGeneration; index++ {
			var wire ipc.Message
			EncodeControlRequest(&sample.request, sample.attachments, 2, &wire)
			wire.Payload[index] = 1
			var decoded ControlRequest
			if _, ok := DecodeControlRequest(&wire, &decoded); ok {
				t.Fatalf("%d: accepted nonzero byte %d", sample.request.Opcode, index)
			}
			if _, _, ok := ControlRequestOpcode(&wire); !ok {
				t.Fatalf("%d: byte %d made the request unattributable", sample.request.Opcode, index)
			}
		}
	}
}

func TestControlReply(t *testing.T) {
	for opcode := ControlAddSession; opcode <= ControlSetDevice; opcode++ {
		for result := ControlOK; result <= lastControlResult; result++ {
			reply := ControlReply{Opcode: opcode, Result: result, Generation: uint64(opcode) * 5}
			var wire ipc.Message
			if !EncodeControlReply(&reply, 3, &wire) || wire.Kind != ipc.KindReply || wire.HandleCount != 0 {
				t.Fatal("encode")
			}
			var decoded ControlReply
			if !DecodeControlReply(&wire, 3, opcode, &decoded) || decoded != reply {
				t.Fatalf("decoded %+v", decoded)
			}
			if DecodeControlReply(&wire, 4, opcode, &decoded) || DecodeControlReply(&wire, 3, opcode%2+1, &decoded) {
				t.Fatal("accepted mismatched correlation or opcode")
			}
		}
	}
	bad := []ControlReply{{}, {Opcode: ControlSetDevice + 1}, {Opcode: ControlAddSession, Result: lastControlResult + 1}}
	for _, reply := range bad {
		var wire ipc.Message
		if EncodeControlReply(&reply, 1, &wire) {
			t.Fatalf("encoded %+v", reply)
		}
	}
	reply := ControlReply{Opcode: ControlSetDevice, Generation: 4}
	var wire ipc.Message
	if EncodeControlReply(&reply, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
	mutations := []func(*ipc.Message){
		func(m *ipc.Message) { m.ProtocolID = ProtocolID },
		func(m *ipc.Message) { m.Version = 0 },
		func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		func(m *ipc.Message) { m.PayloadSize = ControlSize + 1 },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.Payload[1] = byte(lastControlResult + 1) },
	}
	for index := controlReserved; index < controlGeneration; index++ {
		offset := index
		mutations = append(mutations, func(m *ipc.Message) { m.Payload[offset] = 1 })
	}
	for index, mutate := range mutations {
		EncodeControlReply(&reply, 3, &wire)
		mutate(&wire)
		var decoded ControlReply
		if DecodeControlReply(&wire, 3, ControlSetDevice, &decoded) || decoded != (ControlReply{}) {
			t.Fatalf("mutation %d accepted", index)
		}
	}
}

func FuzzDecodeControlRequest(f *testing.F) {
	for _, sample := range controlSamples() {
		var wire ipc.Message
		EncodeControlRequest(&sample.request, sample.attachments, 1, &wire)
		f.Add(wire.Payload[:ControlSize], wire.HandleCount)
	}
	f.Fuzz(func(t *testing.T, payload []byte, count uint8) {
		wire := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: 6,
			HandleCount: count, Handles: [ipc.MaxAttachedHandles]handle.ID{40, 41, 42, 43}}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		var decoded ControlRequest
		correlation, ok := DecodeControlRequest(&wire, &decoded)
		if !ok {
			return
		}
		var again ipc.Message
		if !EncodeControlRequest(&decoded, wire.Handles[:wire.HandleCount], correlation, &again) ||
			again.Payload != wire.Payload || again.HandleCount != wire.HandleCount || again.PayloadSize != wire.PayloadSize {
			t.Fatalf("%+v does not re-encode", decoded)
		}
	})
}
