package network

import (
	"encoding/binary"
	"testing"
)

func TestTCPSequenceArithmeticWrap(t *testing.T) {
	if !seqLT(0xffffffff, 0) || !seqGT(0, 0xffffffff) || !seqBetween(0, 0xffffffff, 1) {
		t.Fatal("sequence comparison does not cross wrap")
	}
	if !seqGT(0x80000000, 0x7fffffff) || !seqLT(0x7fffffff, 0x80000000) {
		t.Fatal("sequence comparison failed at the sign boundary")
	}
}

func TestTCPWireMSSAndChecksum(t *testing.T) {
	source, destination := IPv4{192, 0, 2, 1}, IPv4{198, 51, 100, 2}
	var bytes [64]byte
	written := putTCP(bytes[:], source, destination, tcpSegment{sourcePort: 1234, destinationPort: 80,
		sequence: 7, flags: tcpFlagSYN, window: 4096, mss: 1200})
	parsed, status := parseTCP(ipv4Packet{source: source, destination: destination, protocol: protocolTCP, payload: bytes[:written]})
	if status != tcpOK || parsed.mss != 1200 || parsed.sequence != 7 || parsed.flags != tcpFlagSYN {
		t.Fatalf("parsed %+v status %d", parsed, status)
	}
	bytes[5] ^= 1
	if _, status = parseTCP(ipv4Packet{source: source, destination: destination, protocol: protocolTCP, payload: bytes[:written]}); status != tcpBadChecksum {
		t.Fatalf("corrupt checksum status %d", status)
	}
}

func TestTCPWireRejectsMalformedHeadersAndOptions(t *testing.T) {
	source, destination := IPv4{192, 0, 2, 1}, IPv4{198, 51, 100, 2}
	valid := func(data []byte) []byte {
		data = append([]byte(nil), data...)
		data[16], data[17] = 0, 0
		binary.BigEndian.PutUint16(data[16:18], checksumFold(checksumAdd(
			pseudoHeaderSum(source, destination, protocolTCP, len(data)), data)))
		return data
	}
	base := make([]byte, 24)
	binary.BigEndian.PutUint16(base[0:2], 1234)
	binary.BigEndian.PutUint16(base[2:4], 80)
	base[12], base[13] = 6<<4, tcpFlagSYN
	base[20], base[21], base[22], base[23] = tcpOptionMSS, 4, 0x04, 0xb0
	if segment, status := parseTCP(ipv4Packet{source: source, destination: destination, payload: valid(base)}); status != tcpOK || segment.mss != 1200 {
		t.Fatalf("valid MSS status %d segment %+v", status, segment)
	}
	for _, data := range [][]byte{
		make([]byte, 19),
		append([]byte(nil), base[:20]...),
		func() []byte { b := append([]byte(nil), base...); b[12] = 4 << 4; return b }(),
		func() []byte { b := append([]byte(nil), base...); b[20], b[21] = 30, 1; return b }(),
		func() []byte { b := append([]byte(nil), base...); b[20], b[21] = tcpOptionMSS, 3; return b }(),
		func() []byte { b := append([]byte(nil), base...); b[0], b[1] = 0, 0; return b }(),
	} {
		if len(data) >= tcpHeaderSize {
			data = valid(data)
		}
		if _, status := parseTCP(ipv4Packet{source: source, destination: destination, payload: data}); status == tcpOK {
			t.Fatalf("accepted malformed header %x", data)
		}
	}
}

func TestTCPWireMaximumSegment(t *testing.T) {
	source, destination := IPv4{192, 0, 2, 1}, IPv4{198, 51, 100, 2}
	var data [MTU - ipv4HeaderSize]byte
	payload := make([]byte, len(data)-tcpHeaderSize)
	for index := range payload {
		payload[index] = byte(index)
	}
	written := putTCP(data[:], source, destination, tcpSegment{sourcePort: 1234, destinationPort: 80,
		sequence: 1, acknowledgment: 2, flags: tcpFlagACK, window: 4096, payload: payload})
	segment, status := parseTCP(ipv4Packet{source: source, destination: destination, payload: data[:written]})
	if status != tcpOK || len(segment.payload) != len(payload) {
		t.Fatalf("maximum segment status %d payload %d", status, len(segment.payload))
	}
}

func TestTCPLoopbackStreamAndHalfClose(t *testing.T) {
	h := newHarness(t)
	listener, result := h.stack.TCPOpen(0)
	if result != ResultOK {
		t.Fatal(result)
	}
	if _, _, result = h.stack.TCPBind(0, listener, LoopbackAddress, 8080); result != ResultOK {
		t.Fatal(result)
	}
	if result = h.stack.TCPListen(0, listener, 2); result != ResultOK {
		t.Fatal(result)
	}
	client, result := h.stack.TCPOpen(1)
	if result != ResultOK {
		t.Fatal(result)
	}
	if _, result = h.stack.TCPConnect(1, client, LoopbackAddress, 8080, h.now); result != ResultOK {
		t.Fatal(result)
	}
	server, tuple, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK || tuple.Remote != LoopbackAddress {
		t.Fatalf("accept %d %+v", result, tuple)
	}
	payload := []byte("a stream larger than one tiny read")
	if written, result := h.stack.TCPWrite(1, client, payload, h.now); result != ResultOK || written != len(payload) {
		t.Fatalf("write %d %d", written, result)
	}
	var first [7]byte
	if read, result := h.stack.TCPRead(0, server, first[:], h.now); result != ResultOK || read != len(first) {
		t.Fatalf("first read %d %d", read, result)
	}
	var rest [64]byte
	read, result := h.stack.TCPRead(0, server, rest[:], h.now)
	if result != ResultOK || string(append(first[:], rest[:read]...)) != string(payload) {
		t.Fatalf("rest read %d %d", read, result)
	}
	if result = h.stack.TCPShutdownWrite(1, client, h.now); result != ResultOK {
		t.Fatal(result)
	}
	if read, result = h.stack.TCPRead(0, server, rest[:], h.now); result != ResultEOF || read != 0 {
		t.Fatalf("EOF read %d %d", read, result)
	}
	if written, result := h.stack.TCPWrite(0, server, []byte("reply"), h.now); result != ResultOK || written != 5 {
		t.Fatalf("reply %d %d", written, result)
	}
	if read, result = h.stack.TCPRead(1, client, rest[:], h.now); result != ResultOK || string(rest[:read]) != "reply" {
		t.Fatalf("reply read %d %d %q", read, result, rest[:read])
	}
}

