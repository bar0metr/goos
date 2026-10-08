package netapi

import (
	"goos/kernel/ipc"
	"testing"
)

var (
	gateway = [4]byte{10, 0, 2, 2}
	local   = [4]byte{10, 0, 2, 15}
	station = [6]byte{0x52, 0x54, 0, 0x12, 0x34, 0x56}
)

func requests() []Request {
	return []Request{
		{Opcode: OpStatus},
		{Opcode: OpReload},
		{Opcode: OpEcho, Destination: gateway, Sequence: 0xbeef, TimeoutMS: MinEchoTimeoutMS},
		{Opcode: OpEcho, Destination: [4]byte{8, 8, 8, 8}, TimeoutMS: MaxEchoTimeoutMS},
		{Opcode: OpServers},
		{Opcode: OpServers, Generation: ^uint64(0)},
		{Opcode: OpTCPStatus},
	}
}

func TestRequestRoundTrip(t *testing.T) {
	for _, request := range requests() {
		var wire ipc.Message
		wire.Payload[100] = 1
		if !EncodeRequest(&request, 17, &wire) || wire.Payload[100] != 0 || wire.Kind != ipc.KindRequest {
			t.Fatalf("encode %+v", request)
		}
		var decoded Request
		correlation, ok := DecodeRequest(&wire, &decoded)
		if !ok || correlation != 17 || decoded != request {
			t.Fatalf("decoded %+v", decoded)
		}
		opcode, correlation, ok := RequestOpcode(&wire)
		if !ok || opcode != request.Opcode || correlation != 17 {
			t.Fatal("request opcode")
		}
	}
}

func TestRequestInvalid(t *testing.T) {
	invalid := []Request{
		{},
		{Opcode: lastOpcode + 1},
		{Opcode: OpStatus, Sequence: 1},
		{Opcode: OpStatus, TimeoutMS: 100},
		{Opcode: OpReload, Destination: gateway},
		{Opcode: OpEcho, TimeoutMS: 1000},
		{Opcode: OpEcho, Destination: gateway, TimeoutMS: MinEchoTimeoutMS - 1},
		{Opcode: OpEcho, Destination: gateway, TimeoutMS: MaxEchoTimeoutMS + 1},
		{Opcode: OpEcho, Destination: gateway},
		{Opcode: OpStatus, Generation: 1},
		{Opcode: OpReload, Generation: 1},
		{Opcode: OpEcho, Destination: gateway, TimeoutMS: MinEchoTimeoutMS, Generation: 1},
		{Opcode: OpServers, Destination: gateway},
		{Opcode: OpServers, Sequence: 1},
		{Opcode: OpServers, TimeoutMS: 100},
		{Opcode: OpTCPStatus, Generation: 1},
	}
	for _, request := range invalid {
		var wire ipc.Message
		if EncodeRequest(&request, 1, &wire) {
			t.Fatalf("encoded %+v", request)
		}
	}
	valid := requests()[0]
	var wire ipc.Message
	if EncodeRequest(&valid, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
}

func TestRequestRejects(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID = ControlProtocolID }},
		{"version", func(m *ipc.Message) { m.Version = 0 }},
		{"version 1", func(m *ipc.Message) { m.Version = 1 }},
		{"version 2", func(m *ipc.Message) { m.Version = 2 }},
		{"reply kind", func(m *ipc.Message) { m.Kind = ipc.KindReply }},
		{"notification", func(m *ipc.Message) { m.Kind = ipc.KindNotification }},
		{"no correlation", func(m *ipc.Message) { m.Correlation = 0 }},
		{"short", func(m *ipc.Message) { m.PayloadSize = RequestSize - 1 }},
		{"long", func(m *ipc.Message) { m.PayloadSize = RequestSize + 1 }},
		{"attachment", func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 9 }},
		{"unknown opcode", func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) }},
		{"zero opcode", func(m *ipc.Message) { m.Payload[0] = 0 }},
	}
	for _, request := range requests() {
		for _, mutation := range mutations {
			var wire ipc.Message
			EncodeRequest(&request, 5, &wire)
			mutation.mutate(&wire)
			var decoded Request
			if _, ok := DecodeRequest(&wire, &decoded); ok || decoded != (Request{}) {
				t.Fatalf("%d: accepted %s", request.Opcode, mutation.name)
			}
		}
		reserved := []int{1, 2, 3, 12, 13, 14, 15}
		if request.Opcode != OpEcho {
			reserved = append(reserved, 4, 5, 6, 7, 8, 9, 10, 11)
		}
		if request.Opcode != OpServers {
			reserved = append(reserved, span(requestGeneration, RequestSize)...)
		}
		for _, index := range reserved {
			var wire ipc.Message
			EncodeRequest(&request, 5, &wire)
			wire.Payload[index] = 0x80
			var decoded Request
			if _, ok := DecodeRequest(&wire, &decoded); ok {
				t.Fatalf("%d: accepted nonzero byte %d", request.Opcode, index)
			}
			// Still attributable, so the service can answer it.
			if opcode, _, ok := RequestOpcode(&wire); !ok || opcode != request.Opcode {
				t.Fatalf("%d: byte %d made the request unattributable", request.Opcode, index)
			}
		}
	}
}

