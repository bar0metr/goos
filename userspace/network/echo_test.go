package network

import (
	"encoding/binary"
	"testing"
)

// sendEchoToPeer starts an echo to the on-link peer and returns the request
// the stack put on the wire.
func sendEchoToPeer(h *harness, session int, sequence uint16) sentFrame {
	h.t.Helper()
	if result := h.stack.Echo(session, peerIP, sequence, second, h.now); result != ResultOK {
		h.t.Fatalf("echo %d", result)
	}
	request := only(h.t, h.drain())
	if request.ether.destination != peerMAC || request.ip.source != hostIP || request.ip.destination != peerIP ||
		request.icmp.kind != icmpEchoRequest || request.icmp.sequence != sequence || len(request.icmp.body) != EchoPayload {
		h.t.Fatalf("echo request %+v", request)
	}
	return request
}

func replyTo(request sentFrame) []byte {
	return replyWith(request, request.icmp.identifier, request.icmp.sequence, request.icmp.body)
}

func replyWith(request sentFrame, identifier, sequence uint16, body []byte) []byte {
	return ipv4Frame(hostMAC, peerMAC, request.ip.destination, request.ip.source, protocolICMP,
		icmpBytes(icmpEchoReply, 0, identifier, sequence, body))
}

func readyWithPeer(t *testing.T) *harness {
	h := newHarness(t)
	h.readyStatic(staticConfig)
	h.learn(peerIP, peerMAC)
	h.learn(gatewayIP, gatewayMAC)
	return h
}

func TestEchoArgumentAndStateErrors(t *testing.T) {
	h := newHarness(t)
	for _, test := range []struct {
		name        string
		session     int
		destination IPv4
		timeout     uint64
		want        Result
	}{
		{"negative session", -1, peerIP, second, ResultInvalidArgument},
		{"session too large", MaxSessions, peerIP, second, ResultInvalidArgument},
		{"timeout short", 0, peerIP, MinEchoTimeout - 1, ResultInvalidArgument},
		{"timeout long", 0, peerIP, MaxEchoTimeout + 1, ResultInvalidArgument},
		{"zero destination", 0, IPv4{}, second, ResultInvalidArgument},
		{"broadcast", 0, broadcastIPv4, second, ResultInvalidArgument},
		{"multicast", 0, IPv4{224, 0, 0, 1}, second, ResultInvalidArgument},
		{"link down", 0, peerIP, second, ResultDown},
	} {
		if got := h.stack.Echo(test.session, test.destination, 1, test.timeout, h.now); got != test.want {
			t.Errorf("%s: %d, want %d", test.name, got, test.want)
		}
	}
	h.stack.LinkUp(hostMAC, MTU, h.now)
	if got := h.stack.Echo(0, peerIP, 1, second, h.now); got != ResultNoAddress {
		t.Errorf("configuring: %d", got)
	}
	h.stack.Fail(h.now)
	if got := h.stack.Echo(0, peerIP, 1, second, h.now); got != ResultDriverFault {
		t.Errorf("failed: %d", got)
	}

	h = newHarness(t)
	h.readyStatic(Config{Mode: ModeStatic, Address: hostIP, Prefix: 24})
	if got := h.stack.Echo(0, remoteIP, 1, second, h.now); got != ResultNoRoute {
		t.Errorf("no gateway: %d", got)
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Errorf("NoRoute sent %d frames", len(frames))
	}
	for _, special := range []IPv4{{10, 0, 2, 0}, {10, 0, 2, 255}} {
		if got := h.stack.Echo(0, special, 1, second, h.now); got != ResultInvalidArgument {
			t.Errorf("subnet address %v: %d", special, got)
		}
	}
	h.noCompletion()
}

func TestEchoRoundTrip(t *testing.T) {
	h := readyWithPeer(t)
	request := sendEchoToPeer(h, 3, 42)
	if h.stack.Echo(3, peerIP, 43, second, h.now) != ResultBusy {
		t.Fatal("second echo on a busy session accepted")
	}
	h.advance(7 * millisecond)
	h.receive(replyTo(request))
	completion := h.completion()
	if completion.Result != ResultOK || completion.Session != 3 || completion.Sequence != 42 ||
		completion.Responder != peerIP || completion.RTT != 7*millisecond {
		t.Fatalf("completion %+v", completion)
	}
	// A duplicate reply after completion is ignored.
	h.receive(replyTo(request))
	h.noCompletion()
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("deadline left after the reply")
	}
}