func TestTCPStaleIDAndBindConflict(t *testing.T) {
	h := newHarness(t)
	first, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, first, IPv4{}, 9000); result != ResultOK {
		t.Fatal(result)
	}
	second, _ := h.stack.TCPOpen(1)
	if _, _, result := h.stack.TCPBind(1, second, LoopbackAddress, 9000); result != ResultAddressInUse {
		t.Fatalf("conflicting bind result %d", result)
	}
	if result := h.stack.TCPClose(0, first, h.now); result != ResultOK {
		t.Fatal(result)
	}
	reused, _ := h.stack.TCPOpen(0)
	if reused == first {
		t.Fatal("generation did not change")
	}
	if result := h.stack.TCPClose(0, first, h.now); result != ResultInvalidArgument {
		t.Fatalf("stale close result %d", result)
	}
}

func TestTCPWindowUpdateResumesBufferedWrite(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8081); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	client, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 8081, h.now); result != ResultOK {
		t.Fatal(result)
	}
	server, _, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK {
		t.Fatal(result)
	}
	clientEntry, _ := h.stack.tcpEndpoint(1, client)
	clientEntry.peerWindow = 0
	if written, result := h.stack.TCPWrite(1, client, []byte("resume"), h.now); result != ResultOK || written != 6 {
		t.Fatalf("write %d %d", written, result)
	}
	var buffer [16]byte
	if read, result := h.stack.TCPRead(0, server, buffer[:], h.now); result != ResultBusy || read != 0 {
		t.Fatalf("read before window update %d %d", read, result)
	}
	h.stack.handleACK(clientEntry, ackSegment(clientEntry, clientEntry.sndUna, 4096), h.now)
	if read, result := h.stack.TCPRead(0, server, buffer[:], h.now); result != ResultOK || string(buffer[:read]) != "resume" {
		t.Fatalf("read after window update %d %d %q", read, result, buffer[:read])
	}
}

func TestTCPInvalidACKDoesNotChangeWindow(t *testing.T) {
	entry := tcpEndpoint{state: TCPEstablished, sndUna: 10, sndNxt: 20, peerWindow: 30}
	stack := Stack{tuning: DefaultTuning()}
	stack.handleACK(&entry, ackSegment(&entry, 21, 4000), second)
	if entry.sndUna != 10 || entry.peerWindow != 30 {
		t.Fatalf("invalid ACK changed sender: una=%d window=%d", entry.sndUna, entry.peerWindow)
	}
}

func TestTCPZeroWindowPersistAndReopen(t *testing.T) {
	h := readyWithPeer(t)
	id, result := h.stack.TCPOpen(0)
	if result != ResultOK {
		t.Fatal(result)
	}
	entry, _ := h.stack.tcpEndpoint(0, id)
	entry.state = TCPEstablished
	entry.local, entry.port = hostIP, 50000
	entry.remote, entry.remotePort = peerIP, 7
	entry.localMSS, entry.peerMSS = 1460, 1200
	entry.sndUna, entry.sndNxt, entry.sendSeq = 100, 100, 100
	entry.rcvNxt, entry.peerWindow, entry.cwnd = 200, 0, tcpInitialWindow(entry)
	if written, result := h.stack.TCPWrite(0, id, []byte("x"), h.now); result != ResultOK || written != 1 {
		t.Fatalf("write %d %d", written, result)
	}
	h.stack.advanceTCP(h.now)
	if entry.rtoDeadline == 0 {
		t.Fatal("persist timer not armed")
	}
	h.advance(entry.rtoDeadline - h.now)
	frames := h.drain()
	if len(frames) != 1 {
		t.Fatalf("persist frames %d", len(frames))
	}
	ether, ok := parseEthernet(frames[0])
	if !ok {
		t.Fatal("persist Ethernet frame")
	}
	packet, parseStatus := parseIPv4(ether.payload)
	if parseStatus != parseOK {
		t.Fatalf("persist IPv4 status %d", parseStatus)
	}
	segment, status := parseTCP(packet)
	if status != tcpOK || segment.sequence != 100 || string(segment.payload) != "x" || entry.sndNxt != 100 || entry.sendSent != 0 || entry.persist != 1 {
		t.Fatalf("probe status=%d segment=%+v sndNxt=%d sent=%d probes=%d", status, segment, entry.sndNxt, entry.sendSent, entry.persist)
	}
	h.stack.handleACK(entry, ackSegment(entry, entry.sndUna, 4096), h.now)
	frames = h.drain()
	if len(frames) != 1 {
		t.Fatalf("resume frames %d", len(frames))
	}
	ether, ok = parseEthernet(frames[0])
	if !ok {
		t.Fatal("resume Ethernet frame")
	}
	packet, parseStatus = parseIPv4(ether.payload)
	if parseStatus != parseOK {
		t.Fatalf("resume IPv4 status %d", parseStatus)
	}
	segment, status = parseTCP(packet)
	if status != tcpOK || segment.sequence != 100 || string(segment.payload) != "x" || entry.sndNxt != 101 || entry.sendSent != 1 || entry.persist != 0 {
		t.Fatalf("resume status=%d segment=%+v sndNxt=%d sent=%d probes=%d", status, segment, entry.sndNxt, entry.sendSent, entry.persist)
	}
}

func TestTCPOutOfOrderOverlapAndDuplicate(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	entry := tcpEndpoint{state: TCPEstablished, rcvNxt: 100, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 103, flags: tcpFlagACK, payload: []byte("def")}, second)
	if entry.recvLen != 0 || !entry.reorder[0].used {
		t.Fatalf("later segment delivered early: recv=%d held=%v", entry.recvLen, entry.reorder[0].used)
	}
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK, payload: []byte("abc")}, second)
	if entry.rcvNxt != 106 || string(entry.receive[:entry.recvLen]) != "abcdef" {
		t.Fatalf("reassembled rcvNxt=%d data=%q", entry.rcvNxt, entry.receive[:entry.recvLen])
	}
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK, payload: []byte("abcdef")}, second)
	if entry.rcvNxt != 106 || string(entry.receive[:entry.recvLen]) != "abcdef" {
		t.Fatalf("duplicate changed stream: rcvNxt=%d data=%q", entry.rcvNxt, entry.receive[:entry.recvLen])
	}

	entry = tcpEndpoint{state: TCPEstablished, rcvNxt: 200, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 202, flags: tcpFlagACK, payload: []byte("cdef")}, second)
	stack.handlePayload(&entry, tcpSegment{sequence: 200, flags: tcpFlagACK, payload: []byte("abcd")}, second)
	if entry.rcvNxt != 206 || string(entry.receive[:entry.recvLen]) != "abcdef" {
		t.Fatalf("overlap reassembled rcvNxt=%d data=%q", entry.rcvNxt, entry.receive[:entry.recvLen])
	}
}

