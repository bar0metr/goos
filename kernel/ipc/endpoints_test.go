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

func TestProcessEndpointsLinkAndUnbind(t *testing.T) {
	var table EndpointTable
	if !table.BindProcess(9, 100, 2) || !table.BindProcess(9, 200, 1) {
		t.Fatal("failed to bind process endpoints")
	}
	if side, peer, ok := table.Lookup(9, 100); !ok || side != 1 || peer != 200 {
		t.Fatalf("server endpoint = %d, %d, %v", side, peer, ok)
	}
	if side, peer, ok := table.Lookup(9, 200); !ok || side != 0 || peer != 100 {
		t.Fatalf("client endpoint = %d, %d, %v", side, peer, ok)
	}
	table.Unbind(9, 200)
	if _, _, ok := table.Lookup(9, 200); ok {
		t.Fatal("unbound endpoint remains visible")
	}
	if _, peer, ok := table.Lookup(9, 100); !ok || peer != 0 {
		t.Fatal("peer link was not cleared")
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