func TestEchoViaGateway(t *testing.T) {
	h := readyWithPeer(t)
	if result := h.stack.Echo(0, remoteIP, 1, second, h.now); result != ResultOK {
		t.Fatalf("echo %d", result)
	}
	request := only(t, h.drain())
	if request.ether.destination != gatewayMAC || request.ip.destination != remoteIP {
		t.Fatalf("request %+v", request)
	}
	h.receive(ipv4Frame(hostMAC, gatewayMAC, remoteIP, hostIP, protocolICMP,
		icmpBytes(icmpEchoReply, 0, request.icmp.identifier, 1, request.icmp.body)))
	if completion := h.completion(); completion.Result != ResultOK || completion.Responder != remoteIP {
		t.Fatalf("completion %+v", completion)
	}
}

func TestEchoToOwnAddress(t *testing.T) {
	h := readyWithPeer(t)
	if result := h.stack.Echo(0, hostIP, 5, second, h.now); result != ResultOK {
		t.Fatalf("echo %d", result)
	}
	if frames := h.drain(); len(frames) != 0 {
		t.Fatal("self echo went to the wire")
	}
	if completion := h.completion(); completion.Result != ResultOK || completion.Responder != hostIP || completion.Sequence != 5 {
		t.Fatalf("completion %+v", completion)
	}
}

func TestEchoMismatchedRepliesIgnored(t *testing.T) {
	h := readyWithPeer(t)
	request := sendEchoToPeer(h, 0, 9)
	wrongToken := append([]byte(nil), request.icmp.body...)
	wrongToken[0] ^= 0xff
	wrongFill := append([]byte(nil), request.icmp.body...)
	wrongFill[EchoPayload-1] ^= 0xff
	for name, frame := range map[string][]byte{
		"wrong token":      replyWith(request, request.icmp.identifier, 9, wrongToken),
		"wrong fill":       replyWith(request, request.icmp.identifier, 9, wrongFill),
		"short payload":    replyWith(request, request.icmp.identifier, 9, request.icmp.body[:EchoPayload-1]),
		"long payload":     replyWith(request, request.icmp.identifier, 9, append(append([]byte(nil), request.icmp.body...), 0)),
		"wrong identifier": replyWith(request, request.icmp.identifier+1, 9, request.icmp.body),
		"wrong sequence":   replyWith(request, request.icmp.identifier, 10, request.icmp.body),
		"wrong source": ipv4Frame(hostMAC, peerMAC, IPv4{10, 0, 2, 21}, hostIP, protocolICMP,
			icmpBytes(icmpEchoReply, 0, request.icmp.identifier, 9, request.icmp.body)),
		"request not reply": ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, request.icmp.identifier, 9, request.icmp.body)),
		"nonzero code": ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP,
			icmpBytes(icmpEchoReply, 1, request.icmp.identifier, 9, request.icmp.body)),
		"other type": ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP,
			icmpBytes(13, 0, request.icmp.identifier, 9, request.icmp.body)),
	} {
		h.receive(frame)
		h.drain()
		if completion, ok := h.stack.Completion(); ok {
			t.Fatalf("%s completed the echo: %+v", name, completion)
		}
	}
	h.receive(replyTo(request))
	if completion := h.completion(); completion.Result != ResultOK {
		t.Fatalf("completion %+v", completion)
	}
}

func TestEchoTimeoutAndStaleReply(t *testing.T) {
	h := readyWithPeer(t)
	request := sendEchoToPeer(h, 1, 1)
	h.advance(second - 1)
	h.noCompletion()
	h.advance(1)
	if completion := h.completion(); completion.Result != ResultTimeout || completion.Session != 1 {
		t.Fatalf("completion %+v", completion)
	}
	h.receive(replyTo(request))
	h.noCompletion()
	// A new echo with the same sequence does not accept the stale reply:
	// its token is fresh.
	next := sendEchoToPeer(h, 1, 1)
	if string(next.icmp.body[:echoTokenSize]) == string(request.icmp.body[:echoTokenSize]) {
		t.Fatal("token reused")
	}
	h.receive(replyTo(request))
	h.noCompletion()
	h.receive(replyTo(next))
	if completion := h.completion(); completion.Result != ResultOK {
		t.Fatalf("completion %+v", completion)
	}
}