func TestTCPOutOfOrderFINWaitsForGap(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	entry := tcpEndpoint{state: TCPEstablished, rcvNxt: 100, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 103, flags: tcpFlagACK | tcpFlagFIN}, second)
	if entry.finReceived {
		t.Fatal("out-of-order FIN delivered before gap")
	}
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK, payload: []byte("abc")}, second)
	if !entry.finReceived || entry.rcvNxt != 104 || entry.state != TCPCloseWait || string(entry.receive[:entry.recvLen]) != "abc" {
		t.Fatalf("FIN promotion state=%d rcvNxt=%d fin=%v data=%q", entry.state, entry.rcvNxt, entry.finReceived, entry.receive[:entry.recvLen])
	}
}

func TestTCPAcceptedChildLeavesListenBacklog(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8082); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	first, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, first, LoopbackAddress, 8082, h.now); result != ResultOK {
		t.Fatal(result)
	}
	if _, _, result := h.stack.TCPAccept(0, listener); result != ResultOK {
		t.Fatalf("first accept %d", result)
	}
	secondID, _ := h.stack.TCPOpen(2)
	if _, result := h.stack.TCPConnect(2, secondID, LoopbackAddress, 8082, h.now); result != ResultOK {
		t.Fatal(result)
	}
	if _, _, result := h.stack.TCPAccept(0, listener); result != ResultOK {
		t.Fatalf("second accept with backlog one %d", result)
	}
}

func TestTCPEmbryonicTimeoutReleasesBacklog(t *testing.T) {
	h := readyWithPeer(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, IPv4{}, 8083); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	var bytes [64]byte
	length := putTCP(bytes[:], peerIP, hostIP, tcpSegment{sourcePort: 50000, destinationPort: 8083,
		sequence: 10, flags: tcpFlagSYN, window: 4096, mss: 1200})
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolTCP, bytes[:length]))
	if children := h.stack.listenerChildren(listener); children != 1 {
		t.Fatalf("embryonic children %d", children)
	}
	h.drain()
	h.runUntil(h.now + h.stack.tuning.TCP.ConnectTimeout)
	if children := h.stack.listenerChildren(listener); children != 0 {
		t.Fatalf("timed-out children %d", children)
	}
}

func TestTCPReconfigurationKeepsOnlyWildcardAndLoopbackListeners(t *testing.T) {
	h := readyWithPeer(t)
	specific, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, specific, hostIP, 8084)
	h.stack.TCPListen(0, specific, 1)
	wildcard, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, wildcard, IPv4{}, 8085)
	h.stack.TCPListen(0, wildcard, 1)
	loopback, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, loopback, LoopbackAddress, 8086)
	h.stack.TCPListen(0, loopback, 1)
	h.stack.reconfigureTCP(ResultReconfigured)
	if _, _, result := h.stack.TCPState(0, specific); result != ResultInvalidArgument {
		t.Fatalf("specific listener survived: %d", result)
	}
	for _, id := range []TCPEndpointID{wildcard, loopback} {
		if state, _, result := h.stack.TCPState(0, id); result != ResultOK || state != TCPListen {
			t.Fatalf("listener %d state=%d result=%d", id, state, result)
		}
	}
}

func TestTCPFastRetransmitAndRecovery(t *testing.T) {
	h := readyWithPeer(t)
	id, result := h.stack.TCPOpen(0)
	if result != ResultOK {
		t.Fatal(result)
	}
	entry, _ := h.stack.tcpEndpoint(0, id)
	entry.state = TCPEstablished
	entry.local, entry.port = hostIP, 50001
	entry.remote, entry.remotePort = peerIP, 7
	entry.localMSS, entry.peerMSS = 100, 100
	entry.sndUna, entry.sndNxt, entry.sendSeq = 1000, 1000, 1000
	entry.rcvNxt, entry.peerWindow = 2000, 4096
	entry.cwnd, entry.ssthresh = 1000, 8000
	if written, result := h.stack.TCPWrite(0, id, make([]byte, 400), h.now); result != ResultOK || written != 400 {
		t.Fatalf("write %d %d", written, result)
	}
	if frames := h.drain(); len(frames) != 4 {
		t.Fatalf("initial segments %d", len(frames))
	}
	for range 3 {
		h.stack.handleACK(entry, ackSegment(entry, entry.sndUna, entry.peerWindow), h.now)
	}
	if !entry.fastRecover || entry.recover != 1400 || entry.dupACKs != 3 {
		t.Fatalf("fast recovery=%v recover=%d dupacks=%d", entry.fastRecover, entry.recover, entry.dupACKs)
	}
	if frames := h.drain(); len(frames) != 1 {
		t.Fatalf("fast retransmit frames %d", len(frames))
	}
	cwnd := entry.cwnd
	h.stack.handleACK(entry, ackSegment(entry, entry.sndUna, entry.peerWindow), h.now)
	if entry.cwnd != cwnd+100 {
		t.Fatalf("recovery cwnd %d -> %d", cwnd, entry.cwnd)
	}
	h.stack.handleACK(entry, ackSegment(entry, entry.recover, entry.peerWindow), h.now)
	if entry.fastRecover || entry.cwnd != entry.ssthresh {
		t.Fatalf("recovery exit fast=%v cwnd=%d ssthresh=%d", entry.fastRecover, entry.cwnd, entry.ssthresh)
	}
}

func TestTCPTimeWaitOnlyRefreshesForExactFIN(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	now := uint64(100 * second)
	entry := tcpEndpoint{state: TCPTimeWait, rcvNxt: 101, sndNxt: 50, deadline: now + second}
	stack.handleTCP(&entry, ipv4Packet{}, tcpSegment{sequence: 90, flags: tcpFlagACK | tcpFlagFIN}, now)
	if entry.deadline != now+second {
		t.Fatalf("stale FIN refreshed TIME_WAIT to %d", entry.deadline)
	}
	stack.handleTCP(&entry, ipv4Packet{}, tcpSegment{sequence: 100, flags: tcpFlagACK | tcpFlagFIN}, now)
	if entry.deadline != now+stack.tuning.TCP.TimeWait {
		t.Fatalf("exact FIN deadline %d", entry.deadline)
	}
}

func ackSegment(entry *tcpEndpoint, acknowledgment, window uint32) tcpSegment {
	return tcpSegment{sequence: entry.rcvNxt, acknowledgment: acknowledgment, flags: tcpFlagACK, window: uint16(window)}
}

// establishedWithPeer builds a synchronized connection to peerIP whose
// segments travel through the harness.
func establishedWithPeer(t *testing.T, h *harness, port uint16) (TCPEndpointID, *tcpEndpoint) {
	t.Helper()
	id, result := h.stack.TCPOpen(0)
	if result != ResultOK {
		t.Fatal(result)
	}
	entry, _ := h.stack.tcpEndpoint(0, id)
	entry.state = TCPEstablished
	entry.local, entry.port = hostIP, port
	entry.remote, entry.remotePort = peerIP, 7
	entry.localMSS, entry.peerMSS = 1460, 1460
	entry.sndUna, entry.sndNxt, entry.sendSeq = 100, 100, 100
	entry.rcvNxt, entry.peerWindow, entry.cwnd = 200, 1000, 14600
	entry.sndWL1, entry.sndWL2 = 200, 100
	return id, entry
}

