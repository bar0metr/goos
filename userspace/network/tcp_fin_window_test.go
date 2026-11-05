package network

import "testing"

// A FIN sent while the peer's window is closed is dropped there. The window
// update the peer sends once it reads must leave the FIN's timer armed.
func TestTCPFINAfterClosedWindowArrives(t *testing.T) {
	h := newHarness(t)
	_, client, server := loopbackPair(t, h, 8091)
	data := make([]byte, 4096)
	for {
		written, result := h.stack.TCPWrite(1, client, data, h.now)
		if result == ResultBusy || written == 0 {
			break
		}
		if result != ResultOK {
			t.Fatalf("write %d", result)
		}
	}
	if result := h.stack.TCPShutdownWrite(1, client, h.now); result != ResultOK {
		t.Fatalf("shutdown %d", result)
	}
	buffer := make([]byte, 4096)
	for round := 0; round < 4000; round++ {
		read, result := h.stack.TCPRead(0, server, buffer, h.now)
		if result == ResultEOF {
			return
		}
		if result != ResultOK && result != ResultBusy {
			t.Fatalf("read %d", result)
		}
		if read == 0 {
			h.now += 10 * millisecond
			h.stack.Advance(h.now)
		}
	}
	t.Fatal("the FIN never arrived")
}
