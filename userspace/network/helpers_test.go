package network

import (
	"encoding/binary"
	"testing"
)

var (
	hostMAC    = MAC{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	gatewayMAC = MAC{0x52, 0x55, 0x0a, 0x00, 0x02, 0x02}
	peerMAC    = MAC{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc}
	otherMAC   = MAC{0x52, 0x54, 0x00, 0x01, 0x02, 0x03}

	hostIP    = IPv4{10, 0, 2, 15}
	gatewayIP = IPv4{10, 0, 2, 2}
	peerIP    = IPv4{10, 0, 2, 20}
	remoteIP  = IPv4{192, 0, 2, 99}

	staticConfig = Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: gatewayIP}
)

type harness struct {
	t     testing.TB
	stack *Stack
	now   uint64
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	h := &harness{t: t, stack: new(Stack), now: 1_000 * second}
	h.stack.Init(0x1234_5678_9abc_def0)
	return h
}

// advance moves time forward, runs due timers and checks that no timer is
// left at or before the new time.
func (h *harness) advance(delay uint64) {
	h.t.Helper()
	h.now += delay
	h.stack.Advance(h.now)
	h.checkDeadline()
}

func (h *harness) checkDeadline() {
	h.t.Helper()
	if deadline, ok := h.stack.Deadline(); ok && deadline <= h.now {
		h.t.Fatalf("deadline %d not after now %d", deadline, h.now)
	}
}

// runUntil advances to each pending deadline until the limit, so tests can
// skip idle time without missing a timer.
func (h *harness) runUntil(limit uint64) {
	h.t.Helper()
	for step := 0; step < 10_000; step++ {
		deadline, ok := h.stack.Deadline()
		if !ok || deadline > limit {
			break
		}
		h.now = deadline
		h.stack.Advance(h.now)
		h.checkDeadline()
	}
	if h.now < limit {
		h.now = limit
		h.stack.Advance(h.now)
		h.checkDeadline()
	}
}

func (h *harness) receive(frame []byte) {
	h.t.Helper()
	h.stack.Receive(frame, h.now)
	h.checkDeadline()
}

// drain takes every queued frame.
func (h *harness) drain() [][]byte {
	var frames [][]byte
	var buffer [MaxFrame]byte
	for {
		length, ok := h.stack.NextFrame(buffer[:])
		if !ok {
			return frames
		}
		frames = append(frames, append([]byte(nil), buffer[:length]...))
	}
}

func (h *harness) readyStatic(config Config) {
	h.t.Helper()
	if field := h.stack.Configure(config, h.now); field != FieldNone {
		h.t.Fatalf("configure: field %d", field)
	}
	h.stack.LinkUp(hostMAC, MTU, h.now)
	h.runUntil(h.now + 2*second)
	if state := h.stack.Snapshot(h.now).State; state != StateReady {
		h.t.Fatalf("state %d, want ready", state)
	}
	h.drain()
}

func (h *harness) learn(address IPv4, mac MAC) {
	h.t.Helper()
	h.receive(arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: mac, senderIP: address,
		targetMAC: hostMAC, targetIP: h.stack.address}))
	if _, ok := h.stack.lookupNeighbor(address, h.now); !ok {
		h.t.Fatalf("neighbor %v not learned", address)
	}
	h.drain()
}

func (h *harness) completion() Completion {
	h.t.Helper()
	completion, ok := h.stack.Completion()
	if !ok {
		h.t.Fatal("no completion")
	}
	if extra, ok := h.stack.Completion(); ok {
		h.t.Fatalf("unexpected second completion %+v", extra)
	}
	return completion
}

func (h *harness) noCompletion() {
	h.t.Helper()
	if completion, ok := h.stack.Completion(); ok {
		h.t.Fatalf("unexpected completion %+v", completion)
	}
}

func ethernet(destination, source MAC, etherType uint16, payload []byte) []byte {
	frame := make([]byte, HeaderSize+len(payload))
	putEthernet(frame, destination, source, etherType)
	copy(frame[HeaderSize:], payload)
	return frame
}

