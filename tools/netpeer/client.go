package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The peer as a TCP client of a guest server. On command it connects to a
// guest port, sends one request, takes the whole answer until the guest
// closes, and logs one result line:
//
//	http-get TAG ADDRESS PORT PATH    GET PATH; log status, body size, SHA-256
//	http-abort TAG ADDRESS PORT PATH  GET PATH; reset after the first data
//	tcp-chat TAG ADDRESS PORT TEXT    send TEXT and a newline, half-close,
//	                                  and log what comes back
//
// Results are "http-got", "tcp-got", "client-abort", "client-refused",
// "client-reset" and "client-timeout", each followed by TAG.

const (
	// maxServedFile bounds an -http-file and a received answer alike.
	maxServedFile = 16 << 20
	// clientDeadline is how long one client connection may take in all.
	clientDeadline = 30 * time.Second
	// clientWindow is the receive window the client advertises.
	clientWindow = 65535
	// clientPortBase starts the peer's own ports for client connections.
	clientPortBase = 40000
	clientPortSpan = 9000
)

const (
	clientHTTP = iota
	clientAbort
	clientChat
)

type clientStream struct {
	tag      string
	mode     int
	request  []byte
	iss      uint32
	base     uint32 // sequence number of the first request byte
	acked    bool   // the request, and a chat FIN, are acknowledged
	finSent  bool
	finAcked bool
	gotFIN   bool
	answer   []byte
	done     bool
	timer    bool
	deadline time.Time
}

// clientCommand opens a connection for one of the client commands.
func (h *host) clientCommand(fields []string) ([][]byte, error) {
	if len(fields) < 5 {
		return nil, fmt.Errorf("%s needs TAG ADDRESS PORT and a path or text", fields[0])
	}
	address, err := parseIPv4Addr(fields[2])
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(fields[3], 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("invalid port %q", fields[3])
	}
	hw, ok := h.neighbor(address)
	if !ok {
		return nil, fmt.Errorf("no MAC address known for %s", address)
	}
	stream := &clientStream{tag: fields[1], mode: clientHTTP, deadline: time.Now().Add(clientDeadline)}
	switch fields[0] {
	case "http-abort":
		stream.mode = clientAbort
		fallthrough
	case "http-get":
		stream.request = []byte("GET " + fields[4] + " HTTP/1.0\r\nHost: " + fields[2] + "\r\nUser-Agent: netpeer\r\n\r\n")
	case "tcp-chat":
		stream.mode = clientChat
		stream.request = []byte(strings.Join(fields[4:], " ") + "\n")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tcp == nil {
		h.tcp = map[tcpKey]*tcpConn{}
	}
	var key tcpKey
	for range clientPortSpan {
		local := clientPortBase + h.nextClient%clientPortSpan
		h.nextClient++
		key = tcpKey{address, uint16(port), local}
		if h.tcp[key] == nil {
			break
		}
	}
	if h.tcp[key] != nil {
		return nil, fmt.Errorf("no free client port")
	}
	stream.iss = 0x60000000 + h.nextID.Add(1)*65536
	conn := &tcpConn{client: address, clientMAC: hw, clientPort: uint16(port), localPort: key.local,
		mss: 536, serverSeq: stream.iss, window: clientWindow, active: stream}
	h.tcp[key] = conn
	h.log.printf("client-start %s %s:%d", stream.tag, address, port)
	frame := h.tcpFrame(conn, tcpSYN, nil)
	h.clientArm(key, conn)
	return [][]byte{frame}, nil
}

// clientRequest sends the request, and for a chat the FIN after it.
func (h *host) clientRequest(conn *tcpConn) [][]byte {
	s := conn.active
	mss := max(1, int(min(conn.mss, tcpPeerMSS)))
	var frames [][]byte
	for offset := 0; offset < len(s.request); offset += mss {
		end := min(len(s.request), offset+mss)
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(offset), tcpACK|tcpPSH, s.request[offset:end]))
	}
	if s.mode == clientChat {
		frames = append(frames, h.tcpFrameAt(conn, s.base+uint32(len(s.request)), tcpFIN|tcpACK, nil))
		s.finSent = true
	}
	return frames
}

