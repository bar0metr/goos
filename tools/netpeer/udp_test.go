package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// udpOut decodes one frame the peer sent to the guest as UDP.
func udpOut(t *testing.T, frame []byte) (ipPacket, udpDatagram) {
	t.Helper()
	dst, _, _ := etherHeader(t, frame)
	packet, ok := parseIPv4(frame[14:])
	datagram, ok2 := parseUDP(packet.payload)
	if dst != guestMAC || !ok || !ok2 || packet.protocol != 17 || packet.dst != guestIP || packet.src != peerIP {
		t.Fatalf("frame %s %+v %+v", dst, packet, datagram)
	}
	return packet, datagram
}

// validChecksum checks a UDP checksum over the pseudo-header.
func validChecksum(packet ipPacket) bool {
	sum := uint32(packet.src[0])<<8 | uint32(packet.src[1])
	sum += uint32(packet.src[2])<<8 | uint32(packet.src[3])
	sum += uint32(packet.dst[0])<<8 | uint32(packet.dst[1])
	sum += uint32(packet.dst[2])<<8 | uint32(packet.dst[3])
	sum += 17 + uint32(len(packet.payload))
	return checksum(packet.payload, sum) == 0
}

func TestUDPEcho(t *testing.T) {
	var log bytes.Buffer
	h := testHost(t, &log)
	for _, size := range []int{0, 5, 1472} {
		payload := bytes.Repeat([]byte("x"), size)
		frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 50000, echoPort, payload))
		packet, datagram := udpOut(t, one(t, h.handle(frame)))
		if datagram.srcPort != echoPort || datagram.dstPort != 50000 || !bytes.Equal(datagram.payload, payload) || !validChecksum(packet) {
			t.Fatalf("echo of %d bytes: %+v", size, datagram)
		}
	}
	if !strings.Contains(log.String(), "udp-echo 192.0.2.10:50000 1472\n") {
		t.Fatalf("log %q", log.String())
	}
}

func TestUDPEchoDecoy(t *testing.T) {
	h := testHost(t, io.Discard)
	frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 50001, echoPort, []byte("decoy test")))
	frames := h.handle(frame)
	if len(frames) != 2 {
		t.Fatalf("%d frames", len(frames))
	}
	if _, decoy := udpOut(t, frames[0]); decoy.srcPort != decoyPort || string(decoy.payload) != "decoy" {
		t.Fatalf("decoy %+v", decoy)
	}
	if _, echo := udpOut(t, frames[1]); echo.srcPort != echoPort || string(echo.payload) != "decoy test" {
		t.Fatalf("echo %+v", echo)
	}
}

func TestUDPCommands(t *testing.T) {
	h := testHost(t, io.Discard)
	if _, _, err := h.command("udp-send 192.0.2.10 9000 4000 hi"); err == nil {
		t.Fatal("sent to a guest never seen")
	}
	h.handle(ipv4Frame(peerMAC, guestIP, peerIP, 1, []byte{0}))
	frames, _, err := h.command("udp-send 192.0.2.10 9000 4000 hello there")
	if err != nil {
		t.Fatal(err)
	}
	packet, datagram := udpOut(t, one(t, frames))
	if datagram.srcPort != 4000 || datagram.dstPort != 9000 || string(datagram.payload) != "hello there" || !validChecksum(packet) {
		t.Fatalf("send %+v", datagram)
	}
	frames, _, _ = h.command("udp-bad 192.0.2.10 9000 4000 bad")
	if packet, _ := udpOut(t, one(t, frames)); validChecksum(packet) || binary.BigEndian.Uint16(packet.payload[6:8]) == 0 {
		t.Fatal("udp-bad checksum verifies")
	}
	frames, _, _ = h.command("udp-fill 192.0.2.10 9000 4000 0")
	if _, datagram := udpOut(t, one(t, frames)); len(datagram.payload) != 0 {
		t.Fatalf("fill 0: %d bytes", len(datagram.payload))
	}
	frames, _, _ = h.command("udp-fill 192.0.2.10 9000 4000 1472")
	if _, datagram := udpOut(t, one(t, frames)); len(datagram.payload) != 1472 || datagram.payload[27] != 'b' {
		t.Fatalf("fill 1472: %d bytes", len(datagram.payload))
	}
	frames, _, _ = h.command("udp-burst 192.0.2.10 9000 4000 3")
	if len(frames) != 3 {
		t.Fatalf("%d burst frames", len(frames))
	}
	for index, frame := range frames {
		if _, datagram := udpOut(t, frame); string(datagram.payload) != "burst "+string(rune('1'+index)) {
			t.Fatalf("burst %d: %q", index, datagram.payload)
		}
	}
	for _, line := range []string{"udp-send 192.0.2.10 0 4000 x", "udp-fill 192.0.2.10 9000 4000 1473",
		"udp-burst 192.0.2.10 9000 4000 0", "udp-send 192.0.2.10 9000"} {
		if _, _, err := h.command(line); err == nil {
			t.Errorf("%q accepted", line)
		}
	}
}

func TestEDNSCommand(t *testing.T) {
	h := testHost(t, io.Discard)
	query := dnsQueryBytes(9, "host.test", typeA, true)
	frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 40000, 53, query))
	rcode := func() uint16 {
		_, datagram := udpOut(t, one(t, h.handle(frame)))
		return binary.BigEndian.Uint16(datagram.payload[2:4]) & 0xf
	}
	if rcode() != 0 {
		t.Fatal("EDNS refused before edns off")
	}
	if _, _, err := h.command("edns off"); err != nil || rcode() != rcodeFormErr {
		t.Fatalf("edns off: %v", err)
	}
	if _, _, err := h.command("edns on"); err != nil || rcode() != 0 {
		t.Fatalf("edns on: %v", err)
	}
	if _, _, err := h.command("edns maybe"); err == nil {
		t.Fatal("edns maybe accepted")
	}
}
