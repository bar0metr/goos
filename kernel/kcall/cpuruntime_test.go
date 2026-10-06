package kcall

import "testing"

func TestCPURuntimeRoundTrip(t *testing.T) {
	record := CPURuntime{Index: 3, Online: true, Current: 0x100000007, Ticks: 400, IdleTicks: 100,
		Switches: 5, Steals: 6, Migrations: 7, Reschedules: 8, Shootdowns: 9}
	var wire [CPURuntimeSize]byte
	if !record.Encode(wire[:]) {
		t.Fatal("encode refused")
	}
	decoded, ok := DecodeCPURuntime(wire[:])
	if !ok || decoded != record {
		t.Fatalf("decoded %+v, want %+v", decoded, record)
	}
	if record.Encode(wire[:CPURuntimeSize-1]) {
		t.Fatal("a short buffer was accepted")
	}
	wire[4] = 0x80
	if _, ok := DecodeCPURuntime(wire[:]); ok {
		t.Fatal("unknown flags were accepted")
	}
}

func TestCPURuntimeBusyShare(t *testing.T) {
	for _, test := range []struct {
		ticks, idle, busy uint64
	}{{0, 0, 0}, {100, 100, 0}, {100, 0, 100}, {400, 100, 75}, {10, 50, 0}} {
		if busy := (CPURuntime{Ticks: test.ticks, IdleTicks: test.idle}).Busy(); busy != test.busy {
			t.Fatalf("ticks %d idle %d: busy %d, want %d", test.ticks, test.idle, busy, test.busy)
		}
	}
}
