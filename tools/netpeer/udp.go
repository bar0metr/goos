package main

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Generic UDP for the network gate. The peer runs an echo service on port 7
// and, on command, sends datagrams to a guest: plain text, a run of numbered
// datagrams, a payload of a given size, or one with a broken checksum. A
// guest's MAC is learned from the frames it sends.

const (
	echoPort  = 7
	decoyPort = 8
	// decoyPrefix makes the echo send a decoy from decoyPort first, to show
	// that a connected endpoint ignores a peer's other ports.
	decoyPrefix = "decoy"
)

// learn records the MAC an IPv4 source sent from.
func (h *host) learn(src mac, address ipv4) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.neighbors == nil {
		h.neighbors = map[ipv4]mac{}
	}
	h.neighbors[address] = src
}

func (h *host) neighbor(address ipv4) (mac, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.neighbors[address]
	return m, ok
}

// handleEcho returns the payload to the port it came from, after a decoy
// from another port when the payload asks for one.
func (h *host) handleEcho(src mac, client ipv4, datagram udpDatagram) [][]byte {
	h.log.printf("udp-echo %s:%d %d", client, datagram.srcPort, len(datagram.payload))
	var frames [][]byte
	if strings.HasPrefix(string(datagram.payload), decoyPrefix) {
		frames = append(frames, h.udpFrame(src, client, decoyPort, datagram.srcPort, []byte("decoy"), false))
	}
	return append(frames, h.udpFrame(src, client, echoPort, datagram.srcPort, datagram.payload, false))
}

func (h *host) udpFrame(dst mac, to ipv4, srcPort, dstPort uint16, payload []byte, corrupt bool) []byte {
	udp := buildUDP(h.ip, to, srcPort, dstPort, payload)
	if corrupt {
		binary.BigEndian.PutUint16(udp[6:8], binary.BigEndian.Uint16(udp[6:8])^0x5a5a)
	}
	return etherFrame(dst, h.mac, etherTypeIPv4, buildIPv4(h.ip, to, 17, h.ipID(), udp))
}

// udpCommand runs one of
//
//	udp-send ADDRESS PORT SOURCE_PORT TEXT
//	udp-bad ADDRESS PORT SOURCE_PORT TEXT
//	udp-fill ADDRESS PORT SOURCE_PORT SIZE
//	udp-burst ADDRESS PORT SOURCE_PORT COUNT
//
// udp-bad sends TEXT with a wrong checksum; udp-fill sends SIZE bytes of a
// fill pattern; udp-burst sends COUNT datagrams numbered from 1, in order.
func (h *host) udpCommand(fields []string) ([][]byte, error) {
	if len(fields) < 5 {
		return nil, fmt.Errorf("%s needs ADDRESS PORT SOURCE_PORT and a value", fields[0])
	}
	to, err := parseIPv4Addr(fields[1])
	if err != nil {
		return nil, err
	}
	ports := [2]uint16{}
	for index, text := range fields[2:4] {
		value, err := strconv.ParseUint(text, 10, 16)
		if err != nil || value == 0 {
			return nil, fmt.Errorf("invalid port %q", text)
		}
		ports[index] = uint16(value)
	}
	dst, ok := h.neighbor(to)
	if !ok {
		return nil, fmt.Errorf("no frame seen from %s", to)
	}
	value := strings.Join(fields[4:], " ")
	h.log.printf("%s %s:%d from %d %s", fields[0], to, ports[0], ports[1], value)
	switch fields[0] {
	case "udp-send", "udp-bad":
		return [][]byte{h.udpFrame(dst, to, ports[1], ports[0], []byte(value), fields[0] == "udp-bad")}, nil
	case "udp-fill":
		size, err := strconv.ParseUint(value, 10, 16)
		if err != nil || size > 1472 {
			return nil, fmt.Errorf("invalid size %q", value)
		}
		payload := make([]byte, size)
		for index := range payload {
			payload[index] = 'a' + byte(index%26)
		}
		return [][]byte{h.udpFrame(dst, to, ports[1], ports[0], payload, false)}, nil
	case "udp-burst":
		count, err := strconv.ParseUint(value, 10, 8)
		if err != nil || count == 0 {
			return nil, fmt.Errorf("invalid count %q", value)
		}
		var frames [][]byte
		for number := uint64(1); number <= count; number++ {
			frames = append(frames, h.udpFrame(dst, to, ports[1], ports[0], []byte("burst "+strconv.FormatUint(number, 10)), false))
		}
		return frames, nil
	}
	return nil, fmt.Errorf("unknown command %q", fields[0])
}
