package network

import "testing"

// loopbackPair returns a listener on owner 0, a connected client on owner 1
// and the accepted server child.
func loopbackPair(t *testing.T, h *harness, port uint16) (listener, client, server TCPEndpointID) {
	t.Helper()
	listener, _ = h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, port); result != ResultOK {
		t.Fatalf("bind %d", result)
	}
	if result := h.stack.TCPListen(0, listener, 2); result != ResultOK {
		t.Fatalf("listen %d", result)
	}
	client, _ = h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, port, h.now); result != ResultOK {
		t.Fatalf("connect %d", result)
	}
	var result Result
	if server, _, result = h.stack.TCPAccept(0, listener); result != ResultOK {
		t.Fatalf("accept %d", result)
	}
	return listener, client, server
}

func readiness(t *testing.T, h *harness, owner int, id TCPEndpointID) TCPReadiness {
	t.Helper()
	ready, result := h.stack.TCPReadiness(owner, id)
	if result != ResultOK {
		t.Fatalf("readiness %d", result)
	}
	return ready
}

func TestTCPReadinessFollowsTheStream(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, LoopbackAddress, 8080)
	h.stack.TCPListen(0, listener, 2)
	if got := readiness(t, h, 0, listener).Mask; got != TCPReadyListening {
		t.Fatalf("idle listener %#x", got)
	}
	client, _ := h.stack.TCPOpen(1)
	h.stack.TCPConnect(1, client, LoopbackAddress, 8080, h.now)
	if got := readiness(t, h, 0, listener).Mask; got != TCPReadyListening|TCPReadyAcceptable {
		t.Fatalf("listener with a child %#x", got)
	}
	server, _, _ := h.stack.TCPAccept(0, listener)
	if got := readiness(t, h, 0, listener).Mask; got != TCPReadyListening {
		t.Fatalf("listener after accept %#x", got)
	}
	ready := readiness(t, h, 1, client)
	if ready.Mask != TCPReadyConnected|TCPReadyWritable || ready.Writable != DefaultTCPSendBuffer {
		t.Fatalf("connected client %+v", ready)
	}

	h.stack.TCPWrite(1, client, []byte("hello"), h.now)
	ready = readiness(t, h, 0, server)
	if ready.Mask != TCPReadyConnected|TCPReadyWritable|TCPReadyReadable || ready.Readable != 5 {
		t.Fatalf("server with data %+v", ready)
	}
	var peek [3]byte
	if n, result := h.stack.TCPPeek(0, server, peek[:]); result != ResultOK || string(peek[:n]) != "hel" {
		t.Fatalf("peek %d %d %q", n, result, peek[:n])
	}
	if ready := readiness(t, h, 0, server); ready.Readable != 5 {
		t.Fatalf("peek consumed bytes %+v", ready)
	}

	h.stack.TCPShutdownWrite(1, client, h.now)
	ready = readiness(t, h, 0, server)
	if ready.Mask != TCPReadyConnected|TCPReadyWritable|TCPReadyReadable|TCPReadyReadEOF || ready.Readable != 5 {
		t.Fatalf("FIN behind buffered bytes %+v", ready)
	}
	if got := readiness(t, h, 1, client).Mask; got != TCPReadyConnected|TCPReadyWriteClosed {
		t.Fatalf("client after shutdown %#x", got)
	}
	var all [16]byte
	h.stack.TCPRead(0, server, all[:], h.now)
	if n, result := h.stack.TCPPeek(0, server, all[:]); n != 0 || result != ResultEOF {
		t.Fatalf("peek at EOF %d %d", n, result)
	}
	h.stack.TCPShutdownWrite(0, server, h.now)
	if got := readiness(t, h, 0, server).Mask; got != TCPReadyConnected|TCPReadyReadEOF|TCPReadyWriteClosed|TCPReadyHangUp {
		t.Fatalf("both directions closed %#x", got)
	}
	if got := readiness(t, h, 1, client).Mask; got&TCPReadyHangUp == 0 || got&TCPReadyError != 0 {
		t.Fatalf("client after the peer's FIN %#x", got)
	}
}