func (h *harness) tcpFromPeer(entry *tcpEndpoint, segment tcpSegment) {
	h.t.Helper()
	segment.sourcePort, segment.destinationPort = entry.remotePort, entry.port
	var bytes [MTU]byte
	length := putTCP(bytes[:], peerIP, h.stack.address, segment)
	h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, h.stack.address, protocolTCP, bytes[:length]))
}

func (h *harness) tcpSent() []tcpSegment {
	h.t.Helper()
	var segments []tcpSegment
	for _, frame := range h.drain() {
		ether, ok := parseEthernet(frame)
		if !ok {
			h.t.Fatal("sent frame is not Ethernet")
		}
		packet, status := parseIPv4(ether.payload)
		if status != parseOK || packet.protocol != protocolTCP {
			continue
		}
		segment, tcpStatus := parseTCP(packet)
		if tcpStatus != tcpOK {
			h.t.Fatalf("sent TCP status %d", tcpStatus)
		}
		segments = append(segments, segment)
	}
	return segments
}

func TestTCPFutureACKDropsWholeSegment(t *testing.T) {
	h := readyWithPeer(t)
	_, entry := establishedWithPeer(t, h, 50010)
	h.tcpFromPeer(entry, tcpSegment{sequence: 200, acknowledgment: 105, flags: tcpFlagACK | tcpFlagPSH | tcpFlagFIN,
		window: 9000, payload: []byte("abc")})
	if entry.recvLen != 0 || entry.rcvNxt != 200 || entry.finReceived || entry.state != TCPEstablished {
		t.Fatalf("future ACK delivered: recv=%d rcvNxt=%d fin=%v state=%d", entry.recvLen, entry.rcvNxt, entry.finReceived, entry.state)
	}
	if entry.peerWindow != 1000 || entry.sndUna != 100 || entry.sndWL1 != 200 || entry.sndWL2 != 100 {
		t.Fatalf("future ACK changed sender: window=%d una=%d wl=%d/%d", entry.peerWindow, entry.sndUna, entry.sndWL1, entry.sndWL2)
	}
	sent := h.tcpSent()
	if len(sent) != 1 || sent[0].flags != tcpFlagACK || sent[0].sequence != 100 || sent[0].acknowledgment != 200 {
		t.Fatalf("corrective ACK %+v", sent)
	}
}

func TestTCPOldACKStillDeliversPayload(t *testing.T) {
	h := readyWithPeer(t)
	_, entry := establishedWithPeer(t, h, 50011)
	h.tcpFromPeer(entry, tcpSegment{sequence: 200, acknowledgment: 90, flags: tcpFlagACK, window: 9000, payload: []byte("abc")})
	if entry.rcvNxt != 203 || string(entry.receive[:entry.recvLen]) != "abc" || entry.peerWindow != 1000 {
		t.Fatalf("old ACK: rcvNxt=%d data=%q window=%d", entry.rcvNxt, entry.receive[:entry.recvLen], entry.peerWindow)
	}
}

func TestTCPStaleSegmentKeepsNewerWindow(t *testing.T) {
	for _, start := range []uint32{200, 0xfffffff0} {
		stack := Stack{tuning: DefaultTuning()}
		entry := tcpEndpoint{state: TCPEstablished, sndUna: 100, sndNxt: 100, rcvNxt: start, peerWindow: 1000,
			sndWL1: start, sndWL2: 100, localMSS: 1460, peerMSS: 1460}
		// The later segment arrives first and is held for reassembly.
		later := tcpSegment{sequence: start + 20, acknowledgment: 100, flags: tcpFlagACK, window: 9000, payload: make([]byte, 10)}
		stack.handleTCP(&entry, ipv4Packet{}, later, second)
		if entry.peerWindow != 9000 || entry.sndWL1 != start+20 {
			t.Fatalf("start %#x: newer window %d wl1=%#x", start, entry.peerWindow, entry.sndWL1)
		}
		earlier := tcpSegment{sequence: start, acknowledgment: 100, flags: tcpFlagACK, window: 100, payload: make([]byte, 20)}
		stack.handleTCP(&entry, ipv4Packet{}, earlier, second)
		if entry.peerWindow != 9000 || entry.rcvNxt != start+30 {
			t.Fatalf("start %#x: stale segment set window %d rcvNxt=%#x", start, entry.peerWindow, entry.rcvNxt)
		}
		// Same SEQ and an equal ACK may shrink the window.
		stack.handleTCP(&entry, ipv4Packet{}, ackSegment(&entry, 100, 500), second)
		if entry.peerWindow != 500 || entry.sndWL1 != start+30 {
			t.Fatalf("start %#x: current ACK window %d wl1=%#x", start, entry.peerWindow, entry.sndWL1)
		}
	}
}

func TestTCPWindowUpdateNeedsNewerACKAtSameSequence(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	entry := tcpEndpoint{state: TCPEstablished, sndUna: 0xfffffff8, sndNxt: 0x10, rcvNxt: 300, peerWindow: 1000,
		sndWL1: 300, sndWL2: 0xfffffffc, localMSS: 1460, peerMSS: 1460}
	stack.handleACK(&entry, ackSegment(&entry, 0xfffffffa, 7000), second)
	if entry.peerWindow != 1000 {
		t.Fatalf("older ACK at the same SEQ set window %d", entry.peerWindow)
	}
	stack.handleACK(&entry, ackSegment(&entry, 0x4, 7000), second)
	if entry.peerWindow != 7000 || entry.sndWL2 != 0x4 {
		t.Fatalf("wrapped newer ACK window %d wl2=%#x", entry.peerWindow, entry.sndWL2)
	}
}