func TestRequestOpcodeAttribution(t *testing.T) {
	wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 3, PayloadSize: 200, HandleCount: 2}
	wire.Payload[0] = byte(OpReload)
	if opcode, correlation, ok := RequestOpcode(&wire); !ok || opcode != OpReload || correlation != 3 {
		t.Fatal("oversized request not attributable")
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.PayloadSize = 0 },
		func(m *ipc.Message) { m.Payload[0] = 0 },
		func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) },
		func(m *ipc.Message) { m.Kind = ipc.KindNotification },
		func(m *ipc.Message) { m.ProtocolID = 0 },
	} {
		bad := wire
		mutate(&bad)
		if _, _, ok := RequestOpcode(&bad); ok {
			t.Fatalf("attributed %+v", bad)
		}
	}
}

func replies() []Reply {
	return []Reply{
		{Opcode: OpStatus, State: StateReady, Mode: ModeStatic, Local: local, Gateway: gateway, MAC: station,
			Prefix: 24, Link: LinkUp, Lease: 86400, RX: 1, TX: 2, Drop: 3, Malformed: ^uint64(0), MTU: 1500,
			RateLimited: true, ConflictRun: 4, Probes: 5, ProbeConflicts: 6, Announcements: 7, ActiveConflicts: 8,
			Defenses: 9, Relinquished: 10, RateLimits: 11, Loopback: 12,
			UDPRX: 13, UDPTX: 14, UDPDropNoEndpoint: 15, UDPDropQueueFull: 16, UDPDropChecksum: 17, UDPDropTooLong: 18,
			UDPBindFail: 19, UDPEndpoints: 3, UDPEphemeralMin: 49152, UDPEphemeralMax: 65535, UDPMaxPayload: 4096,
			UDPReceiveQueue: 8, UDPMaxEndpoints: 32, UDPSessionEndpoints: 8},
		{Opcode: OpStatus, State: StateConfiguring, Link: LinkUp, MTU: 576, Fallback: true},
		{Opcode: OpStatus},
		{Opcode: OpStatus, Result: ResultPermissionDenied},
		{Opcode: OpStatus, State: StateFailed, Prefix: MaxPrefix},
		{Opcode: OpEcho, Responder: gateway, RTT: 123456, Sequence: 7},
		{Opcode: OpEcho, Result: ResultTimeout, Sequence: 8},
		{Opcode: OpEcho, Result: ResultRemoteUnreachable, Responder: gateway, Sequence: 9},
		{Opcode: OpEcho, Result: ResultTimeExceeded, Responder: local},
		{Opcode: OpEcho, Result: ResultInvalidArgument},
		{Opcode: OpReload},
		{Opcode: OpReload, Result: ResultConfigInvalid, Field: FieldGateway},
		{Opcode: OpReload, Result: ResultConfigInvalid, Field: FieldProbeMax},
		{Opcode: OpReload, Result: ResultConfigInvalid},
		{Opcode: OpReload, Result: ResultPermissionDenied},
		{Opcode: OpServers, ServerCount: 2, Servers: [MaxServers][4]byte{gateway, local}, Generation: 5},
		{Opcode: OpServers},
		{Opcode: OpServers, Result: ResultPermissionDenied},
		{Opcode: OpTCPStatus, TCPRX: 1, TCPTX: 2, TCPRetransmits: 3, TCPDropChecksum: 4,
			TCPEndpoints: 3, TCPListeners: 1, TCPConnections: 2, TCPStates: [tcpStateCount]uint16{0, 1, 1, 1},
			TCPEphemeralMin: 49152, TCPEphemeralMax: 65535, TCPMaxEndpoints: 32, TCPSessionEndpoints: 8,
			TCPListenBacklog: 8, TCPSendBuffer: 16384, TCPReceiveBuffer: 16384,
			TCPConnectTimeoutMS: 30000, TCPRTOMinMS: 1000, TCPRTOMaxMS: 60000, TCPTimeWaitMS: 60000},
		{Opcode: OpTCPStatus, Result: ResultPermissionDenied},
	}
}