func TestTCPReadinessOfAFailedConnectAndTakeError(t *testing.T) {
	h := newHarness(t)
	client, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 9, h.now); result != ResultOK {
		t.Fatal(result)
	}
	want := TCPReadyConnectFailed | TCPReadyWriteClosed | TCPReadyHangUp | TCPReadyError
	if got := readiness(t, h, 1, client).Mask; got != want {
		t.Fatalf("refused %#x", got)
	}
	if pending, result := h.stack.TCPTakeError(1, client); result != ResultOK || pending != ResultConnectionRefused {
		t.Fatalf("error %d %d", pending, result)
	}
	if pending, _ := h.stack.TCPTakeError(1, client); pending != ResultOK {
		t.Fatalf("error not cleared %d", pending)
	}
	if got := readiness(t, h, 1, client).Mask; got != want&^TCPReadyError {
		t.Fatalf("after the error was read %#x", got)
	}
	if _, result := h.stack.TCPReadiness(0, client); result != ResultInvalidArgument {
		t.Fatalf("another owner's endpoint %d", result)
	}
}

func TestTCPReadinessReportsConnectingAndReset(t *testing.T) {
	h := readyWithPeer(t)
	h.learn(peerIP, peerMAC)
	id, _ := h.stack.TCPOpen(0)
	tuple, result := h.stack.TCPConnect(0, id, peerIP, 80, h.now)
	if result != ResultOK {
		t.Fatal(result)
	}
	if got := readiness(t, h, 0, id).Mask; got != TCPReadyConnecting {
		t.Fatalf("SYN sent %#x", got)
	}
	syn := h.tcpSent()
	h.tcpFrom(tuple, tcpSegment{sequence: 5000, acknowledgment: syn[0].sequence + 1, flags: tcpFlagSYN | tcpFlagACK, window: 65535, mss: 1000})
	h.tcpSent()
	if got := readiness(t, h, 0, id).Mask; got != TCPReadyConnected|TCPReadyWritable {
		t.Fatalf("established %#x", got)
	}
	options, result := h.stack.TCPOptions(0, id)
	if result != ResultOK || options.SendMSS != 1000 || options.TTL != 64 || options.SendBuffer != DefaultTCPSendBuffer ||
		options.ReceiveBuffer != DefaultTCPReceiveBuffer {
		t.Fatalf("options %+v %d", options, result)
	}
	entry, _ := h.stack.tcpEndpoint(0, id)
	h.tcpFrom(tuple, tcpSegment{sequence: entry.rcvNxt, flags: tcpFlagRST})
	want := TCPReadyConnected | TCPReadyWriteClosed | TCPReadyHangUp | TCPReadyError
	if got := readiness(t, h, 0, id).Mask; got != want {
		t.Fatalf("reset %#x", got)
	}
	if pending, _ := h.stack.TCPTakeError(0, id); pending != ResultReset {
		t.Fatalf("pending %d", pending)
	}
}

func TestTCPOptionsBeforeConnect(t *testing.T) {
	h := newHarness(t)
	id, _ := h.stack.TCPOpen(0)
	if options, _ := h.stack.TCPOptions(0, id); options.SendMSS != tcpDefaultMSS {
		t.Fatalf("options %+v", options)
	}
}