func arpFrame(destination MAC, packet arpPacket) []byte {
	payload := make([]byte, arpSize)
	putARP(payload, packet)
	return ethernet(destination, packet.senderMAC, etherTypeARP, payload)
}

func ipv4Bytes(source, destination IPv4, protocol uint8, payload []byte) []byte {
	packet := make([]byte, ipv4HeaderSize+len(payload))
	putIPv4Header(packet, source, destination, protocol, 77, len(payload))
	copy(packet[ipv4HeaderSize:], payload)
	return packet
}

func icmpBytes(kind, code uint8, identifier, sequence uint16, body []byte) []byte {
	message := make([]byte, icmpHeaderSize+len(body))
	putICMPEcho(message, kind, identifier, sequence, body)
	message[1] = code
	message[2], message[3] = 0, 0
	binary.BigEndian.PutUint16(message[2:4], checksum(message))
	return message
}

func udpBytes(source, destination IPv4, sourcePort, destinationPort uint16, payload []byte) []byte {
	datagram := make([]byte, udpHeaderSize+len(payload))
	copy(datagram[udpHeaderSize:], payload)
	putUDP(datagram, source, destination, sourcePort, destinationPort, len(payload))
	return datagram
}

func ipv4Frame(destination, source MAC, from, to IPv4, protocol uint8, payload []byte) []byte {
	return ethernet(destination, source, etherTypeIPv4, ipv4Bytes(from, to, protocol, payload))
}

// sentFrame is one decoded outgoing frame.
type sentFrame struct {
	ether ethernetFrame
	arp   arpPacket
	ip    ipv4Packet
	icmp  icmpMessage
	udp   udpDatagram
}

func decode(t testing.TB, frame []byte) sentFrame {
	t.Helper()
	var sent sentFrame
	ether, ok := parseEthernet(frame)
	if !ok || len(frame) < minFrame {
		t.Fatalf("bad outgoing frame length %d", len(frame))
	}
	sent.ether = ether
	if ether.source != hostMAC {
		t.Fatalf("outgoing source MAC %v", ether.source)
	}
	switch ether.etherType {
	case etherTypeARP:
		packet, status := parseARP(ether.payload)
		if status != parseOK {
			t.Fatalf("outgoing ARP status %d", status)
		}
		sent.arp = packet
	case etherTypeIPv4:
		packet, status := parseIPv4(ether.payload)
		if status != parseOK {
			t.Fatalf("outgoing IPv4 status %d", status)
		}
		if ether.payload[8] != ipv4DefaultTTL {
			t.Fatalf("TTL %d", ether.payload[8])
		}
		sent.ip = packet
		switch packet.protocol {
		case protocolICMP:
			message, ok := parseICMP(packet.payload)
			if !ok {
				t.Fatal("outgoing ICMP checksum")
			}
			sent.icmp = message
		case protocolUDP:
			datagram, ok := parseUDP(packet)
			if !ok || binary.BigEndian.Uint16(packet.payload[6:8]) == 0 {
				t.Fatal("outgoing UDP checksum")
			}
			sent.udp = datagram
		default:
			t.Fatalf("outgoing protocol %d", packet.protocol)
		}
	default:
		t.Fatalf("outgoing ether type %#x", ether.etherType)
	}
	return sent
}

func only(t testing.TB, frames [][]byte) sentFrame {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	return decode(t, frames[0])
}

func checkCounters(t testing.TB, stack *Stack, rx, drop, malformed uint64) {
	t.Helper()
	snapshot := stack.Snapshot(0)
	if snapshot.RX != rx || snapshot.Drop != drop || snapshot.Malformed != malformed {
		t.Fatalf("counters rx %d drop %d malformed %d, want %d %d %d",
			snapshot.RX, snapshot.Drop, snapshot.Malformed, rx, drop, malformed)
	}
}