func TestTCPInitialWindowFollowsSMSS(t *testing.T) {
	for _, test := range []struct {
		mss  uint16
		want uint32
	}{{536, 5360}, {1200, 12000}, {1460, 14600}} {
		h := readyWithPeer(t)
		id, _ := h.stack.TCPOpen(0)
		if _, result := h.stack.TCPConnect(0, id, peerIP, 7, h.now); result != ResultOK {
			t.Fatal(result)
		}
		entry, _ := h.stack.tcpEndpoint(0, id)
		syn := h.tcpSent()
		if len(syn) != 1 {
			t.Fatalf("SYN frames %d", len(syn))
		}
		h.tcpFromPeer(entry, tcpSegment{sequence: 5000, acknowledgment: syn[0].sequence + 1, flags: tcpFlagSYN | tcpFlagACK,
			window: 65535, mss: test.mss})
		if entry.state != TCPEstablished || entry.cwnd != test.want {
			t.Fatalf("active MSS %d: state=%d cwnd=%d want %d", test.mss, entry.state, entry.cwnd, test.want)
		}

		listener, _ := h.stack.TCPOpen(1)
		h.stack.TCPBind(1, listener, IPv4{}, 8090)
		h.stack.TCPListen(1, listener, 1)
		h.drain()
		peer := tcpEndpoint{port: 8090, remotePort: 40000}
		h.tcpFromPeer(&peer, tcpSegment{sequence: 9000, flags: tcpFlagSYN, window: 65535, mss: test.mss})
		synACK := h.tcpSent()
		if len(synACK) != 1 {
			t.Fatalf("SYN-ACK frames %d", len(synACK))
		}
		h.tcpFromPeer(&peer, tcpSegment{sequence: 9001, acknowledgment: synACK[0].sequence + 1, flags: tcpFlagACK, window: 65535})
		child, _, result := h.stack.TCPAccept(1, listener)
		if result != ResultOK {
			t.Fatalf("accept %d", result)
		}
		childEntry, _ := h.stack.tcpEndpoint(1, child)
		if childEntry.cwnd != test.want {
			t.Fatalf("passive MSS %d: cwnd=%d want %d", test.mss, childEntry.cwnd, test.want)
		}
	}
}

func TestTCPInitialWindowAfterLostSYNIsOneSegment(t *testing.T) {
	h := readyWithPeer(t)
	id, _ := h.stack.TCPOpen(0)
	h.stack.TCPConnect(0, id, peerIP, 7, h.now)
	entry, _ := h.stack.tcpEndpoint(0, id)
	h.advance(entry.rtoDeadline - h.now)
	syn := h.tcpSent()
	if len(syn) != 2 || entry.retries != 1 {
		t.Fatalf("SYN frames %d retries %d", len(syn), entry.retries)
	}
	h.tcpFromPeer(entry, tcpSegment{sequence: 5000, acknowledgment: syn[0].sequence + 1, flags: tcpFlagSYN | tcpFlagACK,
		window: 65535, mss: 1200})
	if entry.state != TCPEstablished || entry.cwnd != 1200 || entry.ssthresh != tcpInitialSSThresh {
		t.Fatalf("state=%d cwnd=%d ssthresh=%d", entry.state, entry.cwnd, entry.ssthresh)
	}
}

func TestTCPPassiveCloseReleasesOrphan(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8091); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	var buffer [8]byte
	for round := range 3 * MaxTCPEndpointsPerSession {
		client, result := h.stack.TCPOpen(1)
		if result != ResultOK {
			t.Fatalf("round %d client open %d", round, result)
		}
		if _, result = h.stack.TCPConnect(1, client, LoopbackAddress, 8091, h.now); result != ResultOK {
			t.Fatalf("round %d connect %d", round, result)
		}
		server, _, result := h.stack.TCPAccept(0, listener)
		if result != ResultOK {
			t.Fatalf("round %d accept %d", round, result)
		}
		h.stack.TCPShutdownWrite(1, client, h.now)
		if _, result = h.stack.TCPRead(0, server, buffer[:], h.now); result != ResultEOF {
			t.Fatalf("round %d EOF %d", round, result)
		}
		h.stack.TCPClose(0, server, h.now)
		if state, _, _ := h.stack.TCPState(1, client); state != TCPTimeWait {
			t.Fatalf("round %d client state %d", round, state)
		}
		if _, _, result = h.stack.TCPState(0, server); result != ResultInvalidArgument {
			t.Fatalf("round %d server endpoint survived LAST-ACK", round)
		}
		h.stack.TCPClose(1, client, h.now)
		if stats := h.stack.TCPStats(); stats.Endpoints != 1 {
			t.Fatalf("round %d endpoints %d states %v", round, stats.Endpoints, stats.States)
		}
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("released endpoints left a timer")
	}
}

func TestTCPOrphanFinWait2TimesOut(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50012)
	h.stack.TCPClose(0, id, h.now)
	fin := h.tcpSent()
	if len(fin) != 1 || fin[0].flags&tcpFlagFIN == 0 || entry.state != TCPFinWait1 {
		t.Fatalf("FIN %+v state %d", fin, entry.state)
	}
	h.tcpFromPeer(entry, ackSegment(entry, entry.sndNxt, 1000))
	if entry.state != TCPFinWait2 {
		t.Fatalf("state %d", entry.state)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if stats := h.stack.TCPStats(); stats.Endpoints != 0 {
		t.Fatalf("orphan FIN-WAIT-2 kept %d endpoints", stats.Endpoints)
	}
}

func TestTCPRetransmitLimitClosesConnection(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50013)
	if written, result := h.stack.TCPWrite(0, id, []byte("x"), h.now); result != ResultOK || written != 1 {
		t.Fatalf("write %d %d", written, result)
	}
	h.drain()
	if sent := h.runWithSilentPeer(h.now + 30*60*second); len(sent) != tcpMaxRetransmits {
		t.Fatalf("retransmitted segments %d", len(sent))
	}
	if retransmits := h.stack.TCPStats().Retransmits; retransmits != tcpMaxRetransmits {
		t.Fatalf("retransmits %d", retransmits)
	}
	if entry.state != TCPClosed || entry.lastResult != ResultTimeout {
		t.Fatalf("state=%d result=%d", entry.state, entry.lastResult)
	}
	var buffer [4]byte
	if _, result := h.stack.TCPRead(0, id, buffer[:], h.now); result != ResultTimeout {
		t.Fatalf("read result %d", result)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("closed connection kept a timer")
	}
}

func TestTCPRetransmitProgressResetsLimit(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50014)
	h.stack.TCPWrite(0, id, []byte("x"), h.now)
	for range 3 {
		h.advance(entry.rtoDeadline - h.now)
	}
	if entry.retries != 3 {
		t.Fatalf("retries %d", entry.retries)
	}
	h.tcpFromPeer(entry, ackSegment(entry, entry.sndNxt, 1000))
	if entry.retries != 0 || entry.sndUna != entry.sndNxt {
		t.Fatalf("after ACK retries=%d una=%d nxt=%d", entry.retries, entry.sndUna, entry.sndNxt)
	}
}

func TestTCPLastAckRetransmitLimitReleasesOrphan(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50015)
	h.tcpFromPeer(entry, tcpSegment{sequence: 200, acknowledgment: 100, flags: tcpFlagACK | tcpFlagFIN, window: 1000})
	if entry.state != TCPCloseWait {
		t.Fatalf("state %d", entry.state)
	}
	h.stack.TCPClose(0, id, h.now)
	if entry.state != TCPLastAck {
		t.Fatalf("state %d", entry.state)
	}
	h.drain()
	h.runUntil(h.now + 30*60*second)
	if stats := h.stack.TCPStats(); stats.Endpoints != 0 {
		t.Fatalf("LAST-ACK without a peer kept %d endpoints", stats.Endpoints)
	}
	if _, ok := h.stack.Deadline(); ok {
		t.Fatal("released connection left a timer")
	}
}

