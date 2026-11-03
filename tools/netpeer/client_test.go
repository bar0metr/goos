package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

var (
	streamPeerIP   = ipv4{192, 0, 2, 53}
	streamPeerMAC  = mac{0x52, 0x54, 0, 0, 2, 53}
	streamGuestIP  = ipv4{192, 0, 2, 10}
	streamGuestMAC = mac{0x52, 0x54, 0, 0, 2, 10}
)

func streamHost(log *bytes.Buffer) *host {
	return newHost(config{ip: streamPeerIP, mac: streamPeerMAC, files: map[string][]byte{}}, &logger{w: log})
}

// fromGuest hands the peer one segment the guest sends.
func fromGuest(h *host, srcPort, dstPort uint16, seq, ack uint32, flags byte, window uint16, payload []byte) [][]byte {
	segment := buildTCP(streamGuestIP, streamPeerIP, srcPort, dstPort, seq, ack, flags, window, payload)
	return h.handleIPv4(streamGuestMAC, buildIPv4(streamGuestIP, streamPeerIP, 6, 1, segment))
}

func TestHTTPFileRespectsTheWindow(t *testing.T) {
	var log bytes.Buffer
	h := streamHost(&log)
	file := make([]byte, 10000)
	for i := range file {
		file[i] = byte(i * 7)
	}
	h.files["big"] = file
	const window = 3000
	synack := tcpFromFrame(t, fromGuest(h, 50000, tcpHTTPAltPort, 100, 0, tcpSYN, window, nil))
	next := synack.seq + 1
	if got := fromGuest(h, 50000, tcpHTTPAltPort, 101, next, tcpACK, window, nil); len(got) != 0 {
		t.Fatalf("ACK produced %d frames", len(got))
	}
	request := []byte("GET /big HTTP/1.1\r\nHost: http.test\r\n\r\n")
	clientSeq := uint32(101 + len(request))
	frames := fromGuest(h, 50000, tcpHTTPAltPort, 101, next, tcpACK|tcpPSH, window, request)
	var answer []byte
	fin := false
	for round := 0; round < 100 && !fin; round++ {
		inFlight := 0
		for _, frame := range frames {
			segment := tcpFrameAt(t, frame)
			if len(segment.payload) != 0 {
				if segment.seq != next {
					t.Fatalf("segment at %d, want %d", segment.seq, next)
				}
				answer = append(answer, segment.payload...)
				next += uint32(len(segment.payload))
				inFlight += len(segment.payload)
			}
			if segment.flags&tcpFIN != 0 {
				fin = true
				next++
			}
		}
		if inFlight > window {
			t.Fatalf("%d bytes in flight past a %d-byte window", inFlight, window)
		}
		frames = fromGuest(h, 50000, tcpHTTPAltPort, clientSeq, next, tcpACK, window, nil)
	}
	if !fin {
		t.Fatal("no FIN")
	}
	_, body, found := bytes.Cut(answer, []byte("\r\n\r\n"))
	if !found || !bytes.Equal(body, file) || !bytes.Contains(answer, []byte("Content-Length: 10000\r\n")) {
		t.Fatalf("answer of %d bytes is not the file", len(answer))
	}
	fromGuest(h, 50000, tcpHTTPAltPort, clientSeq, next, tcpACK|tcpFIN, window, nil)
	if len(h.tcp) != 0 {
		t.Fatalf("%d connections left", len(h.tcp))
	}
	if !strings.Contains(log.String(), "http 192.0.2.10:50000 /big") {
		t.Fatalf("log %q", log.String())
	}
}

func TestSilentPortDropsSYN(t *testing.T) {
	var log bytes.Buffer
	h := streamHost(&log)
	if frames := fromGuest(h, 50001, tcpSilentPort, 7, 0, tcpSYN, 1024, nil); len(frames) != 0 {
		t.Fatalf("SYN to the silent port drew %d frames", len(frames))
	}
	if !strings.Contains(log.String(), "tcp-silent 192.0.2.10:50001") {
		t.Fatalf("log %q", log.String())
	}
}

// openClient runs a client command and answers its SYN, returning the
// request segments and the guest's next sequence number.
func openClient(t *testing.T, h *host, line string) (tcpSegment, []tcpSegment, uint32) {
	t.Helper()
	h.learn(streamGuestMAC, streamGuestIP)
	frames, _, err := h.command(line)
	if err != nil {
		t.Fatal(err)
	}
	syn := tcpFromFrame(t, frames)
	if syn.flags != tcpSYN || syn.dstPort != 8080 {
		t.Fatalf("SYN %+v", syn)
	}
	frames = fromGuest(h, 8080, syn.srcPort, 5000, syn.seq+1, tcpSYN|tcpACK, 16384, nil)
	var request []tcpSegment
	for _, frame := range frames {
		request = append(request, tcpFrameAt(t, frame))
	}
	return syn, request, 5001
}

