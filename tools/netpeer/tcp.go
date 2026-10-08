package main

import (
	"encoding/binary"
	"strings"
	"time"
)

const (
	tcpEchoPort = 7
	tcpDNSPort  = 53
	tcpFIN      = 0x01
	tcpSYN      = 0x02
	tcpRST      = 0x04
	tcpPSH      = 0x08
	tcpACK      = 0x10
	tcpPeerMSS  = 1200
)

type tcpSegment struct {
	srcPort, dstPort uint16
	seq, ack         uint32
	flags            byte
	window           uint16
	mss              uint16
	payload          []byte
}

func parseTCP(packet ipPacket) (tcpSegment, bool) {
	data := packet.payload
	if len(data) < 20 {
		return tcpSegment{}, false
	}
	header := int(data[12]>>4) * 4
	if header < 20 || header > len(data) || data[12]&1 != 0 || data[13]&0xc0 != 0 {
		return tcpSegment{}, false
	}
	sum := pseudoSum(packet.src, packet.dst, 6, len(data))
	if checksum(data, sum) != 0 {
		return tcpSegment{}, false
	}
	segment := tcpSegment{srcPort: binary.BigEndian.Uint16(data[0:2]), dstPort: binary.BigEndian.Uint16(data[2:4]),
		seq: binary.BigEndian.Uint32(data[4:8]), ack: binary.BigEndian.Uint32(data[8:12]), flags: data[13] & 0x3f,
		window: binary.BigEndian.Uint16(data[14:16]), mss: 536, payload: data[header:]}
	if segment.srcPort == 0 || segment.dstPort == 0 {
		return tcpSegment{}, false
	}
	for offset := 20; offset < header; {
		kind := data[offset]
		if kind == 0 {
			break
		}
		if kind == 1 {
			offset++
			continue
		}
		if offset+2 > header || data[offset+1] < 2 || offset+int(data[offset+1]) > header {
			return tcpSegment{}, false
		}
		length := int(data[offset+1])
		if kind == 2 {
			if length != 4 {
				return tcpSegment{}, false
			}
			segment.mss = binary.BigEndian.Uint16(data[offset+2 : offset+4])
		}
		offset += length
	}
	return segment, true
}

func pseudoSum(src, dst ipv4, protocol byte, length int) uint32 {
	sum := uint32(src[0])<<8 | uint32(src[1])
	sum += uint32(src[2])<<8 | uint32(src[3])
	sum += uint32(dst[0])<<8 | uint32(dst[1])
	sum += uint32(dst[2])<<8 | uint32(dst[3])
	return sum + uint32(protocol) + uint32(length)
}

func buildTCP(src, dst ipv4, srcPort, dstPort uint16, seq, ack uint32, flags byte, window uint16, payload []byte) []byte {
	header := 20
	if flags&tcpSYN != 0 {
		header = 24
	}
	segment := make([]byte, header+len(payload))
	binary.BigEndian.PutUint16(segment[0:2], srcPort)
	binary.BigEndian.PutUint16(segment[2:4], dstPort)
	binary.BigEndian.PutUint32(segment[4:8], seq)
	binary.BigEndian.PutUint32(segment[8:12], ack)
	segment[12], segment[13] = byte(header/4)<<4, flags
	binary.BigEndian.PutUint16(segment[14:16], window)
	if flags&tcpSYN != 0 {
		segment[20], segment[21] = 2, 4
		binary.BigEndian.PutUint16(segment[22:24], tcpPeerMSS)
	}
	copy(segment[header:], payload)
	binary.BigEndian.PutUint16(segment[16:18], checksum(segment, pseudoSum(src, dst, 6, len(segment))))
	return segment
}

type tcpKey struct {
	address ipv4
	port    uint16
	local   uint16
}
type tcpConn struct {
	client               ipv4
	clientMAC            mac
	clientPort           uint16
	localPort            uint16
	mss                  uint16
	serverSeq, clientSeq uint32
	established          bool
	closing              bool
	window               uint16
	reopenAfter          time.Duration
	openAt               time.Time
	dns                  *dnsStream
}

