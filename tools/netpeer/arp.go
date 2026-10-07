package main

import "encoding/binary"

const (
	arpRequest = 1
	arpReply   = 2
)

type arpPacket struct {
	op        uint16
	senderMAC mac
	senderIP  ipv4
	targetMAC mac
	targetIP  ipv4
}

// parseARP accepts Ethernet/IPv4 ARP only.
func parseARP(data []byte) (arpPacket, bool) {
	if len(data) < 28 ||
		binary.BigEndian.Uint16(data[0:2]) != 1 ||
		binary.BigEndian.Uint16(data[2:4]) != etherTypeIPv4 ||
		data[4] != 6 || data[5] != 4 {
		return arpPacket{}, false
	}
	var p arpPacket
	p.op = binary.BigEndian.Uint16(data[6:8])
	copy(p.senderMAC[:], data[8:14])
	copy(p.senderIP[:], data[14:18])
	copy(p.targetMAC[:], data[18:24])
	copy(p.targetIP[:], data[24:28])
	return p, true
}

func (p arpPacket) bytes() []byte {
	data := make([]byte, 28)
	binary.BigEndian.PutUint16(data[0:2], 1)
	binary.BigEndian.PutUint16(data[2:4], etherTypeIPv4)
	data[4], data[5] = 6, 4
	binary.BigEndian.PutUint16(data[6:8], p.op)
	copy(data[8:14], p.senderMAC[:])
	copy(data[14:18], p.senderIP[:])
	copy(data[18:24], p.targetMAC[:])
	copy(data[24:28], p.targetIP[:])
	return data
}

// handleARP answers requests for the host's own address and, for an armed
// defend-probe rule, answers a probe for that address from the foreign MAC.
// toHost tells whether the frame was sent to the host's MAC or broadcast.
func (h *host) handleARP(data []byte, toHost bool) [][]byte {
	p, ok := parseARP(data)
	if !ok || p.op != arpRequest {
		return nil
	}
	var out [][]byte
	if p.senderIP == (ipv4{}) && h.armed(p.targetIP) {
		out = append(out, defendFrame(h.foreignMAC, p.targetIP, p.senderMAC))
		h.log.printf("defend %s", p.targetIP)
	}
	if toHost && p.targetIP == h.ip {
		reply := arpPacket{op: arpReply, senderMAC: h.mac, senderIP: h.ip, targetMAC: p.senderMAC, targetIP: p.senderIP}
		out = append(out, etherFrame(p.senderMAC, h.mac, etherTypeARP, reply.bytes()))
	}
	return out
}

// claimFrame is a broadcast ARP announcement from foreign: a request whose
// sender and target address are both addr.
func claimFrame(foreign mac, addr ipv4) []byte {
	p := arpPacket{op: arpRequest, senderMAC: foreign, senderIP: addr, targetIP: addr}
	return etherFrame(broadcastMAC, foreign, etherTypeARP, p.bytes())
}

// defendFrame is an ARP reply from foreign to the prober that claims addr.
// The target address repeats the probe's sender address, 0.0.0.0.
func defendFrame(foreign mac, addr ipv4, prober mac) []byte {
	p := arpPacket{op: arpReply, senderMAC: foreign, senderIP: addr, targetMAC: prober}
	return etherFrame(prober, foreign, etherTypeARP, p.bytes())
}