func icmpError(kind uint8, responder IPv4, request sentFrame, mutate func([]byte)) []byte {
	quoted := append([]byte(nil), request.ether.payload[:ipv4HeaderSize+icmpHeaderSize]...)
	if mutate != nil {
		mutate(quoted)
	}
	return ipv4Frame(hostMAC, gatewayMAC, responder, hostIP, protocolICMP, icmpBytes(kind, 1, 0, 0, quoted))
}

func TestEchoErrors(t *testing.T) {
	router := IPv4{10, 0, 2, 2}
	for _, test := range []struct {
		name string
		kind uint8
		want Result
	}{
		{"unreachable", icmpUnreachable, ResultRemoteUnreachable},
		{"time exceeded", icmpTimeExceeded, ResultTimeExceeded},
	} {
		h := readyWithPeer(t)
		h.stack.Echo(2, remoteIP, 77, second, h.now)
		request := only(t, h.drain())
		for name, mutate := range map[string]func([]byte){
			"quoted source":      func(d []byte) { d[15] = 99 },
			"quoted destination": func(d []byte) { d[19] = 98 },
			"quoted protocol":    func(d []byte) { d[9] = protocolUDP },
			"quoted identifier":  func(d []byte) { d[ipv4HeaderSize+4] ^= 1 },
			"quoted sequence":    func(d []byte) { d[ipv4HeaderSize+7] ^= 1 },
			"quoted reply":       func(d []byte) { d[ipv4HeaderSize] = icmpEchoReply },
		} {
			h.receive(icmpError(test.kind, router, request, mutate))
			if completion, ok := h.stack.Completion(); ok {
				t.Fatalf("%s: %s matched: %+v", test.name, name, completion)
			}
		}
		short := ipv4Frame(hostMAC, gatewayMAC, router, hostIP, protocolICMP,
			icmpBytes(test.kind, 0, 0, 0, request.ether.payload[:icmpQuotedEcho-1]))
		h.receive(short)
		h.noCompletion()

		h.receive(icmpError(test.kind, router, request, nil))
		completion := h.completion()
		if completion.Result != test.want || completion.Responder != router || completion.Sequence != 77 ||
			completion.RTT != 0 {
			t.Fatalf("%s: completion %+v", test.name, completion)
		}
	}
}

func TestAnswerEchoRequests(t *testing.T) {
	h := readyWithPeer(t)
	body := []byte("abcdefghijklmnopqrstuvwxyz")
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 0x4242, 9, body)))
	reply := only(t, h.drain())
	if reply.ether.destination != peerMAC || reply.ip.source != hostIP || reply.ip.destination != peerIP ||
		reply.icmp.kind != icmpEchoReply || reply.icmp.identifier != 0x4242 || reply.icmp.sequence != 9 ||
		string(reply.icmp.body) != string(body) {
		t.Fatalf("reply %+v", reply)
	}

	large := make([]byte, MTU-ipv4HeaderSize-icmpHeaderSize)
	for index := range large {
		large[index] = byte(index * 7)
	}
	h.advance(second)
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, large)))
	if reply := only(t, h.drain()); string(reply.icmp.body) != string(large) {
		t.Fatal("full-size echo payload changed")
	}

	refused := map[string][]byte{
		"limited broadcast": ipv4Frame(broadcastMAC, peerMAC, peerIP, broadcastIPv4, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
		"directed broadcast": ipv4Frame(broadcastMAC, peerMAC, peerIP, IPv4{10, 0, 2, 255}, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
		"link broadcast": ipv4Frame(broadcastMAC, peerMAC, peerIP, hostIP, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
		"another host": ipv4Frame(hostMAC, peerMAC, peerIP, IPv4{10, 0, 2, 77}, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
		"bad checksum": func() []byte {
			frame := ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, body))
			frame[len(frame)-1] ^= 1
			return frame
		}(),
		"broadcast source": ipv4Frame(hostMAC, peerMAC, IPv4{10, 0, 2, 255}, hostIP, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
		"zero source": ipv4Frame(hostMAC, peerMAC, IPv4{}, hostIP, protocolICMP,
			icmpBytes(icmpEchoRequest, 0, 1, 1, body)),
	}
	for name, frame := range refused {
		h.receive(frame)
		if frames := h.drain(); len(frames) != 0 {
			t.Errorf("%s answered", name)
		}
	}
}

func TestAnswerEchoRateLimit(t *testing.T) {
	h := readyWithPeer(t)
	request := ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("x")))
	answered := 0
	for index := 0; index < 3*int(defaults.EchoReplyRate); index++ {
		h.receive(request)
		answered += len(h.drain())
		h.now += 10 * millisecond
	}
	// 60 requests over 600 ms fall in one window.
	if answered != int(defaults.EchoReplyRate) {
		t.Fatalf("answered %d in one second", answered)
	}
	h.now += second
	h.receive(request)
	if len(h.drain()) != 1 {
		t.Fatal("limit did not reset")
	}
	if drop := h.stack.Snapshot(h.now).Drop; drop < 2*uint64(defaults.EchoReplyRate) {
		t.Fatalf("drop counter %d", drop)
	}
}