func TestReplyRoundTrip(t *testing.T) {
	for index, reply := range replies() {
		var wire ipc.Message
		if !EncodeReply(&reply, 99, &wire) || wire.Kind != ipc.KindReply || wire.PayloadSize != ReplySize {
			t.Fatalf("reply %d: encode", index)
		}
		var decoded Reply
		if !DecodeReply(&wire, 99, reply.Opcode, &decoded) || decoded != reply {
			t.Fatalf("reply %d: decoded %+v", index, decoded)
		}
	}
}

func TestReplyFieldUsage(t *testing.T) {
	invalid := []Reply{
		{Opcode: 0},
		{Opcode: lastOpcode + 1},
		{Opcode: OpStatus, Result: lastResult + 1},
		{Opcode: OpStatus, State: lastState + 1},
		{Opcode: OpStatus, Mode: lastMode + 1},
		{Opcode: OpStatus, Link: lastLink + 1},
		{Opcode: OpStatus, Prefix: MaxPrefix + 1},
		{Opcode: OpStatus, Field: lastField + 1},
		{Opcode: OpStatus, RTT: 1},
		{Opcode: OpStatus, Responder: gateway},
		{Opcode: OpStatus, Sequence: 1},
		{Opcode: OpStatus, Field: FieldMode},
		{Opcode: OpStatus, Result: ResultBusy, State: StateReady},
		{Opcode: OpStatus, Result: ResultBusy, RX: 1},
		{Opcode: OpEcho},
		{Opcode: OpEcho, Responder: gateway, State: StateReady},
		{Opcode: OpEcho, Responder: gateway, Lease: 1},
		{Opcode: OpEcho, Responder: gateway, MTU: 1500},
		{Opcode: OpEcho, Responder: gateway, Loopback: 1},
		{Opcode: OpReload, RateLimited: true},
		{Opcode: OpReload, Fallback: true},
		{Opcode: OpStatus, Result: ResultBusy, MTU: 1500},
		{Opcode: OpEcho, Responder: gateway, Field: FieldMode},
		{Opcode: OpEcho, Result: ResultTimeout, Responder: gateway},
		{Opcode: OpEcho, Result: ResultTimeout, RTT: 1},
		{Opcode: OpEcho, Result: ResultRemoteUnreachable},
		{Opcode: OpEcho, Result: ResultTimeExceeded, Responder: gateway, RTT: 1},
		{Opcode: OpReload, Field: FieldMode},
		{Opcode: OpReload, Result: ResultBusy, Field: FieldMode},
		{Opcode: OpReload, Sequence: 1},
		{Opcode: OpReload, Result: ResultConfigInvalid, Field: FieldMode, Local: local},
		{Opcode: OpReload, TX: 1},
		{Opcode: OpStatus, ServerCount: 1, Servers: [MaxServers][4]byte{gateway}},
		{Opcode: OpStatus, Generation: 1},
		{Opcode: OpEcho, Responder: gateway, UDPRX: 1},
		{Opcode: OpReload, UDPMaxPayload: 4096},
		{Opcode: OpServers, State: StateReady},
		{Opcode: OpServers, UDPEndpoints: 1},
		{Opcode: OpServers, ServerCount: MaxServers + 1},
		{Opcode: OpServers, ServerCount: 1, Servers: [MaxServers][4]byte{gateway, local}},
		{Opcode: OpServers, Result: ResultBusy, Generation: 1},
	}
	for index, reply := range invalid {
		var wire ipc.Message
		if EncodeReply(&reply, 1, &wire) {
			t.Fatalf("invalid %d encoded: %+v", index, reply)
		}
	}
}

