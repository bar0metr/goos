package network

import (
	"testing"
)

// budgetFlow is one loopback pair streaming total bytes each way.
type budgetFlow struct {
	client, server        TCPEndpointID
	sent, received        [2]int
	total                 int
	clientOwner, srvOwner int
}

// streamLoopbackPairs opens pairs loopback connections, servers on owner 0
// and clients on owner 1, and streams perFlow bytes each way on every pair
// at once. The clock never moves: every byte has to arrive through writes,
// reads and the segments they cause, never through a timer. It fails when
// a whole pass over the pairs moves no byte while bytes remain.
func streamLoopbackPairs(t *testing.T, h *harness, pairs, perFlow int) []*budgetFlow {
	t.Helper()
	for owner := range 2 {
		h.stack.SetTCPQuota(owner, MaxTCPSessionQuota)
	}
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 7100); result != ResultOK {
		t.Fatalf("bind %d", result)
	}
	if result := h.stack.TCPListen(0, listener, MaxTCPBacklog); result != ResultOK {
		t.Fatalf("listen %d", result)
	}
	flows := make([]*budgetFlow, pairs)
	for index := range flows {
		client, _ := h.stack.TCPOpen(1)
		if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 7100, h.now); result != ResultOK {
			t.Fatalf("connection %d connect %d", index, result)
		}
		server, _, result := h.stack.TCPAccept(0, listener)
		if result != ResultOK {
			t.Fatalf("connection %d accept %d", index, result)
		}
		flows[index] = &budgetFlow{client: client, server: server, total: perFlow + index*37, clientOwner: 1}
	}
	start := h.now
	out := make([]byte, 64<<10)
	in := make([]byte, 64<<10)
	for pass := 0; ; pass++ {
		moved, done := 0, true
		for index, f := range flows {
			ends := [2]struct {
				owner int
				id    TCPEndpointID
			}{{f.clientOwner, f.client}, {f.srvOwner, f.server}}
			for side, end := range ends {
				if f.sent[side] < f.total {
					ready := readiness(t, h, end.owner, end.id)
					if ready.Mask&TCPReadyWritable != 0 {
						length := min(f.total-f.sent[side], int(ready.Writable), len(out))
						for at := range length {
							out[at] = byte(f.sent[side]+at) ^ byte(index) ^ byte(side)
						}
						n, result := h.stack.TCPWrite(end.owner, end.id, out[:length], h.now)
						if result != ResultOK || n != length {
							t.Fatalf("pair %d side %d: write %d of %d room, %d", index, side, n, length, result)
						}
						f.sent[side] += n
						moved += n
					}
				}
				peer := 1 - side
				if f.received[peer] < f.total {
					ready := readiness(t, h, end.owner, end.id)
					if ready.Mask&TCPReadyReadable != 0 {
						n, result := h.stack.TCPRead(end.owner, end.id, in, h.now)
						if result != ResultOK {
							t.Fatalf("pair %d side %d: read %d", index, side, result)
						}
						for at := range n {
							if in[at] != byte(f.received[peer]+at)^byte(index)^byte(peer) {
								t.Fatalf("pair %d side %d: byte %d corrupt", index, side, f.received[peer]+at)
							}
						}
						f.received[peer] += n
						moved += n
					}
				}
				if f.sent[side] < f.total || f.received[side] < f.total {
					done = false
				}
			}
		}
		// Advance at the same instant runs held ACKs and checks every
		// index and budget charge in a validation build; no timer is due.
		h.stack.Advance(h.now)
		h.checkDeadline()
		if done {
			break
		}
		if moved == 0 {
			stats := h.stack.TCPStats()
			stalled := 0
			for _, f := range flows {
				if f.received != [2]int{f.total, f.total} {
					stalled++
				}
			}
			t.Fatalf("pass %d: %d of %d pairs stalled with no timer due; stats %+v", pass, stalled, pairs, stats)
		}
	}
	if h.now != start {
		t.Fatal("the clock moved")
	}
	if stats := h.stack.TCPStats(); stats.Retransmits != 0 || stats.DropWindow != 0 || stats.DropMemory != 0 {
		t.Fatalf("retransmits %d window drops %d memory drops %d", stats.Retransmits, stats.DropWindow, stats.DropMemory)
	}
	return flows
}

