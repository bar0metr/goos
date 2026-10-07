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
	h.stack.handleACK(clientEntry, clientEntry.sndUna, 4096, true, h.now)
	if read, result := h.stack.TCPRead(0, server, buffer[:], h.now); result != ResultOK || string(buffer[:read]) != "resume" {
		t.Fatalf("read after window update %d %d %q", read, result, buffer[:read])
	}
}

func TestTCPInvalidACKDoesNotChangeWindow(t *testing.T) {
	entry := tcpEndpoint{state: TCPEstablished, sndUna: 10, sndNxt: 20, peerWindow: 30}
	stack := Stack{tuning: DefaultTuning()}
	stack.handleACK(&entry, 21, 4000, true, second)
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
	entry.rcvNxt, entry.peerWindow = 200, 0
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
	h.stack.handleACK(entry, entry.sndUna, 4096, true, h.now)
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
		h.stack.handleACK(entry, entry.sndUna, uint16(entry.peerWindow), true, h.now)
	}
	if !entry.fastRecover || entry.recover != 1400 || entry.dupACKs != 3 {
		t.Fatalf("fast recovery=%v recover=%d dupacks=%d", entry.fastRecover, entry.recover, entry.dupACKs)
	}
	if frames := h.drain(); len(frames) != 1 {
		t.Fatalf("fast retransmit frames %d", len(frames))
	}
	cwnd := entry.cwnd
	h.stack.handleACK(entry, entry.sndUna, uint16(entry.peerWindow), true, h.now)
	if entry.cwnd != cwnd+100 {
		t.Fatalf("recovery cwnd %d -> %d", cwnd, entry.cwnd)
	}
	h.stack.handleACK(entry, entry.recover, uint16(entry.peerWindow), true, h.now)
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