// Flipping any byte of a reply that its opcode and result leave unused must
// make it undecodable.
func TestReplyUnusedBytes(t *testing.T) {
	statusUsed := span(2, 12)
	statusUsed = append(statusUsed, span(24, 40)...)
	statusUsed = append(statusUsed, span(44, 145)...)
	statusUsed = append(statusUsed, span(replyUDP, replyServerN)...)
	used := map[int][]int{
		0:  statusUsed,
		1:  statusUsed,
		2:  statusUsed,
		4:  statusUsed,
		5:  append(span(12, 24), 40, 41),
		6:  {40, 41},
		7:  append(span(12, 16), 40, 41),
		8:  append(span(12, 16), 40, 41),
		9:  {40, 41},
		11: {42},
		12: {42},
		13: {42},
		15: append([]int{replyServerN}, span(replyServerGen, replyServers+8)...),
		16: append([]int{replyServerN}, span(replyServerGen, replyServers)...),
		18: append(append(append(span(replyTCPCounters, replyTCPConnections+2),
			span(replyTCPStates, replyTCPStates+2*tcpStateCount)...),
			span(replyTCPLimits, replyTCPLimits+7)...), span(replyTCPLimits+8, replyTCPLimits+28)...),
	}
	for index, reply := range replies() {
		mask := [ReplySize]bool{0: true, 1: true}
		for _, offset := range used[index] {
			mask[offset] = true
		}
		for offset := 0; offset < ReplySize; offset++ {
			if mask[offset] {
				continue
			}
			var wire ipc.Message
			EncodeReply(&reply, 4, &wire)
			wire.Payload[offset] ^= 0x01
			var decoded Reply
			if DecodeReply(&wire, 4, reply.Opcode, &decoded) {
				t.Fatalf("reply %d: accepted change at byte %d", index, offset)
			}
		}
	}
}

func span(from, to int) []int {
	var out []int
	for index := from; index < to; index++ {
		out = append(out, index)
	}
	return out
}

func TestReplyRejects(t *testing.T) {
	reply := replies()[0]
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version 1", func(m *ipc.Message) { m.Version = 1 }},
		{"version 2", func(m *ipc.Message) { m.Version = 2 }},
		{"version 3", func(m *ipc.Message) { m.Version = 3 }},
		{"version 5", func(m *ipc.Message) { m.Version = 5 }},
		{"request kind", func(m *ipc.Message) { m.Kind = ipc.KindRequest }},
		{"correlation", func(m *ipc.Message) { m.Correlation = 8 }},
		{"short", func(m *ipc.Message) { m.PayloadSize = ReplySize - 1 }},
		{"long", func(m *ipc.Message) { m.PayloadSize = ReplySize + 1 }},
		{"attachment", func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 2 }},
		{"opcode", func(m *ipc.Message) { m.Payload[0] = byte(OpEcho) }},
		{"result", func(m *ipc.Message) { m.Payload[1] = byte(lastResult + 1) }},
		{"state", func(m *ipc.Message) { m.Payload[2] = byte(lastState + 1) }},
		{"mode", func(m *ipc.Message) { m.Payload[3] = byte(lastMode + 1) }},
		{"prefix", func(m *ipc.Message) { m.Payload[30] = MaxPrefix + 1 }},
		{"link", func(m *ipc.Message) { m.Payload[31] = byte(lastLink + 1) }},
		{"field high byte", func(m *ipc.Message) { m.Payload[43] = 1 }},
	}
	for _, mutation := range mutations {
		var wire ipc.Message
		EncodeReply(&reply, 7, &wire)
		mutation.mutate(&wire)
		var decoded Reply
		if DecodeReply(&wire, 7, OpStatus, &decoded) || decoded != (Reply{}) {
			t.Fatalf("accepted %s", mutation.name)
		}
	}
	var wire ipc.Message
	EncodeReply(&reply, 7, &wire)
	var decoded Reply
	if DecodeReply(&wire, 7, OpEcho, &decoded) || DecodeReply(&wire, 0, OpStatus, &decoded) {
		t.Fatal("accepted unexpected opcode or zero correlation")
	}
	if EncodeReply(&reply, 0, &wire) {
		t.Fatal("encoded without correlation")
	}
	wire = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: 7, PayloadSize: ReplySize}
	wire.Payload[0] = byte(OpReload)
	wire.Payload[1] = byte(ResultConfigInvalid)
	wire.Payload[42] = byte(lastField + 1)
	if DecodeReply(&wire, 7, OpReload, &decoded) {
		t.Fatal("accepted unknown field")
	}
	wire.Payload[42] = 0
	wire.Payload[0], wire.Payload[1] = byte(OpStatus), byte(ResultOK)
	wire.Payload[78] = 2
	if DecodeReply(&wire, 7, OpStatus, &decoded) {
		t.Fatal("accepted a fallback flag other than 0 or 1")
	}
}

