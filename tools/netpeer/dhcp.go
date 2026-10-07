package main

import (
	"encoding/binary"
	"strconv"
)

const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpAck      = 5
	dhcpNak      = 6
	dhcpRelease  = 7
	dhcpInform   = 8
)

var dhcpTypeNames = map[byte]string{
	dhcpDiscover: "DISCOVER", dhcpOffer: "OFFER", dhcpRequest: "REQUEST", dhcpDecline: "DECLINE",
	dhcpAck: "ACK", dhcpNak: "NAK", dhcpRelease: "RELEASE", dhcpInform: "INFORM",
}

var dhcpCookie = [4]byte{99, 130, 83, 99}

const dhcpBroadcastFlag = 0x8000

type dhcpMessage struct {
	xid          uint32
	flags        uint16
	ciaddr       ipv4
	giaddr       ipv4
	chaddr       mac
	msgType      byte
	requested    ipv4
	hasRequested bool
	serverID     ipv4
	hasServerID  bool
}

// address is the address a client message is about: the requested address
// if it names one, else the client's own address.
func (m dhcpMessage) address() ipv4 {
	if m.hasRequested {
		return m.requested
	}
	return m.ciaddr
}

// parseDHCP accepts a BOOTREQUEST for Ethernet with the DHCP cookie and a
// message type option.
func parseDHCP(data []byte) (dhcpMessage, bool) {
	if len(data) < 240 || data[0] != 1 || data[1] != 1 || data[2] != 6 || [4]byte(data[236:240]) != dhcpCookie {
		return dhcpMessage{}, false
	}
	var m dhcpMessage
	m.xid = binary.BigEndian.Uint32(data[4:8])
	m.flags = binary.BigEndian.Uint16(data[10:12])
	copy(m.ciaddr[:], data[12:16])
	copy(m.giaddr[:], data[24:28])
	copy(m.chaddr[:], data[28:34])
	options := data[240:]
	for i := 0; i < len(options); {
		code := options[i]
		if code == 255 {
			break
		}
		if code == 0 {
			i++
			continue
		}
		if i+1 >= len(options) {
			return dhcpMessage{}, false
		}
		length := int(options[i+1])
		if i+2+length > len(options) {
			return dhcpMessage{}, false
		}
		value := options[i+2 : i+2+length]
		switch {
		case code == 53 && length == 1:
			m.msgType = value[0]
		case code == 50 && length == 4:
			copy(m.requested[:], value)
			m.hasRequested = true
		case code == 54 && length == 4:
			copy(m.serverID[:], value)
			m.hasServerID = true
		}
		i += 2 + length
	}
	if m.msgType == 0 {
		return dhcpMessage{}, false
	}
	return m, true
}

func dhcpTypeName(t byte) string {
	if name, ok := dhcpTypeNames[t]; ok {
		return name
	}
	return "TYPE" + strconv.Itoa(int(t))
}

// handleDHCP serves static leases by client MAC. It logs every client
// message and every reply; messages from unknown MACs get no reply.
func (h *host) handleDHCP(data []byte) [][]byte {
	m, ok := parseDHCP(data)
	if !ok {
		return nil
	}
	h.log.printf("dhcp %s %s %s", dhcpTypeName(m.msgType), m.chaddr, m.address())
	lease, known := h.leases[m.chaddr]
	if !known {
		return nil
	}
	var replyType byte
	switch m.msgType {
	case dhcpDiscover:
		replyType = dhcpOffer
	case dhcpRequest:
		if m.hasServerID && m.serverID != h.ip {
			return nil
		}
		// Selecting and init-reboot name the address in option 50;
		// renewing and rebinding carry it in ciaddr.
		if m.address() == lease {
			replyType = dhcpAck
		} else {
			replyType = dhcpNak
		}
	default:
		return nil
	}
	payload := h.dhcpReply(m, replyType, lease)
	logged := lease
	if replyType == dhcpNak {
		logged = m.address()
	}
	h.log.printf("dhcp %s %s %s", dhcpTypeName(replyType), m.chaddr, logged)

	dstIP, dstMAC := broadcastIPv4, broadcastMAC
	if replyType != dhcpNak && m.flags&dhcpBroadcastFlag == 0 && m.ciaddr != (ipv4{}) {
		dstIP, dstMAC = m.ciaddr, m.chaddr
	}
	udp := buildUDP(h.ip, dstIP, 67, 68, payload)
	ip := buildIPv4(h.ip, dstIP, 17, h.ipID(), udp)
	return [][]byte{etherFrame(dstMAC, h.mac, etherTypeIPv4, ip)}
}

// dhcpReply builds a BOOTREPLY. OFFER and ACK carry options 53, 54, 51, 1,
// 3 when a router is set, and 6; NAK carries 53 and 54.
func (h *host) dhcpReply(m dhcpMessage, replyType byte, lease ipv4) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 2, 1, 6
	binary.BigEndian.PutUint32(b[4:8], m.xid)
	binary.BigEndian.PutUint16(b[10:12], m.flags)
	if replyType == dhcpAck {
		copy(b[12:16], m.ciaddr[:])
	}
	if replyType != dhcpNak {
		copy(b[16:20], lease[:])
	}
	copy(b[24:28], m.giaddr[:])
	copy(b[28:34], m.chaddr[:])
	copy(b[236:240], dhcpCookie[:])
	b = append(b, 53, 1, replyType)
	b = append(b, 54, 4)
	b = append(b, h.ip[:]...)
	if replyType != dhcpNak {
		b = append(b, 51, 4)
		b = binary.BigEndian.AppendUint32(b, h.leaseTime)
		b = append(b, 1, 4)
		b = append(b, h.mask[:]...)
		if h.hasRouter {
			b = append(b, 3, 4)
			b = append(b, h.router[:]...)
		}
		if len(h.dns) > 0 {
			b = append(b, 6, byte(4*len(h.dns)))
			for _, server := range h.dns {
				b = append(b, server[:]...)
			}
		}
	}
	b = append(b, 255)
	for len(b) < 300 {
		b = append(b, 0)
	}
	return b
}
