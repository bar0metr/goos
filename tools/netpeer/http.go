package main

import (
	"bytes"
	"strconv"
	"strings"
)

type httpStream struct {
	request   []byte
	window    uint16 // the client's receive window
	tx        *txStream
	clientFIN bool
}

// handleHTTP takes one segment on an established HTTP connection. It runs
// with h.mu held. The peer answers one request within the client's window,
// closes with FIN after the answer, and forgets the connection once both
// FINs are acknowledged.
func (h *host) handleHTTP(key tcpKey, conn *tcpConn, segment tcpSegment) [][]byte {
	s := conn.http
	if segment.flags&tcpACK != 0 {
		s.window = segment.window
		if s.tx != nil {
			s.tx.acknowledge(segment.ack)
		}
	}
	received := len(segment.payload) != 0 || segment.flags&tcpFIN != 0
	if received && segment.seq != conn.clientSeq {
		return [][]byte{h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil)}
	}
	if len(segment.payload) != 0 {
		conn.clientSeq += uint32(len(segment.payload))
		if s.tx == nil {
			if len(s.request)+len(segment.payload) > 8192 {
				delete(h.tcp, key)
				return [][]byte{h.tcpFrame(conn, tcpRST|tcpACK, nil)}
			}
			s.request = append(s.request, segment.payload...)
			if response, route, complete := h.reply(s.request); complete {
				s.tx = &txStream{out: response, base: conn.serverSeq}
				h.log.printf("http %s:%d %s", conn.client, conn.clientPort, route)
			}
		}
	}
	if segment.flags&tcpFIN != 0 {
		conn.clientSeq++
		s.clientFIN = true
	}
	var frames [][]byte
	if s.tx != nil {
		frames = h.streamSend(conn, s.tx, s.window)
	}
	if received && len(frames) == 0 {
		frames = append(frames, h.tcpFrameAt(conn, conn.serverSeq, tcpACK, nil))
	}
	if s.clientFIN && (s.tx == nil || s.tx.finAcked) {
		delete(h.tcp, key)
		return frames
	}
	if s.tx != nil {
		h.streamArm(key, conn, s.tx, func() uint16 { return s.window })
	}
	return frames
}

// reply answers a GET or HEAD for a file named with -http-file, and any
// other request through httpReply.
func (h *host) reply(request []byte) ([]byte, string, bool) {
	end := bytes.Index(request, []byte("\r\n\r\n"))
	lineEnd := bytes.Index(request, []byte("\r\n"))
	if end >= 0 && lineEnd >= 0 {
		fields := bytes.Fields(request[:lineEnd])
		if len(fields) == 3 && (string(fields[0]) == "GET" || string(fields[0]) == "HEAD") {
			if body, ok := h.files[strings.TrimPrefix(string(fields[1]), "/")]; ok {
				header := "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) +
					"\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n"
				response := []byte(header)
				if string(fields[0]) == "GET" {
					response = append(response, body...)
				}
				return response, string(fields[1]), true
			}
		}
	}
	return httpReply(request)
}

func httpReply(request []byte) ([]byte, string, bool) {
	end := bytes.Index(request, []byte("\r\n\r\n"))
	if end < 0 {
		return nil, "", false
	}
	lineEnd := bytes.Index(request, []byte("\r\n"))
	if lineEnd < 0 {
		return nil, "", false
	}
	fields := bytes.Fields(request[:lineEnd])
	if len(fields) != 3 {
		return []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"), "bad", true
	}
	method, path := string(fields[0]), string(fields[1])
	length := 0
	headerStart := lineEnd + 2
	if headerStart > end {
		headerStart = end
	}
	for _, line := range bytes.Split(request[headerStart:end], []byte("\r\n")) {
		if bytes.HasPrefix(bytes.ToLower(line), []byte("content-length:")) {
			n, err := strconv.Atoi(string(bytes.TrimSpace(line[15:])))
			if err != nil || n < 0 || n > 4096 {
				return []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"), path, true
			}
			length = n
		}
	}
	if len(request) < end+4+length {
		return nil, path, false
	}
	if method == "HEAD" {
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Type: text/plain\r\n\r\n"), path, true
	}
	switch path {
	case "/hello":
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Type: text/plain\r\n\r\nhello"), path, true
	case "/post":
		return append([]byte("HTTP/1.1 200 OK\r\nContent-Length: "+strconv.Itoa(length)+"\r\n\r\n"), request[end+4:end+4+length]...), path, true
	case "/headers":
		body := "missing"
		if bytes.Contains(bytes.ToLower(request[:end]), []byte("x-test: goos")) {
			body = "goos"
		}
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body), path, true
	case "/chunked":
		return []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5;test=1\r\nhello\r\n0\r\nX-End: yes\r\n\r\n"), path, true
	case "/redirect":
		return []byte("HTTP/1.1 302 Found\r\nLocation: /hello\r\nContent-Length: 0\r\n\r\n"), path, true
	default:
		return []byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"), path, true
	}
}
