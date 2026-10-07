package network

import "testing"

func TestInitialState(t *testing.T) {
	h := newHarness(t)
	snapshot := h.stack.Snapshot(h.now)
	if snapshot != (Snapshot{State: StateDown, Mode: ModeDHCP}) {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("idle stack has a deadline")
	}
	if _, ok := h.stack.NextFrame(make([]byte, MaxFrame)); ok {
		t.Fatal("idle stack has a frame")
	}
}

func TestReceiveFrameFiltering(t *testing.T) {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	base := h.stack.Snapshot(h.now)
	request := icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x"))
	good := ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, request)
	vlan := append([]byte(nil), good...)
	vlan[12], vlan[13] = 0x81, 0x00
	ipv6 := append([]byte(nil), good...)
	ipv6[12], ipv6[13] = 0x86, 0xdd
	oversize := make([]byte, MaxFrame+1)
	copy(oversize, good)
	for _, test := range []struct {
		name      string
		frame     []byte
		drop      uint64
		malformed uint64
		replies   int
	}{
		{"good", good, 0, 0, 1},
		{"short", good[:HeaderSize-1], 0, 1, 0},
		{"empty", nil, 0, 1, 0},
		{"oversize", oversize, 0, 1, 0},
		{"other host MAC", ipv4Frame(otherMAC, peerMAC, peerIP, hostIP, protocolICMP, request), 1, 0, 0},
		{"multicast MAC", ipv4Frame(MAC{0x01, 0, 0x5e, 0, 0, 1}, peerMAC, peerIP, hostIP, protocolICMP, request), 1, 0, 0},
		{"multicast source MAC", ipv4Frame(hostMAC, MAC{0x01, 0, 0, 0, 0, 1}, peerIP, hostIP, protocolICMP, request), 1, 0, 0},
		{"own source MAC", ipv4Frame(hostMAC, hostMAC, peerIP, hostIP, protocolICMP, request), 1, 0, 0},
		{"vlan", vlan, 1, 0, 0},
		{"ipv6", ipv6, 1, 0, 0},
		{"header only", good[:HeaderSize], 0, 1, 0},
		{"truncated ipv4", good[:HeaderSize+ipv4HeaderSize+2], 0, 1, 0},
		{"unknown protocol", ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, 99, request), 1, 0, 0},
		{"our source address", ipv4Frame(hostMAC, peerMAC, hostIP, hostIP, protocolICMP, request), 1, 0, 0},
		{"truncated icmp", ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, request[:4]), 0, 1, 0},
	} {
		before := h.stack.Snapshot(h.now)
		h.receive(test.frame)
		after := h.stack.Snapshot(h.now)
		replies := len(h.drain())
		if after.RX != before.RX+1 || after.Drop-before.Drop != test.drop ||
			after.Malformed-before.Malformed != test.malformed || replies != test.replies {
			t.Errorf("%s: drop +%d malformed +%d replies %d", test.name, after.Drop-before.Drop,
				after.Malformed-before.Malformed, replies)
		}
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.RX != base.RX+15 {
		t.Fatalf("rx %d", snapshot.RX)
	}
}

func TestReceiveWhileDownDrops(t *testing.T) {
	h := newHarness(t)
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP, targetIP: hostIP}))
	checkCounters(t, h.stack, 1, 1, 0)
	if len(h.drain()) != 0 {
		t.Fatal("down stack sent a frame")
	}
}

func TestCountersSaturate(t *testing.T) {
	h := newHarness(t)
	h.stack.drop = maxCounter - 1
	h.stack.CountDrop()
	h.stack.CountDrop()
	if h.stack.Snapshot(h.now).Drop != maxCounter {
		t.Fatal("drop counter wrapped")
	}
	h.stack.rx = maxCounter
	h.receive(nil)
	if snapshot := h.stack.Snapshot(h.now); snapshot.RX != maxCounter || snapshot.Malformed != 1 {
		t.Fatalf("snapshot %+v", snapshot)
	}
}

