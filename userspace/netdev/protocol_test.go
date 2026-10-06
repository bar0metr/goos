package netdev

import (
	"goos/kernel/ipc"
	"testing"
)

var station = [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}

type sample struct {
	name        string
	message     Message
	correlation uint64
}

func samples() []sample {
	return []sample{
		{"ready", Message{Opcode: OpReady, Generation: 7, MTU: MTU, MAC: station, Link: LinkUp}, 0},
		{"ready link down", Message{Opcode: OpReady, Generation: 1, MTU: MTU, MAC: station}, 0},
		{"rx ready", Message{Opcode: OpRxReady, Generation: 7, Slot: 3, Length: MaxFrame}, 0},
		{"rx release", Message{Opcode: OpRxRelease, Generation: 7, Slot: 2}, 0},
		{"tx", Message{Opcode: OpTx, Generation: 9, Slot: 1, Length: MinFrame}, 44},
		{"tx done", Message{Opcode: OpTxDone, Generation: 9, Slot: 1, Result: ResultDeviceFault}, 44},
		{"tx done ok", Message{Opcode: OpTxDone, Generation: 9, Slot: 0}, 1},
		{"fault", Message{Opcode: OpFault, Generation: ^uint64(0), Reason: MaxFaultReason}, 0},
	}
}

func TestRoundTrip(t *testing.T) {
	for _, test := range samples() {
		var wire ipc.Message
		wire.Payload[200] = 0xff
		if !Encode(&test.message, test.correlation, &wire) {
			t.Fatalf("%s: encode failed", test.name)
		}
		if wire.Payload[200] != 0 || wire.PayloadSize != PayloadSize || wire.HandleCount != 0 {
			t.Fatalf("%s: stale envelope %+v", test.name, wire)
		}
		var decoded Message
		correlation, ok := Decode(&wire, &decoded)
		if !ok || correlation != test.correlation || decoded != test.message {
			t.Fatalf("%s: decoded %+v, %d, %v", test.name, decoded, correlation, ok)
		}
	}
}

func TestEnvelopeKinds(t *testing.T) {
	want := map[Opcode]ipc.Kind{OpReady: ipc.KindNotification, OpRxReady: ipc.KindNotification,
		OpRxRelease: ipc.KindNotification, OpFault: ipc.KindNotification, OpTx: ipc.KindRequest, OpTxDone: ipc.KindReply}
	for _, test := range samples() {
		var wire ipc.Message
		Encode(&test.message, test.correlation, &wire)
		if wire.Kind != want[test.message.Opcode] {
			t.Fatalf("%s: kind %d", test.name, wire.Kind)
		}
		other := uint64(0)
		if test.correlation == 0 {
			other = 5
		}
		if Encode(&test.message, other, &wire) {
			t.Fatalf("%s: encoded with correlation %d", test.name, other)
		}
	}
}

func encoded(t *testing.T, test sample) ipc.Message {
	t.Helper()
	var wire ipc.Message
	if !Encode(&test.message, test.correlation, &wire) {
		t.Fatalf("%s: encode failed", test.name)
	}
	return wire
}

func TestDecodeRejectsEnvelope(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ipc.Message)
	}{
		{"protocol", func(m *ipc.Message) { m.ProtocolID++ }},
		{"version", func(m *ipc.Message) { m.Version = 2 }},
		{"short", func(m *ipc.Message) { m.PayloadSize = PayloadSize - 1 }},
		{"long", func(m *ipc.Message) { m.PayloadSize = PayloadSize + 1 }},
		{"handle", func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 3 }},
		{"kind zero", func(m *ipc.Message) { m.Kind = 0 }},
		{"kind request", func(m *ipc.Message) { m.Kind = ipc.KindRequest }},
		{"kind reply", func(m *ipc.Message) { m.Kind = ipc.KindReply }},
		{"kind notification", func(m *ipc.Message) { m.Kind = ipc.KindNotification }},
		{"correlation flip", func(m *ipc.Message) {
			if m.Correlation == 0 {
				m.Correlation = 3
			} else {
				m.Correlation = 0
			}
		}},
	}
	for _, test := range samples() {
		for _, mutation := range mutations {
			wire := encoded(t, test)
			before := wire
			mutation.mutate(&wire)
			if wire == before {
				continue
			}
			var decoded Message
			if _, ok := Decode(&wire, &decoded); ok {
				t.Fatalf("%s: accepted %s", test.name, mutation.name)
			}
			if decoded != (Message{}) {
				t.Fatalf("%s: wrote output on failure", test.name)
			}
		}
	}
}

// Every byte a given opcode leaves unused must be zero; flipping any of them
// must fail, while flipping a used byte may succeed or fail by value.
func TestDecodeRejectsUnusedBytes(t *testing.T) {
	used := map[Opcode][]int{
		OpReady:     {0, 8, 9, 10, 11, 12, 13, 14, 15, 22, 23, 24, 25, 26, 27, 28, 29, 30},
		OpRxReady:   {0, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19},
		OpRxRelease: {0, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17},
		OpTx:        {0, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19},
		OpTxDone:    {0, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 20, 21},
		OpFault:     {0, 8, 9, 10, 11, 12, 13, 14, 15, 20, 21},
	}
	for _, test := range samples() {
		mask := [PayloadSize]bool{}
		for _, index := range used[test.message.Opcode] {
			mask[index] = true
		}
		for index := 0; index < PayloadSize; index++ {
			if mask[index] {
				continue
			}
			wire := encoded(t, test)
			wire.Payload[index] ^= 0x01
			var decoded Message
			if _, ok := Decode(&wire, &decoded); ok {
				t.Fatalf("%s: accepted nonzero byte %d", test.name, index)
			}
		}
	}
}

