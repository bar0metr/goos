package network

import "testing"

func TestStaticProbeThenReady(t *testing.T) {
	h := newHarness(t)
	h.stack.Configure(staticConfig, h.now)
	if state := h.stack.Snapshot(h.now).State; state != StateDown {
		t.Fatalf("state before link %d", state)
	}
	h.stack.LinkUp(hostMAC, MTU, h.now)
	if state := h.stack.Snapshot(h.now).State; state != StateConfiguring {
		t.Fatalf("state after link %d", state)
	}
	first := only(t, h.drain())
	if first.arp.operation != arpRequest || first.arp.senderIP != zeroIPv4 || first.arp.targetIP != hostIP ||
		first.ether.destination != broadcastMAC {
		t.Fatalf("first probe %+v", first.arp)
	}
	h.advance(defaults.ProbeInterval)
	second := only(t, h.drain())
	if second.arp.senderIP != zeroIPv4 || second.arp.targetIP != hostIP {
		t.Fatalf("second probe %+v", second.arp)
	}
	if state := h.stack.Snapshot(h.now).State; state != StateConfiguring {
		t.Fatalf("ready before the probe ended: %d", state)
	}
	h.advance(defaults.ProbeInterval)
	announce := only(t, h.drain())
	if announce.arp.senderIP != hostIP || announce.arp.targetIP != hostIP {
		t.Fatalf("announcement %+v", announce.arp)
	}
	snapshot := h.stack.Snapshot(h.now)
	if snapshot.State != StateReady || snapshot.Address != hostIP || snapshot.Prefix != 24 ||
		snapshot.Gateway != gatewayIP || snapshot.LeaseRemaining != 0 || snapshot.Mode != ModeStatic {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("idle ready stack has a deadline")
	}
}

func TestStaticProbeConflicts(t *testing.T) {
	for _, test := range []struct {
		name   string
		packet arpPacket
	}{
		{"reply from owner", arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP, targetMAC: hostMAC}},
		{"announcement", arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: hostIP, targetIP: hostIP}},
		{"simultaneous probe", arpPacket{operation: arpRequest, senderMAC: peerMAC, targetIP: hostIP}},
	} {
		h := newHarness(t)
		h.stack.Configure(staticConfig, h.now)
		h.stack.LinkUp(hostMAC, MTU, h.now)
		h.drain()
		h.advance(100 * millisecond)
		h.receive(arpFrame(broadcastMAC, test.packet))
		if state := h.stack.Snapshot(h.now).State; state != StateFailed {
			t.Fatalf("%s: state %d, want failed", test.name, state)
		}
		h.runUntil(h.now + 5*second)
		if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateFailed || snapshot.Address != zeroIPv4 {
			t.Fatalf("%s: snapshot %+v", test.name, snapshot)
		}
		if result := h.stack.Echo(0, gatewayIP, 1, second, h.now); result != ResultNoAddress {
			t.Fatalf("%s: echo %d", test.name, result)
		}
		// A reload retries the probe even with the same configuration.
		h.stack.Configure(staticConfig, h.now)
		h.runUntil(h.now + 2*second)
		if state := h.stack.Snapshot(h.now).State; state != StateReady {
			t.Fatalf("%s: state after reload %d", test.name, state)
		}
	}
}

func TestProbeIgnoresUnrelatedARP(t *testing.T) {
	h := newHarness(t)
	h.stack.Configure(staticConfig, h.now)
	h.stack.LinkUp(hostMAC, MTU, h.now)
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP,
		targetIP: gatewayIP}))
	// Our own probe looped back is not a conflict either.
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: hostMAC, targetIP: hostIP}))
	h.runUntil(h.now + 2*second)
	if state := h.stack.Snapshot(h.now).State; state != StateReady {
		t.Fatalf("state %d", state)
	}
}

func TestARPAnswersRequestsForOurAddress(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP,
		targetIP: hostIP}))
	reply := only(t, h.drain())
	if reply.ether.destination != peerMAC || reply.arp.operation != arpReply || reply.arp.senderMAC != hostMAC ||
		reply.arp.senderIP != hostIP || reply.arp.targetMAC != peerMAC || reply.arp.targetIP != peerIP {
		t.Fatalf("reply %+v", reply.arp)
	}
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); !ok {
		t.Fatal("requester not learned")
	}
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP,
		targetIP: IPv4{10, 0, 2, 99}}))
	if frames := h.drain(); len(frames) != 0 {
		t.Fatalf("answered a request for another address: %d frames", len(frames))
	}
}

func TestARPLearningRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		frame  []byte
		learn  IPv4
		wanted bool
	}{
		{"reply to us", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: peerIP,
			targetMAC: hostMAC, targetIP: hostIP}), peerIP, true},
		{"request for someone else", arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC,
			senderIP: peerIP, targetIP: gatewayIP}), peerIP, false},
		{"multicast sender MAC", arpFrame(broadcastMAC, arpPacket{operation: arpReply, senderMAC: MAC{0x01, 0, 0x5e, 0, 0, 1},
			senderIP: peerIP, targetIP: hostIP}), peerIP, false},
		{"zero sender MAC", ethernet(broadcastMAC, peerMAC, etherTypeARP, arpPayload(arpPacket{operation: arpReply,
			senderIP: peerIP, targetIP: hostIP})), peerIP, false},
		{"broadcast sender IP", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
			senderIP: broadcastIPv4, targetIP: hostIP}), broadcastIPv4, false},
		{"directed broadcast sender IP", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
			senderIP: IPv4{10, 0, 2, 255}, targetIP: hostIP}), IPv4{10, 0, 2, 255}, false},
		{"off-link sender IP", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
			senderIP: remoteIP, targetIP: hostIP}), remoteIP, false},
		{"loopback sender IP", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
			senderIP: IPv4{127, 0, 0, 1}, targetIP: hostIP}), IPv4{127, 0, 0, 1}, false},
		{"claims our address", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
			senderIP: hostIP, targetIP: hostIP}), hostIP, false},
		{"truncated", arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: peerIP,
			targetIP: hostIP})[:HeaderSize+arpSize-1], peerIP, false},
	} {
		h := newHarness(t)
		h.readyStatic(staticConfig)
		h.receive(test.frame)
		if _, ok := h.stack.lookupNeighbor(test.learn, h.now); ok != test.wanted {
			t.Errorf("%s: learned %v, want %v", test.name, ok, test.wanted)
		}
	}
}