func TestLinkDownFailsEchoes(t *testing.T) {
	for _, test := range []struct {
		reason Result
		want   Result
	}{
		{ResultDown, ResultDown},
		{ResultDriverFault, ResultDriverFault},
		{ResultTimeout, ResultDown},
	} {
		h := readyWithPeer(t)
		h.stack.Echo(0, peerIP, 1, second, h.now)
		h.stack.Echo(1, IPv4{10, 0, 2, 50}, 1, second, h.now)
		h.stack.LinkDown(test.reason, h.now)
		if h.stack.PendingFrames() != 0 {
			t.Fatal("frames kept for a dead link")
		}
		results := map[int]Result{}
		for {
			completion, ok := h.stack.Completion()
			if !ok {
				break
			}
			results[completion.Session] = completion.Result
		}
		if len(results) != 2 || results[0] != test.want || results[1] != test.want {
			t.Fatalf("reason %d: results %v", test.reason, results)
		}
		snapshot := h.stack.Snapshot(h.now)
		if snapshot.State != StateDown || snapshot.Link || snapshot.Address != zeroIPv4 {
			t.Fatalf("snapshot %+v", snapshot)
		}
		if result := h.stack.Echo(0, peerIP, 1, second, h.now); result != ResultDown {
			t.Fatalf("echo while down %d", result)
		}
		if _, ok := h.stack.Deadline(); ok {
			t.Fatal("deadline while down")
		}
		// The driver comes back: configuration starts over.
		h.stack.LinkUp(hostMAC, MTU, h.now)
		h.runUntil(h.now + claimTime)
		if state := h.stack.Snapshot(h.now).State; state != StateReady {
			t.Fatalf("state after relink %d", state)
		}
	}
}

func TestLinkUpVariants(t *testing.T) {
	h := readyWithPeer(t)
	// A repeated Ready from the same device changes nothing.
	h.stack.LinkUp(hostMAC, MTU, h.now)
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady {
		t.Fatalf("snapshot %+v", snapshot)
	}
	h.stack.Echo(0, peerIP, 1, second, h.now)
	// A new MAC is a new device.
	h.stack.LinkUp(otherMAC, MTU, h.now)
	if completion := h.completion(); completion.Result != ResultDriverFault {
		t.Fatalf("completion %+v", completion)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateConfiguring || snapshot.MAC != otherMAC {
		t.Fatalf("snapshot %+v", snapshot)
	}
	for name, mac := range map[string]MAC{"zero": {}, "broadcast": broadcastMAC, "multicast": {0x01, 0, 0x5e, 0, 0, 1}} {
		h := newHarness(t)
		h.stack.LinkUp(mac, MTU, h.now)
		if state := h.stack.Snapshot(h.now).State; state != StateFailed {
			t.Errorf("%s MAC: state %d", name, state)
		}
	}
	h = newHarness(t)
	h.stack.LinkUp(hostMAC, minMTU-1, h.now)
	if state := h.stack.Snapshot(h.now).State; state != StateFailed {
		t.Errorf("small MTU: state %d", state)
	}
	// Failed recovers on the next LinkUp.
	h.stack.LinkUp(hostMAC, 9000, h.now)
	if state := h.stack.Snapshot(h.now).State; state != StateConfiguring || h.stack.mtu != MTU {
		t.Errorf("after recovery: state %d mtu %d", state, h.stack.mtu)
	}
}

func TestFail(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.Echo(0, peerIP, 1, second, h.now)
	h.stack.Fail(h.now)
	if completion := h.completion(); completion.Result != ResultDriverFault {
		t.Fatalf("completion %+v", completion)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateFailed || snapshot.Address != zeroIPv4 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP, targetIP: hostIP}))
	if len(h.drain()) != 0 {
		t.Fatal("failed stack answered")
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("failed stack has a deadline")
	}
	h.stack.Configure(staticConfig, h.now)
	h.runUntil(h.now + claimTime)
	if state := h.stack.Snapshot(h.now).State; state != StateReady {
		t.Fatalf("after reload %d", state)
	}
}