// runWithSilentPeer runs timers until limit. The peer answers ARP but never
// TCP. It returns the TCP segments sent.
func (h *harness) runWithSilentPeer(limit uint64) []tcpSegment {
	h.t.Helper()
	var sent []tcpSegment
	for {
		for _, frame := range h.drain() {
			ether, _ := parseEthernet(frame)
			if ether.etherType == etherTypeARP {
				if request, status := parseARP(ether.payload); status == parseOK && request.operation == arpRequest && request.targetIP == peerIP {
					h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: peerIP,
						targetMAC: hostMAC, targetIP: hostIP}))
				}
				continue
			}
			if packet, status := parseIPv4(ether.payload); status == parseOK && packet.protocol == protocolTCP {
				if segment, tcpStatus := parseTCP(packet); tcpStatus == tcpOK {
					sent = append(sent, segment)
				}
			}
		}
		deadline, ok := h.stack.Deadline()
		if !ok || deadline > limit {
			return sent
		}
		h.now = deadline
		h.stack.Advance(h.now)
	}
}

// passiveOpen completes a handshake from peer to a listener.
func (h *harness) passiveOpen(peer *tcpEndpoint, iss uint32) {
	h.t.Helper()
	h.tcpFromPeer(peer, tcpSegment{sequence: iss, flags: tcpFlagSYN, window: 65535, mss: 1460})
	synACK := h.tcpSent()
	if len(synACK) != 1 || synACK[0].flags != tcpFlagSYN|tcpFlagACK {
		h.t.Fatalf("SYN-ACK %+v", synACK)
	}
	h.tcpFromPeer(peer, tcpSegment{sequence: iss + 1, acknowledgment: synACK[0].sequence + 1, flags: tcpFlagACK, window: 65535})
}

func TestTCPUnacceptedChildReleasedOnRenumber(t *testing.T) {
	h := readyWithPeer(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, IPv4{}, 8095)
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	h.passiveOpen(&tcpEndpoint{port: 8095, remotePort: 40001}, 7000)
	if children := h.stack.listenerChildren(listener); children != 1 {
		t.Fatalf("children %d", children)
	}
	renumbered := IPv4{10, 0, 2, 16}
	if field := h.stack.Configure(Config{Mode: ModeStatic, Address: renumbered, Prefix: 24, Gateway: gatewayIP}, h.now); field != FieldNone {
		t.Fatalf("configure field %d", field)
	}
	if children := h.stack.listenerChildren(listener); children != 0 {
		t.Fatalf("children after renumber %d", children)
	}
	if state, _, result := h.stack.TCPState(0, listener); result != ResultOK || state != TCPListen {
		t.Fatalf("listener state=%d result=%d", state, result)
	}
	if stats := h.stack.TCPStats(); stats.Endpoints != 1 {
		t.Fatalf("endpoints %d states %v", stats.Endpoints, stats.States)
	}
	h.claim()
	h.finishAnnouncements()
	h.drain()
	h.learn(peerIP, peerMAC)
	h.passiveOpen(&tcpEndpoint{port: 8095, remotePort: 40002}, 9000)
	child, tuple, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK || child == 0 || tuple.Local != renumbered || tuple.RemotePort != 40002 {
		t.Fatalf("accept %d %+v", result, tuple)
	}
}

func TestTCPUnacceptedChildReleasedOnResetAndListenerClose(t *testing.T) {
	h := readyWithPeer(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, IPv4{}, 8096)
	h.stack.TCPListen(0, listener, 2)
	peer := tcpEndpoint{port: 8096, remotePort: 40003}
	h.passiveOpen(&peer, 7000)
	h.tcpFromPeer(&peer, tcpSegment{sequence: 7001, flags: tcpFlagRST})
	if children := h.stack.listenerChildren(listener); children != 0 {
		t.Fatalf("reset child kept, children %d", children)
	}
	h.passiveOpen(&tcpEndpoint{port: 8096, remotePort: 40004}, 8000)
	h.stack.TCPClose(0, listener, h.now)
	if stats := h.stack.TCPStats(); stats.Endpoints != 0 {
		t.Fatalf("listener close kept %d endpoints", stats.Endpoints)
	}
	if sent := h.tcpSent(); len(sent) != 1 || sent[0].flags&tcpFlagRST == 0 {
		t.Fatalf("child reset %+v", sent)
	}
}

// forgetPeer drops every neighbour so the next segment waits for ARP.
func (h *harness) forgetPeer() {
	h.stack.clearNeighbors()
}

func (h *harness) answerARP() {
	h.t.Helper()
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: peerIP,
		targetMAC: hostMAC, targetIP: hostIP}))
}

func TestTCPBufferedDataWaitsForARP(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50020)
	h.forgetPeer()
	if written, result := h.stack.TCPWrite(0, id, []byte("data"), h.now); result != ResultOK || written != 4 {
		t.Fatalf("write %d %d", written, result)
	}
	if sent := h.tcpSent(); len(sent) != 0 || !entry.l2Pending || entry.sendSent != 0 || entry.rtoDeadline == 0 {
		t.Fatalf("sent=%d pending=%v sendSent=%d rto=%d", len(sent), entry.l2Pending, entry.sendSent, entry.rtoDeadline)
	}
	h.answerARP()
	sent := h.tcpSent()
	if len(sent) != 1 || string(sent[0].payload) != "data" || sent[0].sequence != 100 || entry.l2Pending {
		t.Fatalf("after ARP %+v pending=%v", sent, entry.l2Pending)
	}
}

func TestTCPBufferedDataFailsWithNeighbor(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50021)
	_, idle := establishedWithPeer(t, h, 50022)
	h.forgetPeer()
	h.stack.TCPWrite(0, id, []byte("data"), h.now)
	h.runUntil(h.now + 30*second)
	if entry.state != TCPClosed || entry.lastResult != ResultNeighborUnreachable || entry.rtoDeadline != 0 {
		t.Fatalf("state=%d result=%d rto=%d", entry.state, entry.lastResult, entry.rtoDeadline)
	}
	if _, result := h.stack.TCPWrite(0, id, []byte("x"), h.now); result != ResultNeighborUnreachable {
		t.Fatalf("later write %d", result)
	}
	if idle.state != TCPEstablished {
		t.Fatalf("idle connection state %d", idle.state)
	}
}

