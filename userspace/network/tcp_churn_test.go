package network

import "testing"

// One owner with the linuxsockd quota opens, connects, accepts and closes
// loopback connections far past its quota: closed connections must give
// their endpoints back, TIME_WAIT included.
func TestTCPLoopbackChurnStaysWithinQuota(t *testing.T) {
	h := newHarness(t)
	if !h.stack.SetTCPQuota(0, MaxTCPSessionQuota) {
		t.Fatal("quota")
	}
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8080); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 8); result != ResultOK {
		t.Fatal(result)
	}
	for round := 0; round < 4*MaxTCPSessionQuota; round++ {
		client, result := h.stack.TCPOpen(0)
		if result != ResultOK {
			t.Fatalf("round %d: open %d, endpoints %d", round, result, h.stack.TCPStats().Endpoints)
		}
		if _, result = h.stack.TCPConnect(0, client, LoopbackAddress, 8080, h.now); result != ResultOK {
			t.Fatalf("round %d: connect %d", round, result)
		}
		server, _, result := h.stack.TCPAccept(0, listener)
		if result != ResultOK {
			t.Fatalf("round %d: accept %d", round, result)
		}
		h.stack.TCPClose(0, client, h.now)
		h.stack.TCPClose(0, server, h.now)
	}
}
