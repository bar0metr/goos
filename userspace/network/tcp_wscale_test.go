package network

import (
	"bytes"
	"testing"
)

func TestTCPWireWindowScaleOption(t *testing.T) {
	source, destination := IPv4{192, 0, 2, 1}, IPv4{198, 51, 100, 2}
	var bytes [64]byte
	for _, test := range []struct {
		segment tcpSegment
		length  int
		ok      bool
		shift   uint8
	}{
		{tcpSegment{flags: tcpFlagSYN, mss: 1460, wscaleOK: true, wscale: 7}, 28, true, 7},
		{tcpSegment{flags: tcpFlagSYN, mss: 1460, wscaleOK: true, wscale: 20}, 28, true, tcpMaxWindowScale},
		{tcpSegment{flags: tcpFlagSYN, mss: 1460}, 24, false, 0},
		// Only a SYN carries options.
		{tcpSegment{flags: tcpFlagACK, wscaleOK: true, wscale: 7}, 20, false, 0},
	} {
		test.segment.sourcePort, test.segment.destinationPort = 1234, 80
		length := putTCP(bytes[:], source, destination, test.segment)
		if length != test.length {
			t.Fatalf("%+v: length %d want %d", test.segment, length, test.length)
		}
		parsed, status := parseTCP(ipv4Packet{source: source, destination: destination, protocol: protocolTCP, payload: bytes[:length]})
		if status != tcpOK || parsed.wscaleOK != test.ok || parsed.wscale != test.shift {
			t.Fatalf("%+v: parsed %+v status %d", test.segment, parsed, status)
		}
	}
	// A window scale option whose length is not 3 is malformed.
	length := putTCP(bytes[:], source, destination, tcpSegment{sourcePort: 1, destinationPort: 2, flags: tcpFlagSYN, mss: 1460, wscaleOK: true, wscale: 2})
	bytes[26] = 4
	bytes[16], bytes[17] = 0, 0
	sum := checksumFold(checksumAdd(pseudoHeaderSum(source, destination, protocolTCP, length), bytes[:length]))
	bytes[16], bytes[17] = byte(sum>>8), byte(sum)
	if _, status := parseTCP(ipv4Packet{source: source, destination: destination, protocol: protocolTCP, payload: bytes[:length]}); status != tcpMalformed {
		t.Fatalf("bad length status %d", status)
	}
}

func TestTCPWindowScaleShiftFollowsBuffer(t *testing.T) {
	for _, test := range []struct {
		buffer uint32
		shift  uint8
	}{{4, 0}, {65535, 0}, {65536, 1}, {DefaultTCPReceiveBuffer, 3}, {MaxTCPBuffer, 7}} {
		h := newHarness(t)
		h.stack.tuning.TCP.ReceiveBuffer = test.buffer
		if shift := h.stack.tcpWindowScale(); shift != test.shift {
			t.Fatalf("buffer %d shift %d want %d", test.buffer, shift, test.shift)
		}
	}
}

// activeScaled opens a connection to the peer and answers its SYN with a
// SYN-ACK that offers peerShift when peerOffers.
func activeScaled(t *testing.T, h *harness, buffer uint32, peerOffers bool, peerShift uint8) (TCPEndpointID, *tcpEndpoint, tcpSegment) {
	t.Helper()
	h.stack.tuning.TCP.ReceiveBuffer = buffer
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
		window: 65535, mss: 1460, wscaleOK: peerOffers, wscale: peerShift})
	if entry.state != TCPEstablished {
		t.Fatalf("state %d", entry.state)
	}
	h.tcpSent()
	return id, entry, syn[0]
}

