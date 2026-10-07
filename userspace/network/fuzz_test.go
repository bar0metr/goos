package network

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// fuzzStack builds a stack in one of several states so fuzzed input reaches
// the ready, DHCP-selecting and DHCP-requesting paths.
func fuzzStack(t *testing.T, state byte) *harness {
	switch state % 3 {
	case 0:
		h := readyWithPeer(t)
		h.stack.Echo(0, peerIP, 1, second, h.now)
		h.stack.Echo(1, remoteIP, 2, second, h.now)
		h.stack.Echo(2, IPv4{10, 0, 2, 70}, 3, second, h.now)
		h.drain()
		return h
	case 1:
		h, _ := dhcpHarness(t)
		return h
	default:
		h, discover := dhcpHarness(t)
		h.receive(dhcpFrame(replyFor(discover, hostIP, leaseOptions(dhcpOffer)).bytes()))
		h.drain()
		return h
	}
}

// wrapFuzz turns fuzz data into a frame shaped by kind, fixing the checksums
// and identifiers the fuzzer cannot guess so deeper parsers see the input.
func wrapFuzz(h *harness, kind byte, data []byte) []byte {
	switch kind % 4 {
	case 1:
		packet := append([]byte(nil), data...)
		if len(packet) >= ipv4HeaderSize {
			headerLength := int(packet[0]&0x0f) * 4
			if headerLength >= ipv4HeaderSize && headerLength <= len(packet) {
				packet[10], packet[11] = 0, 0
				binary.BigEndian.PutUint16(packet[10:12], checksum(packet[:headerLength]))
			}
		}
		return ethernet(hostMAC, peerMAC, etherTypeIPv4, packet)
	case 2:
		var kindByte, code byte
		if len(data) >= 2 {
			kindByte, code, data = data[0], data[1], data[2:]
		}
		identifier := h.stack.echoBase + uint16(len(data)%MaxSessions)
		message := icmpBytes(kindByte, code, identifier, 1, data)
		if len(message) > MTU-ipv4HeaderSize {
			message = message[:MTU-ipv4HeaderSize]
		}
		return ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, message)
	case 3:
		payload := append([]byte(nil), data...)
		if len(payload) >= dhcpChaddrAt+6 {
			binary.BigEndian.PutUint32(payload[4:8], h.stack.dhcp.xid)
			copy(payload[dhcpChaddrAt:], hostMAC[:])
		}
		if len(payload) > MTU-ipv4HeaderSize-udpHeaderSize {
			payload = payload[:MTU-ipv4HeaderSize-udpHeaderSize]
		}
		return dhcpFrame(payload)
	}
	return data
}

func FuzzReceive(f *testing.F) {
	request := icmpBytes(icmpEchoRequest, 0, 1, 1, []byte("ping"))
	f.Add(byte(0), ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolICMP, request))
	f.Add(byte(0), arpFrame(broadcastMAC, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP, targetIP: hostIP}))
	f.Add(byte(4), arpFrame(hostMAC, arpPacket{operation: arpReply, senderMAC: peerMAC, senderIP: hostIP, targetIP: hostIP}))
	f.Add(byte(1), ipv4Bytes(peerIP, hostIP, protocolICMP, request))
	f.Add(byte(2), append([]byte{icmpUnreachable, 1}, make([]byte, 40)...))
	f.Add(byte(2), append([]byte{icmpEchoReply, 0}, make([]byte, EchoPayload)...))
	offer := serverReply{op: bootReply, htype: hardwareEther, hlen: hardwareLength, chaddr: hostMAC, yiaddr: hostIP,
		cookie: dhcpCookie, options: encodeOptions(leaseOptions(dhcpOffer))}
	f.Add(byte(3+4), offer.bytes())
	ack := offer
	ack.options = encodeOptions(leaseOptions(dhcpAck))
	f.Add(byte(3+8), ack.bytes())
	overloaded := offer
	overloaded.options = encodeOptions([]option{{optionMessageType, []byte{dhcpOffer}}, {optionOverload, []byte{overloadBoth}}})
	overloaded.file = encodeOptions(leaseOptions(dhcpOffer)[1:4])
	overloaded.sname = encodeOptions(leaseOptions(dhcpOffer)[4:5])
	f.Add(byte(3+4), overloaded.bytes())

	f.Fuzz(func(t *testing.T, selector byte, data []byte) {
		h := fuzzStack(t, selector/4)
		before := h.stack.Snapshot(h.now)
		frame := wrapFuzz(h, selector, data)
		h.receive(frame)
		h.receive(frame)
		after := h.stack.Snapshot(h.now)
		if after.RX != before.RX+2 {
			t.Fatalf("rx %d -> %d", before.RX, after.RX)
		}
		if after.Malformed-before.Malformed > 2 {
			t.Fatalf("malformed grew by %d for two frames", after.Malformed-before.Malformed)
		}
		if h.stack.txCount < 0 || h.stack.txCount > TxQueueFrames {
			t.Fatalf("tx count %d", h.stack.txCount)
		}
		for _, sent := range h.drain() {
			if len(sent) < minFrame || len(sent) > MaxFrame {
				t.Fatalf("sent frame of %d bytes", len(sent))
			}
			decode(t, sent)
		}
		h.runUntil(h.now + 70*second)
		h.drain()
		for {
			if _, ok := h.stack.Completion(); !ok {
				break
			}
		}
		for index := range h.stack.sessions[:] {
			if h.stack.sessions[index].phase != echoIdle {
				t.Fatalf("session %d still busy after every deadline", index)
			}
		}
	})
}