func TestTCPReuseAddressFollowsLinux(t *testing.T) {
	h := newHarness(t)
	listener, client, server := loopbackPair(t, h, 8080)
	other, _ := h.stack.TCPOpen(2)
	// The listener and its child hold the port without reuse.
	if _, _, result := h.stack.TCPBindReuse(2, other, IPv4{}, 8080, true); result != ResultAddressInUse {
		t.Fatalf("bind over a listener %d", result)
	}
	h.stack.TCPClose(0, listener, h.now)
	if _, _, result := h.stack.TCPBindReuse(2, other, IPv4{}, 8080, true); result != ResultAddressInUse {
		t.Fatalf("bind over a child without reuse %d", result)
	}
	h.stack.TCPClose(0, server, h.now)
	h.stack.TCPClose(1, client, h.now)
	// The server closed first: its side waits in TIME_WAIT on port 8080.
	if h.timeWaits(0)+h.tombstones(0) != 1 {
		t.Fatalf("TIME_WAIT %d", h.timeWaits(0))
	}
	if _, _, result := h.stack.TCPBindReuse(2, other, IPv4{}, 8080, true); result != ResultAddressInUse {
		t.Fatalf("bind over TIME_WAIT without reuse %d", result)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if _, _, result := h.stack.TCPBind(2, other, IPv4{}, 8080); result != ResultOK {
		t.Fatalf("bind after TIME_WAIT %d", result)
	}
}

func TestTCPReuseAddressRebindsOverTimeWait(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBindReuse(0, listener, LoopbackAddress, 8080, true); result != ResultOK {
		t.Fatal(result)
	}
	h.stack.TCPListen(0, listener, 2)
	second, _ := h.stack.TCPOpen(2)
	if _, _, result := h.stack.TCPBindReuse(2, second, IPv4{}, 8080, true); result != ResultAddressInUse {
		t.Fatalf("reuse over a listener %d", result)
	}
	if _, _, result := h.stack.TCPBind(2, second, IPv4{127, 0, 0, 2}, 8080); result != ResultOK {
		t.Fatalf("a different address %d", result)
	}
	client, _ := h.stack.TCPOpen(1)
	h.stack.TCPConnect(1, client, LoopbackAddress, 8080, h.now)
	server, _, _ := h.stack.TCPAccept(0, listener)
	h.stack.TCPClose(0, server, h.now)
	h.stack.TCPClose(1, client, h.now)
	h.stack.TCPClose(0, listener, h.now)
	if h.timeWaits(0)+h.tombstones(0) != 1 {
		t.Fatalf("TIME_WAIT %d", h.timeWaits(0))
	}
	// The child took the listener's reuse, so a restarted server binds.
	restarted, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, restarted, LoopbackAddress, 8080); result != ResultAddressInUse {
		t.Fatalf("rebind without reuse %d", result)
	}
	if _, _, result := h.stack.TCPBindReuse(0, restarted, LoopbackAddress, 8080, true); result != ResultOK {
		t.Fatalf("rebind with reuse %d", result)
	}
	if result := h.stack.TCPListen(0, restarted, 2); result != ResultOK {
		t.Fatalf("listen %d", result)
	}
}

func TestTCPExplicitQuota(t *testing.T) {
	h := newHarness(t)
	if h.stack.SetTCPQuota(0, MaxTCPSessionQuota+1) || h.stack.SetTCPQuota(MaxTCPSessions, 8) || h.stack.SetTCPQuota(0, -1) {
		t.Fatal("accepted an invalid quota")
	}
	h.stack.SetTCPQuota(0, MaxTCPSessionQuota)
	for count := range MaxTCPSessionQuota {
		if _, result := h.stack.TCPOpen(0); result != ResultOK {
			t.Fatalf("open %d: %d", count, result)
		}
	}
	if _, result := h.stack.TCPOpen(0); result != ResultBusy {
		t.Fatalf("past the quota %d", result)
	}
	// The ordinary quota of other owners is untouched, and the tuning does
	// not lower an explicit grant.
	h.stack.tuning.TCP.SessionEndpoints = 2
	for count := range 2 {
		if _, result := h.stack.TCPOpen(1); result != ResultOK {
			t.Fatalf("ordinary open %d: %d", count, result)
		}
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("past the ordinary quota %d", result)
	}
	h.stack.SetTCPQuota(2, 4)
	for count := range 2 {
		h.stack.TCPOpen(2)
		_ = count
	}
	if _, result := h.stack.TCPOpen(2); result != ResultBusy {
		t.Fatalf("a small quota above the tuning %d", result)
	}
	if h.stack.tcpOwnerQuota(0) != MaxTCPSessionQuota {
		t.Fatalf("explicit quota lowered to %d", h.stack.tcpOwnerQuota(0))
	}
}

func TestTCPListenersCapacity(t *testing.T) {
	h := newHarness(t)
	for owner := range 5 {
		h.stack.SetTCPQuota(owner, MaxTCPSessionQuota)
	}
	for index := range MaxTCPListeners {
		id, _ := h.stack.TCPOpen(index % 5)
		h.stack.TCPBind(index%5, id, LoopbackAddress, uint16(9000+index))
		if result := h.stack.TCPListen(index%5, id, 1); result != ResultOK {
			t.Fatalf("listener %d: %d", index, result)
		}
	}
	id, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, id, LoopbackAddress, 8999)
	if result := h.stack.TCPListen(0, id, 1); result != ResultBusy {
		t.Fatalf("listener past the capacity %d", result)
	}
}