func TestTCPWindowScaleNegotiation(t *testing.T) {
	for _, test := range []struct {
		name               string
		buffer             uint32
		peerOffers         bool
		synOffers          bool
		rcvScale, sndScale uint8
	}{
		{"both", DefaultTCPReceiveBuffer, true, true, 3, 5},
		{"peer only", 32 << 10, true, false, 0, 0},
		{"local only", DefaultTCPReceiveBuffer, false, true, 0, 0},
		{"none", 32 << 10, false, false, 0, 0},
	} {
		t.Run("active "+test.name, func(t *testing.T) {
			h := readyWithPeer(t)
			_, entry, syn := activeScaled(t, h, test.buffer, test.peerOffers, 5)
			if syn.wscaleOK != test.synOffers || (syn.wscaleOK && syn.wscale != 3) {
				t.Fatalf("SYN option %v shift %d", syn.wscaleOK, syn.wscale)
			}
			if entry.rcvScale != test.rcvScale || entry.sndScale != test.sndScale {
				t.Fatalf("scales rcv %d snd %d", entry.rcvScale, entry.sndScale)
			}
			// The SYN-ACK's window is never scaled; later windows are.
			if entry.peerWindow != 65535 {
				t.Fatalf("SYN-ACK window %d", entry.peerWindow)
			}
			h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 1000})
			if want := uint32(1000) << test.sndScale; entry.peerWindow != want {
				t.Fatalf("peer window %d want %d", entry.peerWindow, want)
			}
			h.stack.TCPWrite(0, tcpID(entry), []byte("x"), h.now)
			sent := h.tcpSent()
			if len(sent) != 1 {
				t.Fatalf("data frames %d", len(sent))
			}
			if want := uint16(min(h.stack.tcpAdvertisedWindow(entry)>>test.rcvScale, 65535)); sent[0].window != want {
				t.Fatalf("advertised %d want %d", sent[0].window, want)
			}
		})
		t.Run("passive "+test.name, func(t *testing.T) {
			h := readyWithPeer(t)
			h.stack.tuning.TCP.ReceiveBuffer = test.buffer
			listener, _ := h.stack.TCPOpen(0)
			h.stack.TCPBind(0, listener, IPv4{}, 8090)
			h.stack.TCPListen(0, listener, 1)
			peer := tcpEndpoint{port: 8090, remotePort: 40000}
			h.tcpFromPeer(&peer, tcpSegment{sequence: 9000, flags: tcpFlagSYN, window: 65535, mss: 1460, wscaleOK: test.peerOffers, wscale: 5})
			synACK := h.tcpSent()
			if len(synACK) != 1 || synACK[0].flags != tcpFlagSYN|tcpFlagACK {
				t.Fatalf("SYN-ACK %+v", synACK)
			}
			both := test.synOffers && test.peerOffers
			if synACK[0].wscaleOK != both || (both && synACK[0].wscale != 3) {
				t.Fatalf("SYN-ACK option %v shift %d", synACK[0].wscaleOK, synACK[0].wscale)
			}
			if want := uint16(min(h.stack.tcpWindowScaleTestFree(), 65535)); synACK[0].window != want {
				t.Fatalf("SYN-ACK window %d want %d", synACK[0].window, want)
			}
			h.tcpFromPeer(&peer, tcpSegment{sequence: 9001, acknowledgment: synACK[0].sequence + 1, flags: tcpFlagACK, window: 1000})
			child, _, result := h.stack.TCPAccept(0, listener)
			if result != ResultOK {
				t.Fatal(result)
			}
			entry, _ := h.stack.tcpEndpoint(0, child)
			var rcv, snd uint8
			if both {
				rcv, snd = 3, 5
			}
			if entry.rcvScale != rcv || entry.sndScale != snd || entry.peerWindow != uint32(1000)<<snd {
				t.Fatalf("scales rcv %d snd %d window %d", entry.rcvScale, entry.sndScale, entry.peerWindow)
			}
		})
	}
}

// tcpWindowScaleTestFree is the window a new child's SYN-ACK offers: its
// first receive ring.
func (stack *Stack) tcpWindowScaleTestFree() int {
	return min(tcpBufferFloor, int(stack.tuning.TCP.ReceiveBuffer))
}

// TestTCPScaledWindowExceedsUnscaledLimit fills a scaled connection
// without reading until its ring reaches the tuned buffer, then reads it
// empty: the update must offer the whole 256 KiB, which an unscaled field
// cannot carry.
func TestTCPScaledWindowExceedsUnscaledLimit(t *testing.T) {
	h := readyWithPeer(t)
	id, entry, _ := activeScaled(t, h, DefaultTCPReceiveBuffer, true, 0)
	payload := bytes.Repeat([]byte("w"), 1460)
	for {
		room := int(entry.rcvEdge - entry.rcvNxt)
		if room <= 0 {
			break
		}
		h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 65535,
			payload: payload[:min(room, len(payload))]})
		for _, segment := range h.tcpSent() {
			if segment.window != 0 && uint32(segment.window)<<3 > uint32(len(entry.recvRing)) {
				t.Fatalf("window %d beyond ring %d", segment.window, len(entry.recvRing))
			}
		}
	}
	if len(entry.recvRing) != DefaultTCPReceiveBuffer {
		t.Fatalf("ring %d", len(entry.recvRing))
	}
	// A small read moves the edge less than a segment: no update.
	small := make([]byte, 100)
	if n, result := h.stack.TCPRead(0, id, small, h.now); n != 100 || result != ResultOK {
		t.Fatalf("read %d %d", n, result)
	}
	if sent := h.tcpSent(); len(sent) != 0 {
		t.Fatalf("update after a small read: %+v", sent)
	}
	buffer := make([]byte, DefaultTCPReceiveBuffer)
	for {
		n, result := h.stack.TCPRead(0, id, buffer, h.now)
		if result != ResultOK || n == 0 {
			break
		}
	}
	sent := h.tcpSent()
	if len(sent) == 0 {
		t.Fatal("no window update")
	}
	last := sent[len(sent)-1]
	if bytes := uint32(last.window) << 3; bytes <= 65535 || bytes != DefaultTCPReceiveBuffer || entry.rcvEdge != entry.rcvNxt+bytes {
		t.Fatalf("update window field %d edge %d", last.window, entry.rcvEdge-entry.rcvNxt)
	}
}

