package network

import "testing"

// lifecycleRun is one netbench pass of one owner on a fresh ephemeral
// listener: rounds connections opened, accepted and closed client first.
func lifecycleRun(t *testing.T, h *harness, owner int, rounds int) {
	t.Helper()
	listener, result := h.stack.TCPOpen(owner)
	if result != ResultOK {
		t.Fatalf("listener %d", result)
	}
	_, port, result := h.stack.TCPBind(owner, listener, LoopbackAddress, 0)
	if result != ResultOK {
		t.Fatalf("bind %d", result)
	}
	h.stack.TCPListen(owner, listener, 8)
	for round := 0; round < rounds; round++ {
		client, result := h.stack.TCPOpen(owner)
		if result != ResultOK {
			t.Fatalf("owner %d round %d open %d", owner, round, result)
		}
		if _, result := h.stack.TCPConnect(owner, client, LoopbackAddress, port, h.now); result != ResultOK {
			t.Fatalf("owner %d round %d connect %d", owner, round, result)
		}
		if _, result, done := h.stack.TCPConnectResult(owner, client); !done || result != ResultOK {
			t.Fatalf("owner %d round %d port %d: connect done=%v result %d", owner, round, port, done, result)
		}
		child, _, result := h.stack.TCPAccept(owner, listener)
		if result != ResultOK {
			t.Fatalf("owner %d round %d accept %d", owner, round, result)
		}
		h.stack.TCPClose(owner, client, h.now)
		h.stack.TCPClose(owner, child, h.now)
		h.advance(1_000_000)
	}
	h.stack.TCPClose(owner, listener, h.now)
}

// TestLoopbackLifecycleAfterAnotherCommand runs one command's connections,
// then another's on a new ephemeral listener while the first's TIME_WAIT
// tuples remain.
func TestLoopbackLifecycleAfterAnotherCommand(t *testing.T) {
	h := newHarness(t)
	for owner := 1; owner <= 4; owner++ {
		lifecycleRun(t, h, owner, 30)
	}
}

// TestLoopbackLifecycleOfAReusedOwner runs a command's connections, ends
// its session, and runs a second command under the same owner index, as
// netd gives a new session the slot of one that ended.
var passes = 3

func TestLoopbackLifecycleOfAReusedOwner(t *testing.T) {
	h := newHarness(t)
	for pass := 0; pass < passes; pass++ {
		if !h.stack.SetTCPQuota(1, 8) {
			t.Fatal("quota")
		}
		lifecycleRun(t, h, 1, 30)
		h.stack.TCPCloseOwner(1, h.now)
		h.stack.SetTCPQuota(1, 0)
	}
}