func TestEchoRequestBuildsValidFrame(t *testing.T) {
	h := readyWithPeer(t)
	request := sendEchoToPeer(h, 0, 0x0102)
	payload := request.ether.payload
	if binary.BigEndian.Uint16(payload[2:4]) != ipv4HeaderSize+icmpHeaderSize+EchoPayload {
		t.Fatal("total length")
	}
	if payload[0] != 0x45 || payload[9] != protocolICMP {
		t.Fatal("header fields")
	}
	first := binary.BigEndian.Uint16(payload[4:6])
	h.stack.CancelSession(0)
	again := sendEchoToPeer(h, 0, 0x0102)
	if binary.BigEndian.Uint16(again.ether.payload[4:6]) == first {
		t.Fatal("identification did not advance")
	}
	if request.icmp.identifier != again.icmp.identifier {
		t.Fatal("identifier is per session")
	}
	other := sendEchoToPeer(h, 1, 0x0102)
	if other.icmp.identifier == again.icmp.identifier {
		t.Fatal("sessions share an identifier")
	}
}

func TestCancelSessionDropsCompletion(t *testing.T) {
	h := readyWithPeer(t)
	request := sendEchoToPeer(h, 4, 1)
	h.stack.CancelSession(4)
	h.receive(replyTo(request))
	h.noCompletion()
	h.runUntil(h.now + claimTime)
	h.noCompletion()
	h.stack.CancelSession(-1)
	h.stack.CancelSession(MaxSessions)
	sendEchoToPeer(h, 4, 2)
}

func TestCompletionHeldUntilTaken(t *testing.T) {
	h := readyWithPeer(t)
	sendEchoToPeer(h, 0, 1)
	h.advance(second)
	if result := h.stack.Echo(0, peerIP, 2, second, h.now); result != ResultBusy {
		t.Fatalf("echo on a session with an untaken completion: %d", result)
	}
	h.completion()
	sendEchoToPeer(h, 0, 2)
}

func TestTransmitRingFull(t *testing.T) {
	h := readyWithPeer(t)
	for session := 0; session < MaxSessions; session++ {
		if result := h.stack.Echo(session, peerIP, 1, second, h.now); result != ResultOK {
			t.Fatalf("echo %d", result)
		}
	}
	if h.stack.PendingFrames() != TxQueueFrames {
		t.Fatalf("pending %d", h.stack.PendingFrames())
	}
	before := h.stack.Snapshot(h.now).Drop
	h.receive(arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP,
		targetIP: hostIP}))
	if h.stack.Snapshot(h.now).Drop != before+1 || h.stack.PendingFrames() != TxQueueFrames {
		t.Fatal("full ring did not count a drop")
	}
	var small [10]byte
	if _, ok := h.stack.NextFrame(small[:]); ok {
		t.Fatal("frame copied into a short buffer")
	}
	if frames := h.drain(); len(frames) != TxQueueFrames {
		t.Fatalf("drained %d", len(frames))
	}
	if tx := h.stack.Snapshot(h.now).TX; tx < TxQueueFrames {
		t.Fatalf("tx %d", tx)
	}
}