// TestTCPLoopbackScaledWindow checks that two local endpoints negotiate a
// shift and that the sender sees a window past 64 KiB.
func TestTCPLoopbackScaledWindow(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, LoopbackAddress, 8080)
	h.stack.TCPListen(0, listener, 1)
	client, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 8080, h.now); result != ResultOK {
		t.Fatal(result)
	}
	server, _, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK {
		t.Fatal(result)
	}
	sender, _ := h.stack.tcpEndpoint(1, client)
	receiver, _ := h.stack.tcpEndpoint(0, server)
	if sender.rcvScale != 3 || sender.sndScale != 3 || receiver.rcvScale != 3 || receiver.sndScale != 3 {
		t.Fatalf("scales %d/%d %d/%d", sender.rcvScale, sender.sndScale, receiver.rcvScale, receiver.sndScale)
	}
	data := bytes.Repeat([]byte("l"), 200<<10)
	for written := 0; written < len(data); {
		n, result := h.stack.TCPWrite(1, client, data[written:], h.now)
		if result != ResultOK {
			break
		}
		written += n
	}
	if sender.peerWindow == 0 && len(receiver.recvRing) < DefaultTCPReceiveBuffer {
		t.Fatalf("receiver stalled at ring %d", len(receiver.recvRing))
	}
	buffer := make([]byte, len(data))
	for {
		n, result := h.stack.TCPRead(0, server, buffer, h.now)
		if result != ResultOK || n == 0 {
			break
		}
	}
	if sender.peerWindow <= 65535 {
		t.Fatalf("peer window %d after the reader drained %d-byte ring", sender.peerWindow, len(receiver.recvRing))
	}
}

// TestTCPPersistWithScaledZeroWindow closes a scaled peer window, probes it,
// and reopens it by one unit of the shift.
func TestTCPPersistWithScaledZeroWindow(t *testing.T) {
	h := readyWithPeer(t)
	id, entry, _ := activeScaled(t, h, DefaultTCPReceiveBuffer, true, 7)
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 0})
	if entry.peerWindow != 0 {
		t.Fatalf("peer window %d", entry.peerWindow)
	}
	data := bytes.Repeat([]byte("p"), 300)
	if n, result := h.stack.TCPWrite(0, id, data, h.now); n != len(data) || result != ResultOK {
		t.Fatalf("write %d %d", n, result)
	}
	if sent := h.tcpSent(); len(sent) != 0 {
		t.Fatalf("sent into a zero window: %+v", sent)
	}
	if entry.rtoDeadline == 0 {
		t.Fatal("persist timer not armed")
	}
	h.advance(entry.rtoDeadline - h.now)
	probe := h.tcpSent()
	if len(probe) != 1 || len(probe[0].payload) != 1 || entry.persist != 1 {
		t.Fatalf("probe %+v persist %d", probe, entry.persist)
	}
	// Window field 1 is 128 bytes after a shift of 7.
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 1})
	sent := h.tcpSent()
	total := 0
	for _, segment := range sent {
		total += len(segment.payload)
	}
	if entry.peerWindow != 128 || entry.persist != 0 || total != 128 {
		t.Fatalf("reopen window %d persist %d sent %d", entry.peerWindow, entry.persist, total)
	}
}

// TestTCPScaledReceiveWindowRoundsToZero checks the receive side: free
// space below one unit of the shift advertises a zero window, and reading
// reopens it.
func TestTCPScaledReceiveWindowRoundsToZero(t *testing.T) {
	h := readyWithPeer(t)
	id, entry, _ := activeScaled(t, h, DefaultTCPReceiveBuffer, true, 0)
	entry.rcvScale = 14
	// A pure ACK now offers free>>14, which is zero for a 4 KiB ring.
	h.stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, h.now)
	sent := h.tcpSent()
	if len(sent) != 1 || sent[0].window != 0 || entry.rcvEdge != entry.rcvNxt {
		t.Fatalf("ACK %+v edge %d", sent, entry.rcvEdge-entry.rcvNxt)
	}
	// Bytes the real buffer holds are still accepted past the rounded edge.
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 1, payload: []byte("abc")})
	h.tcpSent()
	buffer := make([]byte, 8)
	if n, result := h.stack.TCPRead(0, id, buffer, h.now); n != 3 || result != ResultOK || string(buffer[:3]) != "abc" {
		t.Fatalf("read %d %d", n, result)
	}
}