func TestClientFetchesFromAGuest(t *testing.T) {
	var log bytes.Buffer
	h := streamHost(&log)
	syn, request, guestSeq := openClient(t, h, "http-get one 192.0.2.10 8080 /file")
	if len(request) != 1 || !strings.HasPrefix(string(request[0].payload), "GET /file HTTP/1.0\r\n") || request[0].ack != guestSeq {
		t.Fatalf("request %+v", request)
	}
	acked := request[0].seq + uint32(len(request[0].payload))
	answer := []byte("HTTP/1.0 200 OK\r\nContent-Length: 4\r\n\r\nbody")
	frames := fromGuest(h, 8080, syn.srcPort, guestSeq, acked, tcpACK|tcpPSH|tcpFIN, 16384, answer)
	if len(frames) != 2 || tcpFrameAt(t, frames[1]).flags&tcpFIN == 0 || tcpFrameAt(t, frames[0]).ack != guestSeq+uint32(len(answer))+1 {
		t.Fatalf("%d frames after the answer", len(frames))
	}
	fromGuest(h, 8080, syn.srcPort, guestSeq+uint32(len(answer))+1, acked+1, tcpACK, 16384, nil)
	if len(h.tcp) != 0 {
		t.Fatalf("%d connections left", len(h.tcp))
	}
	sum := sha256.Sum256([]byte("body"))
	if want := "http-got one status=200 body=4 sha256=" + hex.EncodeToString(sum[:]); !strings.Contains(log.String(), want) {
		t.Fatalf("log %q, want %q", log.String(), want)
	}
}

func TestClientChatHalfClosesAndLogsTheReply(t *testing.T) {
	var log bytes.Buffer
	h := streamHost(&log)
	syn, request, guestSeq := openClient(t, h, "tcp-chat two 192.0.2.10 8080 hello listener")
	if len(request) != 2 || string(request[0].payload) != "hello listener\n" || request[1].flags&tcpFIN == 0 {
		t.Fatalf("chat segments %+v", request)
	}
	finSeq := request[1].seq + 1
	frames := fromGuest(h, 8080, syn.srcPort, guestSeq, finSeq, tcpACK|tcpPSH|tcpFIN, 16384, []byte("from-guest\n"))
	if len(frames) != 1 {
		t.Fatalf("%d frames after the reply", len(frames))
	}
	if len(h.tcp) != 0 {
		t.Fatalf("%d connections left", len(h.tcp))
	}
	if !strings.Contains(log.String(), `tcp-got two bytes=11 text="from-guest\n"`) {
		t.Fatalf("log %q", log.String())
	}
}

func TestClientRefusedAndAborted(t *testing.T) {
	var log bytes.Buffer
	h := streamHost(&log)
	h.learn(streamGuestMAC, streamGuestIP)
	frames, _, err := h.command("http-get refused 192.0.2.10 8080 /")
	if err != nil {
		t.Fatal(err)
	}
	syn := tcpFromFrame(t, frames)
	fromGuest(h, 8080, syn.srcPort, 0, syn.seq+1, tcpRST|tcpACK, 0, nil)
	if !strings.Contains(log.String(), "client-refused refused") || len(h.tcp) != 0 {
		t.Fatalf("log %q connections %d", log.String(), len(h.tcp))
	}
	syn, request, guestSeq := openClient(t, h, "http-abort cut 192.0.2.10 8080 /file")
	acked := request[0].seq + uint32(len(request[0].payload))
	frames = fromGuest(h, 8080, syn.srcPort, guestSeq, acked, tcpACK|tcpPSH, 16384, []byte("HTTP/1.0 200 OK\r\n"))
	if reset := tcpFromFrame(t, frames); reset.flags != tcpRST {
		t.Fatalf("abort sent %+v", reset)
	}
	if !strings.Contains(log.String(), "client-abort cut after=17") || len(h.tcp) != 0 {
		t.Fatalf("log %q connections %d", log.String(), len(h.tcp))
	}
	if _, _, err := h.command("http-get x 192.0.2.99 8080 /"); err == nil {
		t.Fatal("a client to an unknown neighbor started")
	}
}