func TestTCPFINWaitsForARP(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50023)
	h.forgetPeer()
	h.stack.TCPShutdownWrite(0, id, h.now)
	if sent := h.tcpSent(); len(sent) != 0 || entry.state != TCPFinWait1 || !entry.l2Pending || entry.rtoDeadline == 0 {
		t.Fatalf("sent=%d state=%d pending=%v rto=%d", len(sent), entry.state, entry.l2Pending, entry.rtoDeadline)
	}
	h.answerARP()
	sent := h.tcpSent()
	if len(sent) != 1 || sent[0].flags&tcpFlagFIN == 0 || sent[0].sequence != 100 {
		t.Fatalf("after ARP %+v", sent)
	}
}

func TestTCPFINFailsWithNeighbor(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50024)
	orphanID, orphan := establishedWithPeer(t, h, 50025)
	h.tcpFromPeer(orphan, tcpSegment{sequence: 200, acknowledgment: 100, flags: tcpFlagACK | tcpFlagFIN, window: 1000})
	h.drain()
	h.forgetPeer()
	h.stack.TCPShutdownWrite(0, id, h.now)
	h.stack.TCPClose(0, orphanID, h.now)
	if entry.state != TCPFinWait1 || !entry.l2Pending || orphan.state != TCPLastAck || !orphan.l2Pending {
		t.Fatalf("states %d/%d pending %v/%v", entry.state, orphan.state, entry.l2Pending, orphan.l2Pending)
	}
	if sent := h.tcpSent(); len(sent) != 0 {
		t.Fatalf("FIN sent without a neighbour: %+v", sent)
	}
	h.runUntil(h.now + 30*second)
	if entry.state != TCPClosed || entry.lastResult != ResultNeighborUnreachable {
		t.Fatalf("FIN-WAIT-1 state=%d result=%d", entry.state, entry.lastResult)
	}
	if stats := h.stack.TCPStats(); stats.Endpoints != 1 {
		t.Fatalf("LAST-ACK orphan kept, endpoints %d", stats.Endpoints)
	}
}

func receivedStream(entry *tcpEndpoint) string { return string(entry.receive[:entry.recvLen]) }

func TestTCPFINAtReceiveWindowRightEdge(t *testing.T) {
	for _, start := range []uint32{100, 0xfffffffe} {
		h := readyWithPeer(t)
		h.stack.tuning.TCP.ReceiveBuffer = 4
		id, entry := establishedWithPeer(t, h, 50100)
		entry.rcvNxt = start
		end := start + 4
		h.tcpFromPeer(entry, tcpSegment{sequence: start, acknowledgment: entry.sndNxt,
			flags: tcpFlagACK | tcpFlagFIN, payload: []byte("abcd")})
		if entry.rcvNxt != end || receivedStream(entry) != "abcd" || entry.peerFIN || entry.finReceived || entry.state != TCPEstablished {
			t.Fatalf("start=%d: rcvNxt=%d data=%q peerFIN=%v fin=%v state=%d",
				start, entry.rcvNxt, receivedStream(entry), entry.peerFIN, entry.finReceived, entry.state)
		}
		if sent := h.tcpSent(); len(sent) != 1 || sent[0].acknowledgment != end {
			t.Fatalf("start=%d: ACKs %+v", start, sent)
		}
		var buffer [4]byte
		if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); n != 4 || result != ResultOK || string(buffer[:]) != "abcd" {
			t.Fatalf("start=%d: read %d %d %q", start, n, result, buffer[:])
		}
		if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); n != 0 || result != ResultBusy {
			t.Fatalf("start=%d: premature EOF %d %d", start, n, result)
		}
		h.tcpSent()
		h.tcpFromPeer(entry, tcpSegment{sequence: end, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN})
		if !entry.peerFIN || entry.peerFINAt != end || !entry.finReceived || entry.rcvNxt != end+1 || entry.state != TCPCloseWait {
			t.Fatalf("start=%d: FIN at=%d known=%v received=%v rcvNxt=%d state=%d",
				start, entry.peerFINAt, entry.peerFIN, entry.finReceived, entry.rcvNxt, entry.state)
		}
		if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); n != 0 || result != ResultEOF {
			t.Fatalf("start=%d: EOF %d %d", start, n, result)
		}
		h.tcpFromPeer(entry, tcpSegment{sequence: end, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN})
		if entry.rcvNxt != end+1 {
			t.Fatalf("start=%d: duplicate FIN advanced to %d", start, entry.rcvNxt)
		}
	}
}

func TestTCPFINInsideReceiveWindow(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.ReceiveBuffer = 5
	id, entry := establishedWithPeer(t, h, 50101)
	entry.rcvNxt = 100
	h.tcpFromPeer(entry, tcpSegment{sequence: 100, acknowledgment: entry.sndNxt,
		flags: tcpFlagACK | tcpFlagFIN, payload: []byte("abcd")})
	if !entry.peerFIN || entry.peerFINAt != 104 || !entry.finReceived || entry.rcvNxt != 105 || entry.state != TCPCloseWait {
		t.Fatalf("FIN at=%d known=%v received=%v rcvNxt=%d state=%d",
			entry.peerFINAt, entry.peerFIN, entry.finReceived, entry.rcvNxt, entry.state)
	}
	var buffer [4]byte
	if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); n != 4 || result != ResultOK || string(buffer[:]) != "abcd" {
		t.Fatalf("read %d %d %q", n, result, buffer[:])
	}
	if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); n != 0 || result != ResultEOF {
		t.Fatalf("EOF %d %d", n, result)
	}
}

func TestTCPFINBeyondClippedPayload(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.ReceiveBuffer = 4
	_, entry := establishedWithPeer(t, h, 50102)
	entry.rcvNxt = 100
	h.tcpFromPeer(entry, tcpSegment{sequence: 100, acknowledgment: entry.sndNxt,
		flags: tcpFlagACK | tcpFlagFIN, payload: []byte("abcdef")})
	if entry.rcvNxt != 104 || receivedStream(entry) != "abcd" || entry.peerFIN || entry.finReceived {
		t.Fatalf("rcvNxt=%d data=%q peerFIN=%v fin=%v", entry.rcvNxt, receivedStream(entry), entry.peerFIN, entry.finReceived)
	}
	for index := range entry.reorder {
		if entry.reorder[index].used {
			t.Fatalf("out-of-window bytes held in slot %d", index)
		}
	}
	if sent := h.tcpSent(); len(sent) != 1 || sent[0].acknowledgment != 104 {
		t.Fatalf("ACKs %+v", sent)
	}
}

func TestTCPFINWithZeroReceiveWindow(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.ReceiveBuffer = 4
	_, entry := establishedWithPeer(t, h, 50103)
	entry.rcvNxt = 100
	entry.recvLen = 4
	copy(entry.receive[:], "abcd")
	fin := tcpSegment{sequence: 100, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN}
	h.tcpFromPeer(entry, fin)
	if entry.rcvNxt != 100 || entry.peerFIN || entry.finReceived || entry.state != TCPEstablished {
		t.Fatalf("zero window: rcvNxt=%d peerFIN=%v fin=%v state=%d", entry.rcvNxt, entry.peerFIN, entry.finReceived, entry.state)
	}
	if sent := h.tcpSent(); len(sent) != 1 || sent[0].acknowledgment != 100 {
		t.Fatalf("ACKs %+v", sent)
	}
	if !h.stack.tcpSegmentAcceptable(entry, tcpSegment{sequence: 100, flags: tcpFlagACK}) {
		t.Fatal("zero-window exact ACK rejected")
	}
}

