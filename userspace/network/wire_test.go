package network

import (
	"encoding/binary"
	"testing"
)

func TestChecksumKnownVector(t *testing.T) {
	// The IPv4 header example from RFC 1071 discussions: checksum 0xb861.
	header := []byte{0x45, 0x00, 0x00, 0x73, 0x00, 0x00, 0x40, 0x00, 0x40, 0x11, 0x00, 0x00,
		0xc0, 0xa8, 0x00, 0x01, 0xc0, 0xa8, 0x00, 0xc7}
	if got := checksum(header); got != 0xb861 {
		t.Fatalf("checksum = %#x", got)
	}
	binary.BigEndian.PutUint16(header[10:12], 0xb861)
	if checksum(header) != 0 {
		t.Fatal("checksum over a valid header is not zero")
	}
	if got := checksum([]byte{0x01}); got != ^uint16(0x0100) {
		t.Fatalf("odd checksum = %#x", got)
	}
}

func TestParseEthernetBounds(t *testing.T) {
	for _, length := range []int{0, 1, HeaderSize - 1, MaxFrame + 1, 4096} {
		if _, ok := parseEthernet(make([]byte, length)); ok {
			t.Errorf("length %d accepted", length)
		}
	}
	for _, length := range []int{HeaderSize, minFrame, MaxFrame} {
		frame, ok := parseEthernet(make([]byte, length))
		if !ok || len(frame.payload) != length-HeaderSize {
			t.Errorf("length %d rejected", length)
		}
	}
}

func TestParseARP(t *testing.T) {
	good := make([]byte, arpSize)
	putARP(good, arpPacket{operation: arpRequest, senderMAC: peerMAC, senderIP: peerIP, targetIP: hostIP})
	mutate := func(change func([]byte)) []byte {
		data := append([]byte(nil), good...)
		change(data)
		return data
	}
	for _, test := range []struct {
		name string
		data []byte
		want parseStatus
	}{
		{"good", good, parseOK},
		{"padded", append(append([]byte(nil), good...), make([]byte, 18)...), parseOK},
		{"truncated", good[:arpSize-1], parseMalformed},
		{"empty", nil, parseMalformed},
		{"hardware type", mutate(func(d []byte) { d[1] = 6 }), parseUnsupported},
		{"protocol type", mutate(func(d []byte) { d[2] = 0x86; d[3] = 0xdd }), parseUnsupported},
		{"hardware length", mutate(func(d []byte) { d[4] = 8 }), parseUnsupported},
		{"protocol length", mutate(func(d []byte) { d[5] = 16 }), parseUnsupported},
		{"operation rarp", mutate(func(d []byte) { d[7] = 3 }), parseUnsupported},
		{"operation zero", mutate(func(d []byte) { d[7] = 0 }), parseUnsupported},
	} {
		packet, status := parseARP(test.data)
		if status != test.want {
			t.Errorf("%s: status %d, want %d", test.name, status, test.want)
		}
		if status == parseOK && (packet.senderIP != peerIP || packet.senderMAC != peerMAC || packet.targetIP != hostIP) {
			t.Errorf("%s: fields %+v", test.name, packet)
		}
	}
}

func TestParseIPv4Packet(t *testing.T) {
	good := ipv4Bytes(peerIP, hostIP, protocolICMP, []byte{1, 2, 3, 4})
	rechecksum := func(data []byte) {
		data[10], data[11] = 0, 0
		headerLength := int(data[0]&0x0f) * 4
		if headerLength > len(data) {
			headerLength = len(data)
		}
		binary.BigEndian.PutUint16(data[10:12], checksum(data[:headerLength]))
	}
	mutate := func(change func([]byte), fix bool) []byte {
		data := append([]byte(nil), good...)
		change(data)
		if fix {
			rechecksum(data)
		}
		return data
	}
	withOptions := make([]byte, 28)
	copy(withOptions, good[:ipv4HeaderSize])
	withOptions[0] = 0x46
	binary.BigEndian.PutUint16(withOptions[2:4], 28)
	rechecksum(withOptions)

	for _, test := range []struct {
		name    string
		data    []byte
		want    parseStatus
		payload int
	}{
		{"good", good, parseOK, 4},
		{"ethernet padding trimmed", append(append([]byte(nil), good...), 0, 0, 0, 0, 0, 0), parseOK, 4},
		{"truncated header", good[:19], parseMalformed, 0},
		{"total beyond data", good[:23], parseMalformed, 0},
		{"version 6", mutate(func(d []byte) { d[0] = 0x65 }, true), parseMalformed, 0},
		{"ihl 4", mutate(func(d []byte) { d[0] = 0x44 }, true), parseMalformed, 0},
		{"total below header", mutate(func(d []byte) { binary.BigEndian.PutUint16(d[2:4], 19) }, true), parseMalformed, 0},
		{"total huge", mutate(func(d []byte) { binary.BigEndian.PutUint16(d[2:4], 0xffff) }, true), parseMalformed, 0},
		{"bad checksum", mutate(func(d []byte) { d[10] ^= 1 }, false), parseMalformed, 0},
		{"options", withOptions, parseUnsupported, 0},
		{"ihl beyond total", mutate(func(d []byte) { d[0] = 0x4f }, true), parseMalformed, 0},
		{"more fragments", mutate(func(d []byte) { d[6] = 0x20 }, true), parseUnsupported, 0},
		{"fragment offset", mutate(func(d []byte) { d[7] = 1 }, true), parseUnsupported, 0},
		{"dont fragment allowed", mutate(func(d []byte) { d[6] = 0x40 }, true), parseOK, 4},
		{"ttl zero", mutate(func(d []byte) { d[8] = 0 }, true), parseUnsupported, 0},
	} {
		packet, status := parseIPv4(test.data)
		if status != test.want {
			t.Errorf("%s: status %d, want %d", test.name, status, test.want)
			continue
		}
		if status == parseOK && (len(packet.payload) != test.payload || packet.source != peerIP || packet.destination != hostIP) {
			t.Errorf("%s: packet %+v", test.name, packet)
		}
	}
}