// 300 loopback pairs stream 32 KiB each way at once under the default
// budgets.
func TestTCPBudgetDefaultHoldsThreeHundredPairs(t *testing.T) {
	h := newHarness(t)
	streamLoopbackPairs(t, h, 300, 32<<10)
	stats := h.stack.TCPStats()
	t.Logf("%d ring bytes, %d capped growths", stats.MemoryBytes, stats.MemoryCapped)
}

// Each owner's budget is twice its connections' floor, the least that
// admits them all: a loopback connection takes the larger receive ring only
// while the owner has used less than half its budget. The rings stay near
// the floor, the windows shrink, every pair still finishes, and nothing
// waits for a timer.
func TestTCPBudgetAtTheFloorStillProgresses(t *testing.T) {
	const pairs = 300
	h := newHarness(t)
	floor := pairs * (tcpBufferFloor + tcpBufferFloor)
	h.stack.SetTCPMemoryBudget(4*floor, 2*floor)
	streamLoopbackPairs(t, h, pairs, 32<<10)
	stats := h.stack.TCPStats()
	if stats.MemoryCapped == 0 {
		t.Fatal("no ring growth was capped")
	}
	for owner := range 2 {
		used, budget := h.stack.TCPOwnerMemory(owner)
		if used > budget {
			t.Fatalf("owner %d holds %d over its budget %d", owner, used, budget)
		}
	}
	t.Logf("%d ring bytes, %d capped growths", stats.MemoryBytes, stats.MemoryCapped)
}

// A handshake whose minimum the budgets cannot pay is refused, never
// admitted into a connection that could not move a byte: connect answers
// Busy, a listener drops the SYN, DropMemory counts both, and
// TCPOwnerMemory names the owner holding the memory. The client's SYN
// timer retries once the owner has room.
func TestTCPBudgetRefusesAdmissionBelowTheMinimum(t *testing.T) {
	h := newHarness(t)
	minimum := tcpBufferFloor + tcpBufferFloor
	h.stack.SetTCPMemoryBudget(0, minimum-1)
	id, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, id, LoopbackAddress, 7200, h.now); result != ResultBusy {
		t.Fatalf("connect below the minimum %d", result)
	}
	if stats := h.stack.TCPStats(); stats.DropMemory != 1 || stats.MemoryBytes != 0 {
		t.Fatalf("drops %d bytes %d", stats.DropMemory, stats.MemoryBytes)
	}
	if used, budget := h.stack.TCPOwnerMemory(1); used != 0 || budget != minimum-1 {
		t.Fatalf("owner memory %d of %d", used, budget)
	}

	// The client's owner fits one connection at the floor; the listener's
	// owner holds the receive ring of a connection a reset ended.
	h.stack.SetTCPMemoryBudget(0, minimum)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, LoopbackAddress, 7200)
	h.stack.TCPListen(0, listener, 4)
	holder, _ := h.stack.TCPOpen(0)
	if _, result := h.stack.TCPConnect(0, holder, LoopbackAddress, 7201, h.now); result != ResultOK {
		t.Fatalf("holder connect %d", result)
	}
	if _, result := h.stack.TCPConnect(1, id, LoopbackAddress, 7200, h.now); result != ResultOK {
		t.Fatalf("connect %d", result)
	}
	if ready := readiness(t, h, 1, id); ready.Mask&TCPReadyConnecting == 0 {
		t.Fatalf("client %+v", ready)
	}
	if _, _, result := h.stack.TCPAccept(0, listener); result != ResultBusy {
		t.Fatalf("accept %d", result)
	}
	if stats := h.stack.TCPStats(); stats.DropMemory != 2 {
		t.Fatalf("drops %d", stats.DropMemory)
	}
	if used, budget := h.stack.TCPOwnerMemory(0); budget-used >= minimum {
		t.Fatalf("listener owner holds %d of %d", used, budget)
	}
	h.stack.TCPClose(0, holder, h.now)
	h.runUntil(h.now + 2*h.stack.tuning.TCP.RTOMin)
	if _, _, result := h.stack.TCPAccept(0, listener); result != ResultOK {
		t.Fatalf("accept after room appeared %d", result)
	}
}
