package main

import (
	"encoding/binary"
	"time"
)

const (
	// dnsResetBody is how much of the answer a TCPRESET name gets before
	// the reset.
	dnsResetBody = 20
	// dnsRetransmit is how long sent answer data may stay unacknowledged
	// before it is sent again from the first unacknowledged byte.
	dnsRetransmit = time.Second
	dnsRetries    = 8
)

// dnsStream is the DNS side of a connection to port 53: the query bytes
// read so far and the framed answer being sent. Offsets count from base,
// the sequence number of the first byte of out.
type dnsStream struct {
	query     []byte
	answered  bool
	out       []byte // length prefix and response
	base      uint32
	una, nxt  int
	max       int // highest offset sent, the FIN counting as len(out)+1
	finSent   bool
	finAcked  bool
	clientFIN bool
	window    uint16 // the client's receive window
	retries   int
	timer     bool
}

// handleDNSTCP takes one segment on an established port 53 connection. It
// runs with h.mu held. The peer answers one query, closes with FIN after
// the answer and forgets the connection once both FINs are acknowledged.
func (h *host) handleDNSTCP(key tcpKey, conn *tcpConn, segment tcpSegment) [][]byte {
	s := conn.dns
	if segment.flags&tcpACK != 0 {
		s.window = segment.window
		if s.answered {
			s.acknowledge(segment.ack)
		}
	}
	received := len(segment.payload) != 0 || segment.flags&tcpFIN != 0
	if received && segment.seq != conn.clientSeq {
		return [][]byte{h.dnsACK(conn)}
	}
	var frames [][]byte
	if len(segment.payload) != 0 {
		conn.clientSeq += uint32(len(segment.payload))
		if !s.answered {
			s.query = append(s.query, segment.payload...)
			if reset := h.dnsTCPQuery(conn); reset != nil {
				delete(h.tcp, key)
				return reset
			}
		}
	}
	if segment.flags&tcpFIN != 0 {
		conn.clientSeq++
		s.clientFIN = true
		if !s.answered {
			s.answered, s.base = true, conn.serverSeq
		}
	}
	if s.answered {
		frames = h.dnsSend(conn)
	}
	if received && len(frames) == 0 {
		frames = append(frames, h.dnsACK(conn))
	}
	if s.finAcked && s.clientFIN {
		delete(h.tcp, key)
		return frames
	}
	h.dnsArm(key, conn)
	return frames
}

// dnsTCPQuery answers the query once its length prefix and body are in. It
// returns the frames that end the connection with a reset, or nil.
func (h *host) dnsTCPQuery(conn *tcpConn) [][]byte {
	s := conn.dns
	if len(s.query) < 2 {
		return nil
	}
	length := int(binary.BigEndian.Uint16(s.query))
	if len(s.query) < 2+length {
		return nil
	}
	query, ok := parseQuery(s.query[2 : 2+length])
	if !ok {
		h.log.printf("dns-tcp-bad %s:%d len=%d", conn.client, conn.clientPort, length)
		return [][]byte{h.tcpFrame(conn, tcpRST|tcpACK, nil)}
	}
	h.log.printf("dns-tcp-query %s %d %s:%d id=%d len=%d", query.logName(), query.qtype, conn.client, conn.clientPort, query.id, length)
	s.answered, s.base = true, conn.serverSeq
	response := buildAnswer(query, h.zone, h.formErr.Load(), true)
	if entry := h.zone.lookup(query.name); entry != nil && entry.kind == kindTCPRESET {
		prefix := binary.BigEndian.AppendUint16(nil, uint16(len(response)))
		frames := [][]byte{
			h.tcpFrame(conn, tcpACK|tcpPSH, prefix),
			h.tcpFrame(conn, tcpACK|tcpPSH, response[:min(dnsResetBody, len(response))]),
			h.tcpFrame(conn, tcpRST|tcpACK, nil),
		}
		h.log.printf("dns-tcp-reset %s %s:%d", query.logName(), conn.client, conn.clientPort)
		return frames
	}
	if response != nil {
		s.out = binary.BigEndian.AppendUint16(nil, uint16(len(response)))
		s.out = append(s.out, response...)
		h.log.printf("dns-tcp-answer %s %s:%d len=%d", query.logName(), conn.client, conn.clientPort, len(response))
	}
	return nil
}

// acknowledge moves the acknowledged offset forward for a valid ACK.
func (s *dnsStream) acknowledge(ack uint32) {
	offset := int(int32(ack - s.base))
	if offset <= s.una || offset > s.max {
		return
	}
	if offset == len(s.out)+1 {
		s.finAcked = true
		offset = len(s.out)
	}
	s.una = offset
	s.nxt = max(s.nxt, s.una)
	s.retries = 0
}

// dnsSend sends what the client's window allows. The length prefix goes in
// a segment of its own, the response in segments of at most the MSS, and
// a FIN follows the last byte.
func (h *host) dnsSend(conn *tcpConn) [][]byte {
	s := conn.dns
	mss := max(1, int(min(conn.mss, tcpPeerMSS)))
	var frames [][]byte
	for s.nxt < len(s.out) && s.nxt < s.una+int(s.window) {
		end := min(len(s.out), s.una+int(s.window), s.nxt+mss)
		if s.nxt < 2 {
			end = min(end, 2)
		}
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(s.nxt), tcpACK|tcpPSH, s.out[s.nxt:end]))
		s.nxt = end
		s.max = max(s.max, end)
	}
	if s.nxt == len(s.out) && !s.finSent {
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(len(s.out)), tcpFIN|tcpACK, nil))
		s.finSent = true
		s.max = len(s.out) + 1
	}
	return frames
}

// dnsACK is a bare ACK at the current send position.
func (h *host) dnsACK(conn *tcpConn) []byte {
	s := conn.dns
	if !s.answered {
		return h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil)
	}
	seq := s.base + uint32(s.nxt)
	if s.finSent {
		seq++
	}
	return h.tcpFrameAt(conn, seq, tcpACK, nil)
}

// dnsArm starts the retransmit timer while answer data or the FIN is
// unacknowledged. When it fires, sending restarts at the first
// unacknowledged byte; after dnsRetries rounds the connection is dropped.
func (h *host) dnsArm(key tcpKey, conn *tcpConn) {
	s := conn.dns
	if !s.answered || s.finAcked || s.timer || h.after == nil {
		return
	}
	s.timer = true
	h.after(dnsRetransmit, func() [][]byte {
		h.mu.Lock()
		defer h.mu.Unlock()
		s.timer = false
		if h.tcp[key] != conn || s.finAcked {
			return nil
		}
		if s.retries++; s.retries > dnsRetries {
			delete(h.tcp, key)
			h.log.printf("dns-tcp-timeout %s:%d", conn.client, conn.clientPort)
			return nil
		}
		s.nxt, s.finSent = s.una, false
		frames := h.dnsSend(conn)
		h.dnsArm(key, conn)
		return frames
	})
}
