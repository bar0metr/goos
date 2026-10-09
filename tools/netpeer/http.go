package main

import (
	"bytes"
	"strconv"
)

type httpStream struct {
	request  []byte
	answered bool
}

func (h *host) handleHTTP(key tcpKey, conn *tcpConn, segment tcpSegment) [][]byte {
	if len(segment.payload) > 0 {
		if segment.seq != conn.clientSeq {
			return [][]byte{h.tcpFrame(conn, tcpACK, nil)}
		}
		conn.clientSeq += uint32(len(segment.payload))
		if len(conn.http.request)+len(segment.payload) > 8192 {
			delete(h.tcp, key)
			return [][]byte{h.tcpFrame(conn, tcpRST|tcpACK, nil)}
		}
		conn.http.request = append(conn.http.request, segment.payload...)
		ack := h.tcpFrame(conn, tcpACK, nil)
		if conn.http.answered {
			return [][]byte{ack}
		}
		response, route, complete := httpReply(conn.http.request)
		if !complete {
			return [][]byte{ack}
		}
		conn.http.answered = true
		frames := [][]byte{ack}
		for len(response) > 0 {
			n := len(response)
			if n > tcpPeerMSS {
				n = tcpPeerMSS
			}
			frames = append(frames, h.tcpFrame(conn, tcpACK|tcpPSH, response[:n]))
			response = response[n:]
		}
		frames = append(frames, h.tcpFrame(conn, tcpFIN|tcpACK, nil))
		conn.closing = true
		h.log.printf("http %s:%d %s", conn.client, conn.clientPort, route)
		return frames
	}
	if segment.flags&tcpFIN != 0 && segment.seq == conn.clientSeq {
		conn.clientSeq++
		ack := h.tcpFrame(conn, tcpACK, nil)
		delete(h.tcp, key)
		return [][]byte{ack}
	}
	if conn.closing && segment.flags&tcpACK != 0 && segment.ack == conn.serverSeq {
		delete(h.tcp, key)
	}
	return nil
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
