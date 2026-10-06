package network

import (
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
		if datagram, ok := parseUDP(packet); ok && len(datagram.payload) > len(packet.payload)-udpHeaderSize {
			t.Fatal("UDP payload past the packet")
		}
		parseICMP(packet.payload)
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