func TestSaturatingAdd(t *testing.T) {
	if SaturatingAdd(1, 2) != 3 || SaturatingAdd(^uint64(0)-1, 1) != ^uint64(0) ||
		SaturatingAdd(^uint64(0), 1) != ^uint64(0) || SaturatingAdd(^uint64(0)-1, ^uint64(0)) != ^uint64(0) {
		t.Fatal("saturation")
	}
}

func TestText(t *testing.T) {
	for result := ResultOK; result <= lastResult; result++ {
		if result.Text() == "" {
			t.Fatalf("result %d has no text", result)
		}
	}
	if (lastResult+1).Text() != "" || ResultTimeout.Text() != "timeout" || ResultNoRoute.Text() != "no route" ||
		ResultNeighborUnreachable.Text() != "neighbor unreachable" {
		t.Fatal("result text")
	}
	for state := StateDown; state <= lastState; state++ {
		if state.Text() == "" {
			t.Fatalf("state %d has no text", state)
		}
	}
	if (lastState+1).Text() != "" || ModeDHCP.Text() != "dhcp" || ModeStatic.Text() != "static" ||
		(lastMode+1).Text() != "" || LinkUp.Text() != "up" || LinkDown.Text() != "down" || (lastLink+1).Text() != "" {
		t.Fatal("enum text")
	}
	keys := map[Field]string{FieldNone: "", FieldMode: "system.net.mode", FieldAddress: "system.net.ipv4.address",
		FieldPrefix: "system.net.ipv4.prefix", FieldGateway: "system.net.ipv4.gateway",
		FieldTTL: "system.net.ipv4.default-ttl", FieldDHCPMaxRetry: "system.net.dhcp.max-retry-ms",
		FieldProbeMax: "system.net.ipv4.probe-max-ms", lastField + 1: ""}
	seen := map[string]bool{}
	for field := FieldMode; field <= lastField; field++ {
		key := field.Key()
		if len(key) < len("system.net.") || key[:len("system.net.")] != "system.net." || seen[key] {
			t.Fatalf("field %d key %q", field, key)
		}
		seen[key] = true
	}
	for field, key := range keys {
		if field.Key() != key {
			t.Fatalf("field %d key %q", field, field.Key())
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	for _, request := range requests() {
		var wire ipc.Message
		EncodeRequest(&request, 1, &wire)
		f.Add(wire.Payload[:RequestSize])
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 6}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		var decoded Request
		correlation, ok := DecodeRequest(&wire, &decoded)
		if !ok {
			return
		}
		if _, _, attributable := RequestOpcode(&wire); !attributable {
			t.Fatal("decodable request is not attributable")
		}
		var again ipc.Message
		if !EncodeRequest(&decoded, correlation, &again) || again != wire {
			t.Fatalf("%+v does not re-encode", decoded)
		}
	})
}

func FuzzDecodeReply(f *testing.F) {
	for _, reply := range replies() {
		var wire ipc.Message
		EncodeReply(&reply, 1, &wire)
		f.Add(wire.Payload[:ReplySize])
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: 6}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		opcode := OpStatus
		if len(payload) > 0 {
			opcode = Opcode(payload[0])
		}
		var decoded Reply
		if !DecodeReply(&wire, 6, opcode, &decoded) {
			return
		}
		var again ipc.Message
		if !EncodeReply(&decoded, 6, &again) || again != wire {
			t.Fatalf("%+v does not re-encode", decoded)
		}
	})
}
