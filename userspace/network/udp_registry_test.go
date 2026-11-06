package network

import "testing"

// TestUDPFullGrantInOneSession opens, binds and receives on the 512
// endpoints linuxsockd's grant allows one session.
func TestUDPFullGrantInOneSession(t *testing.T) {
	h := readyWithPeer(t)
	if !h.stack.SetUDPQuota(0, MaxUDPSessionQuota) {
		t.Fatal("quota")
	}
	ids := make([]EndpointID, MaxUDPSessionQuota)
	ports := make([]uint16, MaxUDPSessionQuota)
	for index := range ids {
		ids[index] = h.open(0)
		ports[index] = h.bind(0, ids[index], IPv4{}, 0)
	}
	if _, result := h.stack.UDPOpen(0); result != ResultBusy {
		t.Fatalf("open past the grant: %d", result)
	}
	// The owner's buffer share is half the grant, so each datagram is taken
	// before the next arrives.
	for index, port := range ports {
		h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte{byte(index)}))
		if _, payload := h.take(0, ids[index]); len(payload) != 1 || payload[0] != byte(index) {
			t.Fatalf("endpoint %d got %v", index, payload)
		}
	}
	stats := h.stack.UDPStats()
	if stats.Endpoints != MaxUDPSessionQuota || stats.RX != MaxUDPSessionQuota || stats.MaxLookupProbes > 8 {
		t.Fatalf("stats %+v", stats)
	}
	if h.stack.udpOwnerBuffers[0] != 0 {
		t.Fatalf("owner holds %d buffers after every receive", h.stack.udpOwnerBuffers[0])
	}
}

func TestUDPQuotaFillFailReleaseReuse(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.SetUDPQuota(0, MaxUDPSessionQuota)
	ids := make([]EndpointID, MaxUDPSessionQuota)
	for index := range ids {
		ids[index] = h.open(0)
	}
	if _, result := h.stack.UDPOpen(0); result != ResultBusy {
		t.Fatalf("open past the quota: %d", result)
	}
	// Another owner still opens: the quota is the owner's.
	other := h.open(1)
	closed := ids[100]
	port := h.bind(0, closed, IPv4{}, 9100)
	h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("queued")))
	if h.stack.udpOwnerBuffers[0] != 1 {
		t.Fatalf("owner buffers %d", h.stack.udpOwnerBuffers[0])
	}
	if result := h.stack.UDPClose(0, closed); result != ResultOK {
		t.Fatal(result)
	}
	if h.stack.udpOwnerBuffers[0] != 0 || h.stack.udpPorts.count != 0 {
		t.Fatalf("close kept buffers %d or the port index %d", h.stack.udpOwnerBuffers[0], h.stack.udpPorts.count)
	}
	again := h.open(0)
	if again.slot() != closed.slot() || again == closed {
		t.Fatalf("reopened %#x after %#x", again, closed)
	}
	if result := h.stack.UDPClose(0, closed); result != ResultInvalidArgument {
		t.Fatalf("stale ID closed: %d", result)
	}
	if h.bind(0, again, IPv4{}, 9100) != 9100 {
		t.Fatal("closed endpoint kept its port")
	}
	h.stack.UDPCloseOwner(0)
	if stats := h.stack.UDPStats(); stats.Endpoints != 1 || h.stack.udpOwnerEndpoints[0] != 0 || h.stack.udpPorts.count != 0 {
		t.Fatalf("after session end: endpoints %d owner %d indexed %d", stats.Endpoints, h.stack.udpOwnerEndpoints[0], h.stack.udpPorts.count)
	}
	for range MaxUDPSessionQuota {
		h.open(0)
	}
	if h.stack.UDPClose(1, other) != ResultOK {
		t.Fatal("other owner's endpoint")
	}
	// Slots are reused: the registry never grew past the endpoints held.
	if len(h.stack.udp) > MaxUDPSessionQuota+1 {
		t.Fatalf("registry %d slots", len(h.stack.udp))
	}
}

// TestUDPRegistryCeilingAndProbes fills the whole registry across every
// session and checks that delivery reads one short bucket per datagram.
func TestUDPRegistryCeilingAndProbes(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.UDP.MaxEndpoints = MaxUDPEndpoints
	h.stack.tuning.UDP.EphemeralMin = 1024
	type held struct {
		owner int
		id    EndpointID
		port  uint16
	}
	var all []held
	for owner := range MaxUDPSessions {
		h.stack.SetUDPQuota(owner, MaxUDPSessionQuota)
		for range MaxUDPSessionQuota {
			id := h.open(owner)
			all = append(all, held{owner, id, h.bind(owner, id, IPv4{}, 0)})
		}
	}
	if len(all) != MaxUDPEndpoints {
		t.Fatalf("opened %d", len(all))
	}
	if slot := h.stack.allocateUDP(); slot != -1 || len(h.stack.udp) != MaxUDPEndpoints {
		t.Fatalf("registry past its ceiling: slot %d of %d", slot, len(h.stack.udp))
	}
	for _, entry := range all {
		h.receive(udpFrame(peerIP, peerMAC, 7, entry.port, []byte("p")))
		if _, payload := h.take(entry.owner, entry.id); string(payload) != "p" {
			t.Fatalf("port %d payload %q", entry.port, payload)
		}
	}
	stats := h.stack.UDPStats()
	if stats.MaxLookupProbes > 16 || stats.LookupProbes > 3*uint64(len(all)) {
		t.Fatalf("probes total %d max %d over %d datagrams", stats.LookupProbes, stats.MaxLookupProbes, len(all))
	}
}