func FuzzParseDHCP(f *testing.F) {
	offer := serverReply{op: bootReply, htype: hardwareEther, hlen: hardwareLength, xid: 7, chaddr: hostMAC, yiaddr: hostIP,
		cookie: dhcpCookie, options: encodeOptions(leaseOptions(dhcpOffer))}
	f.Add(offer.bytes())
	overloaded := offer
	overloaded.options = encodeOptions([]option{{optionMessageType, []byte{dhcpOffer}}, {optionOverload, []byte{overloadBoth}}})
	overloaded.file = encodeOptions(leaseOptions(dhcpOffer)[1:4])
	overloaded.sname = encodeOptions(leaseOptions(dhcpOffer)[4:5])
	f.Add(overloaded.bytes())
	f.Add(offer.bytes()[:dhcpOptionsAt])
	f.Fuzz(func(t *testing.T, data []byte) {
		message, status := parseDHCP(data, 7, hostMAC)
		if status != parseOK {
			return
		}
		if message.seen&seenKind == 0 {
			t.Fatal("accepted without a message type")
		}
		lease, ok := message.validLease()
		if !ok {
			return
		}
		if !hostAddress(lease.address, lease.prefix) || lease.length < minLeaseSeconds*second ||
			!(lease.renew < lease.rebind && lease.rebind < lease.length) || lease.renew == 0 {
			t.Fatalf("invalid lease accepted: %+v", lease)
		}
		if lease.router != zeroIPv4 && !samePrefix(lease.router, lease.address, lease.prefix) {
			t.Fatalf("router outside prefix: %+v", lease)
		}
	})
}

func FuzzParsers(f *testing.F) {
	f.Add(ipv4Bytes(peerIP, hostIP, protocolUDP, udpBytes(peerIP, hostIP, portServer, portClient, []byte("x"))))
	f.Add(ipv4Bytes(peerIP, hostIP, protocolICMP, icmpBytes(icmpEchoRequest, 0, 1, 2, []byte("abc"))))
	f.Add(arpPayload(arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP, targetIP: hostIP}))
	f.Fuzz(func(t *testing.T, data []byte) {
		parseARP(data)
		parseQuotedEcho(data)
		if message, ok := parseICMP(data); ok && len(message.body) != len(data)-icmpHeaderSize {
			t.Fatal("ICMP body length")
		}
		packet, status := parseIPv4(data)
		if status != parseOK {
			return
		}
		if len(packet.payload) > len(data)-ipv4HeaderSize {
			t.Fatal("IPv4 payload past the data")
		}
		if datagram, status := parseUDP(packet); status == udpOK && len(datagram.payload) > len(packet.payload)-udpHeaderSize {
			t.Fatal("UDP payload past the packet")
		}
		parseICMP(packet.payload)
	})
}