func TestTCPNoDataAfterFIN(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	entry := tcpEndpoint{state: TCPEstablished, rcvNxt: 100, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("abc")}, second)
	if !entry.finReceived || entry.rcvNxt != 104 || entry.state != TCPCloseWait || receivedStream(&entry) != "abc" {
		t.Fatalf("FIN fin=%v rcvNxt=%d state=%d data=%q", entry.finReceived, entry.rcvNxt, entry.state, receivedStream(&entry))
	}
	for _, segment := range []tcpSegment{
		{sequence: 104, flags: tcpFlagACK, payload: []byte("xyz")},
		{sequence: 103, flags: tcpFlagACK | tcpFlagFIN},
		{sequence: 100, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("abc")},
		{sequence: 101, flags: tcpFlagACK, payload: []byte("bcdefg")},
		{sequence: 106, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("late")},
	} {
		stack.handlePayload(&entry, segment, second)
		if entry.rcvNxt != 104 || receivedStream(&entry) != "abc" {
			t.Fatalf("segment %+v: rcvNxt=%d data=%q", segment, entry.rcvNxt, receivedStream(&entry))
		}
	}
	var buffer [16]byte
	stack.tcp[0] = entry
	stack.tcp[0].generation, stack.tcp[0].owner = 1, 0
	id := tcpID(&stack.tcp[0], 0)
	if read, result := stack.TCPRead(0, id, buffer[:], second); result != ResultOK || string(buffer[:read]) != "abc" {
		t.Fatalf("read %d %d", read, result)
	}
	if read, result := stack.TCPRead(0, id, buffer[:], second); result != ResultEOF || read != 0 {
		t.Fatalf("EOF read %d %d", read, result)
	}
}

func TestTCPOutOfOrderFINKeepsItsSequence(t *testing.T) {
	stack := Stack{tuning: DefaultTuning()}
	// A later same-sequence segment cannot move a held FIN.
	entry := tcpEndpoint{state: TCPEstablished, rcvNxt: 100, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 103, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("d")}, second)
	stack.handlePayload(&entry, tcpSegment{sequence: 103, flags: tcpFlagACK, payload: []byte("defg")}, second)
	stack.handlePayload(&entry, tcpSegment{sequence: 106, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("gh")}, second)
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK, payload: []byte("abc")}, second)
	if !entry.finReceived || entry.rcvNxt != 105 || receivedStream(&entry) != "abcd" {
		t.Fatalf("held FIN fin=%v rcvNxt=%d data=%q", entry.finReceived, entry.rcvNxt, receivedStream(&entry))
	}

	// An in-order segment that overlaps the FIN stops at it.
	entry = tcpEndpoint{state: TCPEstablished, rcvNxt: 100, sndNxt: 50}
	stack.handlePayload(&entry, tcpSegment{sequence: 102, flags: tcpFlagACK | tcpFlagFIN, payload: []byte("cd")}, second)
	if entry.finReceived || !entry.peerFIN || entry.peerFINAt != 104 {
		t.Fatalf("out-of-order FIN fin=%v known=%v at=%d", entry.finReceived, entry.peerFIN, entry.peerFINAt)
	}
	stack.handlePayload(&entry, tcpSegment{sequence: 100, flags: tcpFlagACK, payload: []byte("abcdefgh")}, second)
	if !entry.finReceived || entry.rcvNxt != 105 || receivedStream(&entry) != "abcd" || entry.state != TCPCloseWait {
		t.Fatalf("overlap fin=%v rcvNxt=%d data=%q state=%d", entry.finReceived, entry.rcvNxt, receivedStream(&entry), entry.state)
	}
	for index := range entry.reorder {
		if entry.reorder[index].used {
			t.Fatalf("reassembly slot %d still held after FIN", index)
		}
	}
}

// A client that closes first leaves an orphan in TIME_WAIT on its own count.
// Opening past the session limit reuses the oldest of them, never another
// owner's, and never an endpoint the owner still holds.
func TestTCPOpenReusesOwnTimeWait(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8092); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	timeWait := func(owner int) int {
		count := 0
		for index := range h.stack.tcp {
			entry := &h.stack.tcp[index]
			if entry.state == TCPTimeWait && int(entry.owner) == owner {
				count++
			}
		}
		return count
	}
	var buffer [8]byte
	cycle := func(owner int, round int) {
		t.Helper()
		client, result := h.stack.TCPOpen(owner)
		if result != ResultOK {
			t.Fatalf("owner %d round %d open %d", owner, round, result)
		}
		if _, result = h.stack.TCPConnect(owner, client, LoopbackAddress, 8092, h.now); result != ResultOK {
			t.Fatalf("owner %d round %d connect %d", owner, round, result)
		}
		server, _, result := h.stack.TCPAccept(0, listener)
		if result != ResultOK {
			t.Fatalf("owner %d round %d accept %d", owner, round, result)
		}
		h.stack.TCPClose(owner, client, h.now)
		if _, result = h.stack.TCPRead(0, server, buffer[:], h.now); result != ResultEOF {
			t.Fatalf("owner %d round %d EOF %d", owner, round, result)
		}
		h.stack.TCPClose(0, server, h.now)
		h.now++
	}
	for round := range 3 * MaxTCPEndpointsPerSession {
		cycle(1, round)
		if got := timeWait(1); got != min(round+1, MaxTCPEndpointsPerSession) {
			t.Fatalf("round %d: %d entries in TIME_WAIT", round, got)
		}
	}
	// Endpoints owner 2 still holds are never reused for it.
	var held []TCPEndpointID
	for range MaxTCPEndpointsPerSession {
		id, result := h.stack.TCPOpen(2)
		if result != ResultOK {
			t.Fatalf("owner 2 open %d", result)
		}
		held = append(held, id)
	}
	if _, result := h.stack.TCPOpen(2); result != ResultBusy {
		t.Fatalf("owner 2 past its limit: %d", result)
	}
	if got := timeWait(1); got != MaxTCPEndpointsPerSession {
		t.Fatalf("owner 2 took owner 1's TIME_WAIT: %d left", got)
	}
	for _, id := range held {
		if state, _, result := h.stack.TCPState(2, id); result != ResultOK || state != TCPOpen {
			t.Fatalf("held endpoint %d state %d result %d", id, state, result)
		}
	}
}
