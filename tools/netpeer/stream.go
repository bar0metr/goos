package main

import "time"

const (
	// streamRetransmit is how long sent data may stay unacknowledged
	// before it is sent again from the first unacknowledged byte.
	streamRetransmit = time.Second
	streamRetries    = 8
)

// txStream is one fixed answer sent within the receiver's window and
// retransmitted from the first unacknowledged byte, then closed with FIN.
// Offsets count from base, the sequence number of the first byte of out.
type txStream struct {
	out      []byte
	base     uint32
	una, nxt int
	max      int // highest offset sent, the FIN counting as len(out)+1
	finSent  bool
	finAcked bool
	retries  int
	timer    bool
}

// acknowledge moves the acknowledged offset forward for a valid ACK.
func (s *txStream) acknowledge(ack uint32) {
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

// streamSend sends what window allows in segments of at most the MSS, and
// a FIN after the last byte. It leaves conn.serverSeq at the next sequence
// number, so bare ACKs carry it.
func (h *host) streamSend(conn *tcpConn, s *txStream, window uint16) [][]byte {
	mss := max(1, int(min(conn.mss, tcpPeerMSS)))
	var frames [][]byte
	for s.nxt < len(s.out) && s.nxt < s.una+int(window) {
		end := min(len(s.out), s.una+int(window), s.nxt+mss)
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(s.nxt), tcpACK|tcpPSH, s.out[s.nxt:end]))
		s.nxt = end
		s.max = max(s.max, end)
	}
	if s.nxt == len(s.out) && !s.finSent {
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(len(s.out)), tcpFIN|tcpACK, nil))
		s.finSent = true
		s.max = len(s.out) + 1
	}
	conn.serverSeq = s.base + uint32(s.nxt)
	if s.finSent && s.nxt == len(s.out) {
		conn.serverSeq++
	}
	return frames
}

// streamArm starts the retransmit timer while data or the FIN is
// unacknowledged; after streamRetries rounds the connection is dropped.
// window reports the receiver's current window when the timer fires.
func (h *host) streamArm(key tcpKey, conn *tcpConn, s *txStream, window func() uint16) {
	if s.finAcked || s.timer || h.after == nil {
		return
	}
	s.timer = true
	h.after(streamRetransmit, func() [][]byte {
		h.mu.Lock()
		defer h.mu.Unlock()
		s.timer = false
		if h.tcp[key] != conn || s.finAcked {
			return nil
		}
		if s.retries++; s.retries > streamRetries {
			delete(h.tcp, key)
			h.log.printf("tcp-timeout %s:%d", conn.client, conn.clientPort)
			return nil
		}
		s.nxt, s.finSent = s.una, false
		frames := h.streamSend(conn, s, max(window(), 1))
		h.streamArm(key, conn, s, window)
		return frames
	})
}