func (h *host) tcpFrame(conn *tcpConn, flags byte, payload []byte) []byte {
	frame := h.tcpFrameAt(conn, conn.serverSeq, flags, payload)
	if flags&tcpSYN != 0 || flags&tcpFIN != 0 {
		conn.serverSeq++
	}
	conn.serverSeq += uint32(len(payload))
	return frame
}

// tcpFrameAt builds a segment at seq without moving the send sequence.
func (h *host) tcpFrameAt(conn *tcpConn, seq uint32, flags byte, payload []byte) []byte {
	tcp := buildTCP(h.ip, conn.client, conn.localPort, conn.clientPort, seq, conn.clientSeq, flags, conn.window, payload)
	return etherFrame(conn.clientMAC, h.mac, etherTypeIPv4, buildIPv4(h.ip, conn.client, 6, h.ipID(), tcp))
}

func (h *host) handleTCP(src mac, packet ipPacket) [][]byte {
	segment, ok := parseTCP(packet)
	if !ok || packet.dst != h.ip {
		return nil
	}
	key := tcpKey{packet.src, segment.srcPort, segment.dstPort}
	h.mu.Lock()
	if h.tcp == nil {
		h.tcp = map[tcpKey]*tcpConn{}
	}
	conn := h.tcp[key]
	if segment.flags&tcpRST != 0 {
		delete(h.tcp, key)
		h.mu.Unlock()
		return nil
	}
	if conn == nil {
		if segment.flags&tcpSYN == 0 || segment.dstPort != tcpEchoPort && segment.dstPort != tcpDNSPort {
			h.mu.Unlock()
			ack := segment.seq + uint32(len(segment.payload))
			if segment.flags&tcpSYN != 0 {
				ack++
			}
			if segment.flags&tcpFIN != 0 {
				ack++
			}
			flags, seq := byte(tcpRST|tcpACK), uint32(0)
			if segment.flags&tcpACK != 0 {
				flags, seq, ack = tcpRST, segment.ack, 0
			}
			reset := buildTCP(h.ip, packet.src, segment.dstPort, segment.srcPort, seq, ack, flags, 0, nil)
			return [][]byte{etherFrame(src, h.mac, etherTypeIPv4, buildIPv4(h.ip, packet.src, 6, h.ipID(), reset))}
		}
		window := uint16(16384)
		reopenAfter := h.tcpZeroWindow
		if reopenAfter != 0 {
			window = 0
			h.tcpZeroWindow = 0
		}
		conn = &tcpConn{client: packet.src, clientMAC: src, clientPort: segment.srcPort,
			localPort: segment.dstPort, mss: segment.mss,
			serverSeq: 0x40000000 + h.nextID.Add(1)*4096, clientSeq: segment.seq + 1,
			window: window, reopenAfter: reopenAfter}
		if conn.localPort == tcpDNSPort {
			conn.dns = &dnsStream{window: segment.window}
		}
		h.tcp[key] = conn
		if h.tcpDropSYN {
			h.tcpDropSYN = false
			h.mu.Unlock()
			h.log.printf("tcp-drop syn %s:%d", packet.src, segment.srcPort)
			return nil
		}
		frame := h.tcpFrame(conn, tcpSYN|tcpACK, nil)
		delay := h.tcpDelaySYNACK
		h.tcpDelaySYNACK = 0
		h.mu.Unlock()
		if conn.dns != nil {
			h.log.printf("dns-tcp-accept %s:%d", packet.src, segment.srcPort)
		} else {
			h.log.printf("tcp-syn %s:%d mss=%d", packet.src, segment.srcPort, segment.mss)
		}
		if delay != 0 && h.later != nil {
			h.later(delay, [][]byte{frame})
			return nil
		}
		return [][]byte{frame}
	}
	if !conn.established {
		if segment.flags&tcpSYN != 0 && segment.seq+1 == conn.clientSeq {
			frame := h.tcpFrameAt(conn, conn.serverSeq-1, tcpSYN|tcpACK, nil)
			h.mu.Unlock()
			return [][]byte{frame}
		}
		if segment.flags&tcpACK != 0 && segment.ack == conn.serverSeq && segment.seq == conn.clientSeq {
			conn.established = true
			if conn.reopenAfter != 0 && h.later != nil {
				conn.openAt = time.Now().Add(conn.reopenAfter)
				tcp := buildTCP(h.ip, conn.client, conn.localPort, conn.clientPort, conn.serverSeq, conn.clientSeq, tcpACK, 16384, nil)
				update := etherFrame(conn.clientMAC, h.mac, etherTypeIPv4, buildIPv4(h.ip, conn.client, 6, h.ipID(), tcp))
				h.later(conn.reopenAfter, [][]byte{update})
				conn.reopenAfter = 0
			}
		}
		h.mu.Unlock()
		return nil
	}
	if conn.dns != nil {
		frames := h.handleDNSTCP(key, conn, segment)
		h.mu.Unlock()
		return frames
	}
	if conn.closing && len(segment.payload) == 0 && segment.flags&tcpFIN == 0 {
		if segment.flags&tcpACK != 0 && segment.ack == conn.serverSeq {
			delete(h.tcp, key)
		}
		h.mu.Unlock()
		return nil
	}
	if conn.window == 0 && !conn.openAt.IsZero() && !time.Now().Before(conn.openAt) {
		conn.window = 16384
		conn.openAt = time.Time{}
	}
	if len(segment.payload) != 0 {
		if conn.window == 0 {
			frame := h.tcpFrame(conn, tcpACK, nil)
			h.mu.Unlock()
			h.log.printf("tcp-zero-probe %s:%d", packet.src, segment.srcPort)
			return [][]byte{frame}
		}
		if segment.seq != conn.clientSeq {
			frame := h.tcpFrame(conn, tcpACK, nil)
			h.mu.Unlock()
			return [][]byte{frame}
		}
		if h.tcpDropData {
			h.tcpDropData = false
			h.mu.Unlock()
			h.log.printf("tcp-drop data %s:%d", packet.src, segment.srcPort)
			return nil
		}
		conn.clientSeq += uint32(len(segment.payload))
		payload := append([]byte(nil), segment.payload...)
		if strings.HasPrefix(string(payload), "reset") {
			frame := h.tcpFrame(conn, tcpRST|tcpACK, nil)
			delete(h.tcp, key)
			h.mu.Unlock()
			return [][]byte{frame}
		}
		frame := h.tcpFrame(conn, tcpACK|tcpPSH, payload)
		delay := time.Duration(0)
		if strings.HasPrefix(string(payload), "delay") {
			delay = time.Second
		}
		frames := [][]byte{frame}
		if strings.HasPrefix(string(payload), "reorder") && len(payload) > 1 {
			conn.serverSeq -= uint32(len(payload))
			firstLength := len(payload) / 2
			first := h.tcpFrame(conn, tcpACK|tcpPSH, payload[:firstLength])
			second := h.tcpFrame(conn, tcpACK|tcpPSH, payload[firstLength:])
			frames = [][]byte{second, first}
		} else if strings.HasPrefix(string(payload), "duplicate") {
			frames = [][]byte{frame, append([]byte(nil), frame...)}
		}
		h.mu.Unlock()
		h.log.printf("tcp-echo %s:%d %d", packet.src, segment.srcPort, len(payload))
		if delay != 0 && h.later != nil {
			h.later(delay, frames)
			return nil
		}
		return frames
	}
	if segment.flags&tcpFIN != 0 && segment.seq == conn.clientSeq {
		conn.clientSeq++
		ack := h.tcpFrame(conn, tcpACK, nil)
		fin := h.tcpFrame(conn, tcpFIN|tcpACK, nil)
		conn.closing = true
		h.mu.Unlock()
		return [][]byte{ack, fin}
	}
	h.mu.Unlock()
	return nil
}