func arpPayload(packet arpPacket) []byte {
	payload := make([]byte, arpSize)
	putARP(payload, packet)
	return payload
}

func TestARPTableExpiryAndBound(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	h.learn(peerIP, peerMAC)
	h.advance(defaults.NeighborLifetime - 1)
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); !ok {
		t.Fatal("entry expired early")
	}
	h.advance(1)
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); ok {
		t.Fatal("entry outlived its lifetime")
	}

	for index := 0; index < MaxNeighbors+4; index++ {
		h.learn(IPv4{10, 0, 2, byte(30 + index)}, MAC{0x52, 0, 0, 0, 0, byte(index)})
		h.advance(millisecond)
	}
	used := 0
	for index := range h.stack.neighbors[:] {
		if h.stack.neighbors[index].used {
			used++
		}
	}
	if used != MaxNeighbors {
		t.Fatalf("%d entries", used)
	}
	if _, ok := h.stack.lookupNeighbor(IPv4{10, 0, 2, 30}, h.now); ok {
		t.Fatal("oldest entry was not the one replaced")
	}
	if _, ok := h.stack.lookupNeighbor(IPv4{10, 0, 2, byte(30 + MaxNeighbors + 3)}, h.now); !ok {
		t.Fatal("newest entry missing")
	}
	// Refreshing an existing address reuses its entry.
	h.learn(IPv4{10, 0, 2, 40}, otherMAC)
	if mac, _ := h.stack.lookupNeighbor(IPv4{10, 0, 2, 40}, h.now); mac != otherMAC {
		t.Fatal("refresh did not update the MAC")
	}
}

func TestResolutionRetriesThenNeighborUnreachable(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	if result := h.stack.Echo(0, peerIP, 1, MaxEchoTimeout, h.now); result != ResultOK {
		t.Fatalf("echo %d", result)
	}
	for attempt := 1; attempt <= int(defaults.ResolveAttempts); attempt++ {
		request := only(t, h.drain())
		if request.arp.operation != arpRequest || request.arp.targetIP != peerIP || request.arp.senderIP != hostIP ||
			request.ether.destination != broadcastMAC {
			t.Fatalf("attempt %d: %+v", attempt, request.arp)
		}
		h.noCompletion()
		h.advance(defaults.ResolveInterval - 1)
		if frames := h.drain(); len(frames) != 0 {
			t.Fatalf("attempt %d: request sent early", attempt)
		}
		h.advance(1)
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatalf("a fourth request was sent")
	}
	completion := h.completion()
	if completion.Result != ResultNeighborUnreachable || completion.Session != 0 || completion.Sequence != 1 {
		t.Fatalf("completion %+v", completion)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("deadline left after failure")
	}
}

func TestResolutionSharedByEchoesAndBounded(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	// Two sessions to one next hop share one resolution.
	h.stack.Echo(0, remoteIP, 1, MaxEchoTimeout, h.now)
	h.stack.Echo(1, IPv4{198, 51, 100, 1}, 1, MaxEchoTimeout, h.now)
	if frames := h.drain(); len(frames) != 1 {
		t.Fatalf("%d ARP requests for one gateway", len(frames))
	}
	for session := 2; session < 2+MaxResolutions-1; session++ {
		if result := h.stack.Echo(session, IPv4{10, 0, 2, byte(100 + session)}, 1, MaxEchoTimeout, h.now); result != ResultOK {
			t.Fatalf("session %d: %d", session, result)
		}
	}
	if result := h.stack.Echo(7, IPv4{10, 0, 2, 200}, 1, MaxEchoTimeout, h.now); result != ResultBusy {
		t.Fatalf("fifth resolution: %d, want busy", result)
	}
	h.stack.Echo(7, gatewayIP, 1, MaxEchoTimeout, h.now)
	h.drain()
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: gatewayMAC, senderIP: gatewayIP,
		targetMAC: hostMAC, targetIP: hostIP}))
	frames := h.drain()
	if len(frames) != 3 {
		t.Fatalf("%d echoes sent after resolution, want 3", len(frames))
	}
	for _, frame := range frames {
		sent := decode(t, frame)
		if sent.ether.destination != gatewayMAC || sent.icmp.kind != icmpEchoRequest {
			t.Fatalf("echo frame %+v", sent)
		}
	}
}

func TestClearedNeighborsOnReconfigure(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	h.learn(gatewayIP, gatewayMAC)
	other := staticConfig
	other.Address = IPv4{10, 0, 2, 16}
	h.stack.Configure(other, h.now)
	if _, ok := h.stack.lookupNeighbor(gatewayIP, h.now); ok {
		t.Fatal("neighbor kept across an address change")
	}
}