func TestParseICMP(t *testing.T) {
	good := icmpBytes(icmpEchoRequest, 0, 0x1234, 7, []byte("payload"))
	if message, ok := parseICMP(good); !ok || message.identifier != 0x1234 || message.sequence != 7 ||
		string(message.body) != "payload" {
		t.Fatalf("good ICMP: %+v %v", message, ok)
	}
	if _, ok := parseICMP(good[:icmpHeaderSize-1]); ok {
		t.Error("truncated ICMP accepted")
	}
	if _, ok := parseICMP(good[:len(good)-1]); ok {
		t.Error("ICMP with a byte missing passed its checksum")
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0x40
	if _, ok := parseICMP(bad); ok {
		t.Error("bad ICMP checksum accepted")
	}
}

func TestParseQuotedEcho(t *testing.T) {
	quoted := append(ipv4Bytes(hostIP, remoteIP, protocolICMP, nil)[:ipv4HeaderSize],
		icmpBytes(icmpEchoRequest, 0, 9, 10, nil)...)
	if echo, ok := parseQuotedEcho(quoted); !ok || echo.source != hostIP || echo.destination != remoteIP ||
		echo.identifier != 9 || echo.sequence != 10 {
		t.Fatalf("quoted echo %+v %v", echo, ok)
	}
	for name, change := range map[string]func([]byte){
		"options":  func(d []byte) { d[0] = 0x46 },
		"protocol": func(d []byte) { d[9] = protocolUDP },
		"reply":    func(d []byte) { d[ipv4HeaderSize] = icmpEchoReply },
		"code":     func(d []byte) { d[ipv4HeaderSize+1] = 1 },
	} {
		data := append([]byte(nil), quoted...)
		change(data)
		if _, ok := parseQuotedEcho(data); ok {
			t.Errorf("%s accepted", name)
		}
	}
	if _, ok := parseQuotedEcho(quoted[:icmpQuotedEcho-1]); ok {
		t.Error("short quote accepted")
	}
}

func TestParseUDP(t *testing.T) {
	server := IPv4{10, 0, 2, 2}
	build := func(payload []byte) ipv4Packet {
		return ipv4Packet{source: server, destination: broadcastIPv4, protocol: protocolUDP,
			payload: udpBytes(server, broadcastIPv4, portServer, portClient, payload)}
	}
	good := build([]byte("hello"))
	if datagram, status := parseUDP(good); status != udpOK || string(datagram.payload) != "hello" ||
		datagram.sourcePort != portServer || datagram.destinationPort != portClient {
		t.Fatalf("good UDP %+v %v", datagram, status)
	}
	noChecksum := build([]byte("hello"))
	noChecksum.payload[6], noChecksum.payload[7] = 0, 0
	if _, status := parseUDP(noChecksum); status != udpOK {
		t.Error("zero checksum rejected")
	}
	badChecksum := build([]byte("hello"))
	badChecksum.payload[len(badChecksum.payload)-1] ^= 1
	if _, status := parseUDP(badChecksum); status != udpBadChecksum {
		t.Error("bad checksum accepted")
	}
	wrongPseudo := build([]byte("hello"))
	wrongPseudo.destination = IPv4{10, 0, 2, 15}
	if _, status := parseUDP(wrongPseudo); status != udpBadChecksum {
		t.Error("checksum ignored the pseudo-header")
	}
	padded := build([]byte("hello"))
	padded.payload = append(padded.payload, 0, 0, 0)
	if datagram, status := parseUDP(padded); status != udpOK || string(datagram.payload) != "hello" {
		t.Error("trailing bytes after the UDP length were not trimmed")
	}
	for _, length := range []uint16{0, 7, 14, 0xffff} {
		packet := build([]byte("hello"))
		binary.BigEndian.PutUint16(packet.payload[4:6], length)
		if _, status := parseUDP(packet); status != udpMalformed {
			t.Errorf("UDP length %d accepted", length)
		}
	}
	if _, status := parseUDP(ipv4Packet{payload: []byte{0, 67, 0, 68, 0, 8, 0}}); status != udpMalformed {
		t.Error("truncated UDP header accepted")
	}
}