// handleClient takes one segment on a connection the peer opened. It runs
// with h.mu held.
func (h *host) handleClient(key tcpKey, conn *tcpConn, segment tcpSegment) [][]byte {
	s := conn.active
	if !conn.established {
		if segment.flags&(tcpSYN|tcpACK) != tcpSYN|tcpACK || segment.ack != s.iss+1 {
			return nil
		}
		conn.established = true
		conn.clientSeq = segment.seq + 1
		conn.mss = segment.mss
		s.base = conn.serverSeq
		conn.serverSeq += uint32(len(s.request))
		frames := h.clientRequest(conn)
		if s.finSent {
			conn.serverSeq++
		}
		if len(frames) == 0 {
			frames = [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil)}
		}
		return frames
	}
	if segment.flags&tcpACK != 0 && segment.ack == conn.serverSeq {
		s.acked = true
		s.finAcked = s.finAcked || s.finSent
	}
	received := len(segment.payload) != 0 || segment.flags&tcpFIN != 0
	if received && segment.seq != conn.clientSeq {
		return [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil)}
	}
	if len(segment.payload) != 0 {
		if len(s.answer)+len(segment.payload) > maxServedFile {
			delete(h.tcp, key)
			h.clientResult(s, "client-timeout "+s.tag+" bytes="+strconv.Itoa(len(s.answer))+" too-large")
			return [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpRST, nil)}
		}
		s.answer = append(s.answer, segment.payload...)
		conn.clientSeq += uint32(len(segment.payload))
		if s.mode == clientAbort {
			delete(h.tcp, key)
			h.clientResult(s, "client-abort "+s.tag+" after="+strconv.Itoa(len(s.answer)))
			return [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpRST, nil)}
		}
	}
	if segment.flags&tcpFIN != 0 {
		conn.clientSeq++
		s.gotFIN = true
	}
	var frames [][]byte
	if received {
		frames = append(frames, h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil))
	}
	if s.gotFIN && !s.finSent {
		frames = append(frames, h.tcpFrameAt(conn, conn.serverSeq, tcpFIN|tcpACK, nil))
		conn.serverSeq++
		s.finSent = true
	}
	if s.gotFIN {
		h.clientResult(s, h.clientSummary(s))
	}
	if s.gotFIN && s.finAcked {
		delete(h.tcp, key)
	}
	return frames
}

// clientSummary is the result line of a connection the guest closed.
func (h *host) clientSummary(s *clientStream) string {
	if s.mode == clientChat {
		text := string(s.answer)
		if len(text) > 120 {
			text = text[:120]
		}
		return fmt.Sprintf("tcp-got %s bytes=%d text=%q", s.tag, len(s.answer), text)
	}
	status, body := 0, s.answer
	if end := bytes.Index(s.answer, []byte("\r\n\r\n")); end >= 0 {
		fields := bytes.Fields(s.answer[:end])
		if len(fields) >= 2 && bytes.HasPrefix(fields[0], []byte("HTTP/1.")) {
			status, _ = strconv.Atoi(string(fields[1]))
		}
		body = s.answer[end+4:]
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("http-got %s status=%d body=%d sha256=%s", s.tag, status, len(body), hex.EncodeToString(sum[:]))
}

// clientReset logs a reset from the guest. It runs with h.mu held.
func (h *host) clientReset(conn *tcpConn) {
	s := conn.active
	if !conn.established {
		h.clientResult(s, "client-refused "+s.tag)
		return
	}
	h.clientResult(s, "client-reset "+s.tag+" bytes="+strconv.Itoa(len(s.answer)))
}

func (h *host) clientResult(s *clientStream, line string) {
	if s.done {
		return
	}
	s.done = true
	h.log.printf("%s", line)
}

// clientArm resends what is unacknowledged once a second, the SYN, the
// request, or the FIN, and gives up at the deadline with a reset.
func (h *host) clientArm(key tcpKey, conn *tcpConn) {
	s := conn.active
	if s.timer || h.after == nil {
		return
	}
	s.timer = true
	h.after(streamRetransmit, func() [][]byte {
		h.mu.Lock()
		defer h.mu.Unlock()
		s.timer = false
		if h.tcp[key] != conn {
			return nil
		}
		if time.Now().After(s.deadline) {
			delete(h.tcp, key)
			h.clientResult(s, "client-timeout "+s.tag+" bytes="+strconv.Itoa(len(s.answer)))
			return [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpRST, nil)}
		}
		var frames [][]byte
		switch {
		case !conn.established:
			frames = [][]byte{h.tcpFrameAt(conn, s.iss, tcpSYN, nil)}
		case !s.acked:
			frames = h.clientRequest(conn)
		case s.finSent && !s.finAcked:
			frames = [][]byte{h.tcpFrameAt(conn, conn.serverSeq-1, tcpFIN|tcpACK, nil)}
		}
		h.clientArm(key, conn)
		return frames
	})
}
