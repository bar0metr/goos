package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// config is the peer's view of the segment, fixed at start.
type config struct {
	mac        mac
	ip         ipv4
	mask       ipv4
	router     ipv4
	hasRouter  bool
	dns        []ipv4
	leaseTime  uint32
	leases     map[mac]ipv4
	zone       *zone
	noEDNS     bool
	foreignMAC mac
}

// logger writes one line per event with a single Write, so a test that
// greps the file sees whole lines as soon as they happen.
type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *logger) printf(format string, args ...any) {
	if l == nil || l.w == nil {
		return
	}
	line := fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line)
}

// host is the peer's own station on the segment. handle and command are
// pure over byte slices apart from the log and the armed defence rules.
type host struct {
	config
	log    *logger
	nextID atomic.Uint32
	// formErr answers queries that carry OPT with FORMERR. It starts as
	// -no-edns and the edns command flips it.
	formErr atomic.Bool

	mu        sync.Mutex
	defend    map[ipv4]bool
	neighbors map[ipv4]mac
	// later sends frames after a delay; the server sets it. Without it
	// a slow answer goes at once.
	later func(delay time.Duration, frames [][]byte)
}

// slowAnswer is how long a SLOW name's answer waits: well inside the
// resolver's first 3 s wait, so the query is answered while its receive
// waits in the network service.
const slowAnswer = time.Second

func newHost(cfg config, log *logger) *host {
	h := &host{config: cfg, log: log, defend: map[ipv4]bool{}}
	h.formErr.Store(cfg.noEDNS)
	return h
}

func (h *host) ipID() uint16 { return uint16(h.nextID.Add(1)) }

// handle processes one frame seen on the segment and returns the frames the
// host sends in response.
func (h *host) handle(frame []byte) [][]byte {
	if len(frame) < 14 {
		return nil
	}
	var dst, src mac
	copy(dst[:], frame[0:6])
	copy(src[:], frame[6:12])
	if src == h.mac || src == h.foreignMAC {
		return nil
	}
	toHost := dst == h.mac || dst == broadcastMAC
	payload := frame[14:]
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case etherTypeARP:
		return h.handleARP(payload, toHost)
	case etherTypeIPv4:
		if !toHost {
			return nil
		}
		return h.handleIPv4(src, payload)
	}
	return nil
}

func (h *host) handleIPv4(src mac, data []byte) [][]byte {
	packet, ok := parseIPv4(data)
	if !ok {
		return nil
	}
	if packet.src != (ipv4{}) {
		h.learn(src, packet.src)
	}
	switch packet.protocol {
	case 1:
		if packet.dst != h.ip {
			return nil
		}
		return h.handleICMP(src, packet)
	case 17:
		datagram, ok := parseUDP(packet.payload)
		if !ok {
			return nil
		}
		switch {
		case datagram.dstPort == 67 && (packet.dst == h.ip || packet.dst == broadcastIPv4):
			return h.handleDHCP(datagram.payload)
		case datagram.dstPort == 53 && packet.dst == h.ip:
			return h.handleDNS(src, packet.src, datagram)
		case datagram.dstPort == echoPort && packet.dst == h.ip:
			return h.handleEcho(src, packet.src, datagram)
		}
	}
	return nil
}

// handleICMP answers an echo request with an echo reply that carries the
// same identifier, sequence number and data.
func (h *host) handleICMP(src mac, packet ipPacket) [][]byte {
	message := packet.payload
	if len(message) < 8 || message[0] != 8 || message[1] != 0 {
		return nil
	}
	reply := append([]byte(nil), message...)
	reply[0] = 0
	reply[2], reply[3] = 0, 0
	binary.BigEndian.PutUint16(reply[2:4], checksum(reply, 0))
	ip := buildIPv4(h.ip, packet.src, 1, h.ipID(), reply)
	return [][]byte{etherFrame(src, h.mac, etherTypeIPv4, ip)}
}

func (h *host) handleDNS(src mac, client ipv4, datagram udpDatagram) [][]byte {
	query, ok := parseQuery(datagram.payload)
	if !ok {
		return nil
	}
	h.log.printf("query %s %d %s:%d id=%d", query.logName(), query.qtype, client, datagram.srcPort, query.id)
	response := answer(query, h.zone, h.formErr.Load())
	if response == nil {
		return nil
	}
	udp := buildUDP(h.ip, client, 53, datagram.srcPort, response)
	ip := buildIPv4(h.ip, client, 17, h.ipID(), udp)
	frames := [][]byte{etherFrame(src, h.mac, etherTypeIPv4, ip)}
	if entry := h.zone.lookup(query.name); entry != nil && entry.slow && h.later != nil {
		h.log.printf("slow %s %s", query.logName(), slowAnswer)
		h.later(slowAnswer, frames)
		return nil
	}
	return frames
}

// command runs one control line and returns the frames to send and whether
// the peer should exit.
func (h *host) command(line string) ([][]byte, bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, false, nil
	}
	switch fields[0] {
	case "claim", "defend-probe":
		if len(fields) != 2 {
			return nil, false, fmt.Errorf("%s needs one address", fields[0])
		}
		addr, err := parseIPv4Addr(fields[1])
		if err != nil {
			return nil, false, err
		}
		if fields[0] == "claim" {
			h.log.printf("claim %s", addr)
			return [][]byte{claimFrame(h.foreignMAC, addr)}, false, nil
		}
		h.mu.Lock()
		h.defend[addr] = true
		h.mu.Unlock()
		h.log.printf("defend-probe %s", addr)
		return nil, false, nil
	case "edns":
		if len(fields) != 2 || fields[1] != "on" && fields[1] != "off" {
			return nil, false, fmt.Errorf("edns takes on or off")
		}
		h.formErr.Store(fields[1] == "off")
		h.log.printf("edns %s", fields[1])
		return nil, false, nil
	case "clear":
		h.mu.Lock()
		h.defend = map[ipv4]bool{}
		h.mu.Unlock()
		h.log.printf("clear")
		return nil, false, nil
	case "quit":
		h.log.printf("quit")
		return nil, true, nil
	case "udp-send", "udp-bad", "udp-fill", "udp-burst":
		frames, err := h.udpCommand(fields)
		return frames, false, err
	}
	return nil, false, fmt.Errorf("unknown command %q", fields[0])
}

func (h *host) armed(addr ipv4) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.defend[addr]
}