func TestFieldBounds(t *testing.T) {
	broadcast := [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	multicast := [6]byte{0x01, 0x00, 0x5e, 0, 0, 1}
	invalid := []Message{
		{Opcode: 0, Generation: 1},
		{Opcode: OpFault + 1, Generation: 1, Reason: FaultDevice},
		{Opcode: OpReady, MTU: MTU, MAC: station},
		{Opcode: OpReady, Generation: 1, MTU: MTU - 1, MAC: station},
		{Opcode: OpReady, Generation: 1, MTU: MTU + 1, MAC: station},
		{Opcode: OpReady, Generation: 1, MTU: MTU},
		{Opcode: OpReady, Generation: 1, MTU: MTU, MAC: broadcast},
		{Opcode: OpReady, Generation: 1, MTU: MTU, MAC: multicast},
		{Opcode: OpReady, Generation: 1, MTU: MTU, MAC: station, Link: 2},
		{Opcode: OpReady, Generation: 1, MTU: MTU, MAC: station, Slot: 1},
		{Opcode: OpRxReady, Generation: 1, Slot: Slots, Length: MinFrame},
		{Opcode: OpRxReady, Generation: 1, Length: MinFrame - 1},
		{Opcode: OpRxReady, Generation: 1, Length: MaxFrame + 1},
		{Opcode: OpRxReady, Generation: 1, Length: MinFrame, Result: ResultBusy},
		{Opcode: OpRxRelease, Generation: 1, Slot: Slots},
		{Opcode: OpRxRelease, Generation: 1, Length: MinFrame},
		{Opcode: OpRxRelease, Generation: 1, Link: LinkUp},
		{Opcode: OpTx, Generation: 1, Slot: 0xffff, Length: MinFrame},
		{Opcode: OpTx, Generation: 1, Length: 0},
		{Opcode: OpTx, Generation: 1, Length: MinFrame, MAC: station},
		{Opcode: OpTxDone, Generation: 1, Result: lastResult + 1},
		{Opcode: OpTxDone, Generation: 1, Slot: Slots},
		{Opcode: OpTxDone, Generation: 1, Reason: FaultDevice},
		{Opcode: OpTxDone, Generation: 1, Length: MinFrame},
		{Opcode: OpFault, Generation: 1},
		{Opcode: OpFault, Generation: 1, Reason: MaxFaultReason + 1},
		{Opcode: OpFault, Generation: 1, Reason: FaultDevice, Result: ResultBusy},
		{Opcode: OpFault, Generation: 1, Reason: FaultDevice, Slot: 1},
	}
	for index := range invalid {
		correlation := uint64(0)
		if invalid[index].Opcode == OpTx || invalid[index].Opcode == OpTxDone {
			correlation = 1
		}
		var wire ipc.Message
		if Encode(&invalid[index], correlation, &wire) {
			t.Fatalf("encoded invalid %+v", invalid[index])
		}
	}
	// The decoder must reject the same values when they arrive on the wire.
	wire := encoded(t, samples()[0])
	wire.Payload[22] = 0xdd
	var decoded Message
	if _, ok := Decode(&wire, &decoded); ok {
		t.Fatal("accepted MTU 1501+")
	}
	wire = encoded(t, samples()[5])
	wire.Payload[20] = byte(lastResult + 1)
	if _, ok := Decode(&wire, &decoded); ok {
		t.Fatal("accepted unknown result")
	}
	wire = encoded(t, samples()[2])
	wire.Payload[16] = Slots
	if _, ok := Decode(&wire, &decoded); ok {
		t.Fatal("accepted slot out of range")
	}
	wire = encoded(t, samples()[2])
	wire.Payload[8] = 0
	if _, ok := Decode(&wire, &decoded); ok {
		t.Fatal("accepted zero generation")
	}
}

func TestValidMAC(t *testing.T) {
	tests := []struct {
		mac  [6]byte
		want bool
	}{
		{station, true},
		{[6]byte{0x02, 0, 0, 0, 0, 1}, true},
		{[6]byte{}, false},
		{[6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, false},
		{[6]byte{0x33, 0x33, 0, 0, 0, 1}, false},
	}
	for _, test := range tests {
		if ValidMAC(test.mac) != test.want {
			t.Fatalf("ValidMAC(%x) != %v", test.mac, test.want)
		}
	}
}

func TestSlotOffset(t *testing.T) {
	for slot := uint16(0); slot < Slots; slot++ {
		offset, ok := SlotOffset(slot)
		if !ok || offset != uint64(slot)*SlotSize || offset+SlotSize > PoolSize {
			t.Fatalf("slot %d offset %d", slot, offset)
		}
	}
	if _, ok := SlotOffset(Slots); ok {
		t.Fatal("slot out of range accepted")
	}
	if PoolSize != PoolPages*PageSize || MaxFrame > SlotSize {
		t.Fatal("pool constants disagree")
	}
}

func FuzzDecode(f *testing.F) {
	for _, test := range samples() {
		var wire ipc.Message
		Encode(&test.message, test.correlation, &wire)
		f.Add(uint8(wire.Kind), wire.Correlation, wire.Payload[:PayloadSize])
	}
	f.Fuzz(func(t *testing.T, kind uint8, correlation uint64, payload []byte) {
		wire := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.Kind(kind), Correlation: correlation}
		wire.PayloadSize = uint16(copy(wire.Payload[:], payload))
		var decoded Message
		got, ok := Decode(&wire, &decoded)
		if !ok {
			return
		}
		var again ipc.Message
		if !Encode(&decoded, got, &again) || again != wire {
			t.Fatalf("decoded %+v does not re-encode to the same message", decoded)
		}
	})
}
