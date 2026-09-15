package ipc

import "testing"

func TestEndpointBindingsAreScopedByObjectAndProcess(t *testing.T) {
	var table EndpointTable
	if !table.Bind(7, 0x1000, 0x2000, 0) || !table.Bind(7, 0x2000, 0x1000, 1) || table.Bind(7, 0x1000, 0, 1) {
		t.Fatal("valid binding rejected or duplicate accepted")
	}
	side, peer, ok := table.Lookup(7, 0x2000)
	if !ok || side != 1 || peer != 0x1000 {
		t.Fatalf("lookup = %d %#x %v", side, peer, ok)
	}
	if _, _, ok := table.Lookup(8, 0x2000); ok || table.Bind(0, 1, 0, 0) || table.Bind(1, 0, 0, 0) || table.Bind(1, 1, 0, 2) {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestEndpointTableHoldsBothSidesOfEveryChannel(t *testing.T) {
	var table EndpointTable
	for index := uint64(1); index <= MaxChannels; index++ {
		if !table.Bind(index, index*2, index*2+1, 0) || !table.Bind(index, index*2+1, index*2, 1) {
			t.Fatalf("channel %d did not fit", index)
		}
	}
	if table.Bind(MaxChannels+1, 100, 101, 0) {
		t.Fatal("endpoint capacity exceeded")
	}
}