func TestUDPReuseAddressAndDelivery(t *testing.T) {
	h := readyWithPeer(t)
	first := h.open(0)
	if _, _, result := h.stack.UDPBindReuse(0, first, IPv4{}, 5000, true); result != ResultOK {
		t.Fatal(result)
	}
	plain := h.open(1)
	if _, _, result := h.stack.UDPBind(1, plain, IPv4{}, 5000); result != ResultAddressInUse {
		t.Fatalf("bind without reuse %d", result)
	}
	second := h.open(1)
	if _, _, result := h.stack.UDPBindReuse(1, second, IPv4{}, 5000, true); result != ResultOK {
		t.Fatalf("bind with reuse on both %d", result)
	}
	// The endpoint bound last receives unicast.
	h.receive(udpFrame(peerIP, peerMAC, 7, 5000, []byte("x")))
	if _, payload := h.take(1, second); string(payload) != "x" {
		t.Fatalf("got %q", payload)
	}
	h.empty(0, first)
	h.stack.UDPClose(1, second)
	h.receive(udpFrame(peerIP, peerMAC, 7, 5000, []byte("y")))
	if _, payload := h.take(0, first); string(payload) != "y" {
		t.Fatalf("after close got %q", payload)
	}
}

func TestUDPReadinessPeekAndMaxPayload(t *testing.T) {
	h := readyWithPeer(t)
	id := h.open(0)
	port := h.bind(0, id, IPv4{}, 0)
	ready, result := h.stack.UDPReadiness(0, id)
	if result != ResultOK || ready.Mask != UDPReadyWritable || ready.Queued != 0 {
		t.Fatalf("empty %+v %d", ready, result)
	}
	h.receive(udpFrame(peerIP, peerMAC, 7, port, []byte("first")))
	h.receive(udpFrame(peerIP, peerMAC, 7, port, nil))
	ready, _ = h.stack.UDPReadiness(0, id)
	if ready.Mask != UDPReadyWritable|UDPReadyReadable || ready.Next != 5 || ready.Queued != 2 {
		t.Fatalf("queued %+v", ready)
	}
	var buffer [UDPPayloadCapacity]byte
	if _, found := h.stack.UDPPeek(0, id, buffer[:4]); found {
		t.Fatal("peek into a short buffer")
	}
	received, found := h.stack.UDPPeek(0, id, buffer[:])
	if !found || string(buffer[:received.Length]) != "first" || received.Source != peerIP {
		t.Fatalf("peek %+v", received)
	}
	if _, payload := h.take(0, id); string(payload) != "first" {
		t.Fatalf("take after peek %q", payload)
	}
	ready, _ = h.stack.UDPReadiness(0, id)
	if ready.Mask != UDPReadyWritable|UDPReadyReadable || ready.Next != 0 || ready.Queued != 1 {
		t.Fatalf("empty datagram queued %+v", ready)
	}
	if limit, result := h.stack.UDPMaxPayload(0, id, peerIP); result != ResultOK || limit != MTU-28 {
		t.Fatalf("wire limit %d %d", limit, result)
	}
	if limit, result := h.stack.UDPMaxPayload(0, id, LoopbackAddress); result != ResultOK || limit != UDPPayloadCapacity {
		t.Fatalf("loopback limit %d %d", limit, result)
	}
	if result := h.stack.UDPSendTo(0, id, peerIP, 9, make([]byte, MTU-27), h.now); result != ResultMessageSize {
		t.Fatalf("oversized send %d", result)
	}
}

func TestUDPExplicitQuota(t *testing.T) {
	h := newHarness(t)
	h.stack.SetUDPQuota(0, MaxUDPSessionQuota)
	for count := range MaxUDPSessionQuota {
		if _, result := h.stack.UDPOpen(0); result != ResultOK {
			t.Fatalf("open %d: %d", count, result)
		}
	}
	if _, result := h.stack.UDPOpen(0); result != ResultBusy {
		t.Fatalf("past the quota %d", result)
	}
	for count := range MaxUDPSessionEndpoints {
		if _, result := h.stack.UDPOpen(1); result != ResultOK {
			t.Fatalf("ordinary open %d: %d", count, result)
		}
	}
	if _, result := h.stack.UDPOpen(1); result != ResultBusy {
		t.Fatalf("past the ordinary quota %d", result)
	}
}