func TestConfigure(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.Echo(0, peerIP, 1, second, h.now)
	h.drain()

	invalid := staticConfig
	invalid.Gateway = IPv4{10, 9, 9, 9}
	if field := h.stack.Configure(invalid, h.now); field != FieldGateway {
		t.Fatalf("field %d", field)
	}
	if field := h.stack.Configure(Config{Mode: 7}, h.now); field != FieldMode {
		t.Fatalf("field %d", field)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Gateway != gatewayIP {
		t.Fatalf("invalid config changed state: %+v", snapshot)
	}
	h.noCompletion()

	// The same configuration is a no-op.
	if field := h.stack.Configure(staticConfig, h.now); field != FieldNone {
		t.Fatalf("field %d", field)
	}
	h.noCompletion()
	if len(h.drain()) != 0 {
		t.Fatal("no-op configure sent frames")
	}

	// A gateway change on the same address applies in place.
	moved := staticConfig
	moved.Gateway = IPv4{10, 0, 2, 3}
	h.stack.Configure(moved, h.now)
	h.noCompletion()
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Gateway != moved.Gateway {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if _, ok := h.stack.lookupNeighbor(peerIP, h.now); !ok {
		t.Fatal("gateway change cleared neighbors")
	}

	// A new address fails the echo and probes again.
	renumbered := moved
	renumbered.Address = IPv4{10, 0, 2, 16}
	h.stack.Configure(renumbered, h.now)
	if completion := h.completion(); completion.Result != ResultReconfigured {
		t.Fatalf("completion %+v", completion)
	}
	h.start = h.now
	checkClaim(t, h.claim(), renumbered.Address, h.start, defaults)
	h.finishAnnouncements()
	if snapshot := h.stack.Snapshot(h.now); snapshot.Address != renumbered.Address {
		t.Fatalf("snapshot %+v", snapshot)
	}

	// Leaving static starts DHCP.
	h.stack.Echo(0, peerIP, 2, second, h.now)
	h.stack.Configure(Config{Mode: ModeDHCP, Address: IPv4{1, 2, 3, 4}}, h.now)
	if completion := h.completion(); completion.Result != ResultReconfigured {
		t.Fatalf("completion %+v", completion)
	}
	messages := dhcpMessages(t, h.drain())
	if len(messages) != 1 || messages[0].kind != dhcpDiscover {
		t.Fatalf("messages %+v", messages)
	}
	if snapshot := h.stack.Snapshot(h.now); snapshot.Mode != ModeDHCP || snapshot.State != StateConfiguring {
		t.Fatalf("snapshot %+v", snapshot)
	}
	// DHCP again is a no-op whatever the ignored fields hold.
	h.stack.Configure(Config{Mode: ModeDHCP, Prefix: 9}, h.now)
	if len(h.drain()) != 0 {
		t.Fatal("repeated DHCP configure restarted discovery")
	}
}

func TestConfigureWhileDown(t *testing.T) {
	h := newHarness(t)
	h.stack.Configure(staticConfig, h.now)
	if len(h.drain()) != 0 || h.stack.Snapshot(h.now).State != StateDown {
		t.Fatal("configure while down started work")
	}
	h.stack.LinkUp(hostMAC, MTU, h.now)
	h.runUntil(h.now + claimTime)
	if snapshot := h.stack.Snapshot(h.now); snapshot.State != StateReady || snapshot.Address != hostIP {
		t.Fatalf("snapshot %+v", snapshot)
	}
}

// TestEveryEchoCompletesOnce drives sessions through every outcome with a
// scripted peer and checks each accepted echo yields exactly one completion.
func TestEveryEchoCompletesOnce(t *testing.T) {
	h := readyWithPeer(t)
	accepted := map[int]int{}
	completed := map[int]int{}
	results := map[Result]int{}
	collect := func() {
		for {
			completion, ok := h.stack.Completion()
			if !ok {
				return
			}
			completed[completion.Session]++
			results[completion.Result]++
			if completed[completion.Session] > accepted[completion.Session] {
				t.Fatalf("session %d completed more than accepted", completion.Session)
			}
		}
	}
	destinations := []IPv4{peerIP, remoteIP, IPv4{10, 0, 2, 60}, hostIP}
	for round := 0; round < 200; round++ {
		session := round % MaxSessions
		if h.stack.Echo(session, destinations[round%len(destinations)], uint16(round), MinEchoTimeout+uint64(round%5)*millisecond*300,
			h.now) == ResultOK {
			accepted[session]++
		}
		for _, frame := range h.drain() {
			sent := decode(t, frame)
			if sent.ether.etherType == etherTypeARP && sent.arp.senderIP == hostIP && sent.arp.targetIP != hostIP &&
				round%4 != 0 {
				h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC,
					senderIP: sent.arp.targetIP, targetMAC: hostMAC, targetIP: hostIP}))
				continue
			}
			if sent.ether.etherType == etherTypeIPv4 && sent.icmp.kind == icmpEchoRequest && round%3 == 0 {
				h.receive(ipv4Frame(hostMAC, peerMAC, sent.ip.destination, hostIP, protocolICMP,
					icmpBytes(icmpEchoReply, 0, sent.icmp.identifier, sent.icmp.sequence, sent.icmp.body)))
			}
		}
		switch round % 50 {
		case 17:
			h.stack.LinkDown(ResultDown, h.now)
			h.stack.LinkUp(hostMAC, MTU, h.now)
			h.runUntil(h.now + claimTime)
		case 33:
			h.stack.CancelSession(session)
			accepted[session] = completed[session]
		}
		h.advance(uint64(round%7) * 150 * millisecond)
		collect()
	}
	h.runUntil(h.now + 10*second)
	collect()
	if results[ResultOK] == 0 || results[ResultTimeout] == 0 || results[ResultDown] == 0 {
		t.Fatalf("outcomes not exercised: %v", results)
	}
	t.Logf("outcomes %v", results)
	for session := 0; session < MaxSessions; session++ {
		if accepted[session] != completed[session] {
			t.Fatalf("session %d: accepted %d completed %d", session, accepted[session], completed[session])
		}
	}
}