func FuzzParseTCP(f *testing.F) {
	source, destination := IPv4{192, 0, 2, 1}, IPv4{198, 51, 100, 2}
	var seed [64]byte
	length := putTCP(seed[:], source, destination, tcpSegment{sourcePort: 1234, destinationPort: 80,
		sequence: 7, flags: tcpFlagSYN, window: 4096, mss: 1200})
	f.Add(seed[:length], true)
	f.Add([]byte{0, 1, 0, 2}, false)
	f.Fuzz(func(t *testing.T, data []byte, fixChecksum bool) {
		if len(data) > MTU-ipv4HeaderSize {
			return
		}
		data = append([]byte(nil), data...)
		if fixChecksum && len(data) >= tcpHeaderSize {
			data[16], data[17] = 0, 0
			sum := checksumFold(checksumAdd(pseudoHeaderSum(source, destination, protocolTCP, len(data)), data))
			binary.BigEndian.PutUint16(data[16:18], sum)
		}
		segment, status := parseTCP(ipv4Packet{source: source, destination: destination, protocol: protocolTCP, payload: data})
		if status != tcpOK {
			return
		}
		headerLength := int(data[12]>>4) * 4
		if segment.sourcePort == 0 || segment.destinationPort == 0 || headerLength < tcpHeaderSize ||
			headerLength > len(data) || len(segment.payload) != len(data)-headerLength {
			t.Fatalf("invalid TCP segment accepted: %+v", segment)
		}
	})
}

func FuzzParseIPv4Text(f *testing.F) {
	for _, seed := range []string{"10.0.2.15", "0.0.0.0", "255.255.255.255", "01.2.3.4", "1.2.3"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, text []byte) {
		address, ok := ParseIPv4(text)
		if !ok {
			return
		}
		var buffer [MaxIPv4Text]byte
		if string(AppendIPv4(buffer[:0], address)) != string(text) {
			t.Fatalf("%q is not canonical", text)
		}
	})
}

// FuzzUDPReceive feeds arbitrary UDP bytes, with or without a valid
// checksum, to bound endpoints. Nothing but the queues and counters may
// change, and a queued payload is exactly what the datagram carried.
func FuzzUDPReceive(f *testing.F) {
	f.Add(udpBytes(peerIP, hostIP, 53, 9000, []byte("hello")), true)
	f.Add(udpBytes(peerIP, hostIP, 53, 9002, nil), true)
	f.Add([]byte{0, 53, 0x23, 0x28, 0, 8, 0, 0}, false)
	f.Fuzz(func(t *testing.T, datagram []byte, fix bool) {
		if len(datagram) > MTU-ipv4HeaderSize {
			return
		}
		h := readyWithPeer(t)
		wildcard, specific, connected := h.open(0), h.open(0), h.open(1)
		h.bind(0, wildcard, IPv4{}, 9000)
		h.bind(0, specific, hostIP, 9001)
		_, port, _ := h.stack.UDPConnect(1, connected, peerIP, 53, h.now)
		if fix && len(datagram) >= udpHeaderSize {
			datagram = append([]byte(nil), datagram...)
			length := int(binary.BigEndian.Uint16(datagram[4:6]))
			if length >= udpHeaderSize && length <= len(datagram) {
				datagram[6], datagram[7] = 0, 0
				sum := checksumFold(checksumAdd(pseudoHeaderSum(peerIP, hostIP, protocolUDP, length), datagram[:length]))
				if sum == 0 {
					sum = 0xffff
				}
				binary.BigEndian.PutUint16(datagram[6:8], sum)
			}
		}
		endpoints := h.stack.udp
		h.receive(ipv4Frame(hostMAC, peerMAC, peerIP, hostIP, protocolUDP, datagram))
		for index := range endpoints {
			before, after := endpoints[index], h.stack.udp[index]
			before.queue, before.head, before.count = after.queue, after.head, after.count
			if before != after {
				t.Fatalf("endpoint %d changed: %+v -> %+v", index, endpoints[index], h.stack.udp[index])
			}
		}
		queued := 0
		for _, owner := range []struct {
			owner int
			id    EndpointID
		}{{0, wildcard}, {0, specific}, {1, connected}} {
			var buffer [UDPPayloadCapacity]byte
			received, ok := h.stack.UDPReceive(owner.owner, owner.id, buffer[:])
			if !ok {
				continue
			}
			queued++
			parsed, status := parseUDP(ipv4Packet{source: peerIP, destination: hostIP, payload: datagram})
			if status != udpOK || received.Source != peerIP || received.SourcePort != parsed.sourcePort ||
				!bytes.Equal(buffer[:received.Length], parsed.payload) {
				t.Fatalf("queued %+v from %x", received, datagram)
			}
			if owner.id == connected && (parsed.sourcePort != 53 || parsed.destinationPort != port) {
				t.Fatal("connected endpoint took a datagram from another peer")
			}
		}
		if queued > 1 {
			t.Fatalf("one datagram queued %d times", queued)
		}
	})
}
