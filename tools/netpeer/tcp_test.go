package main

import (
	"testing"
	"time"
)

func TestTCPEchoAndClosedPortReset(t *testing.T) {
	serverIP, clientIP := ipv4{192, 0, 2, 53}, ipv4{192, 0, 2, 10}
	serverMAC, clientMAC := mac{0x52, 0x54, 0, 0, 2, 53}, mac{0x52, 0x54, 0, 0, 2, 10}
	h := newHost(config{ip: serverIP, mac: serverMAC}, nil)
	syn := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 100, 0, tcpSYN, 16384, nil)
	replies := h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 1, syn))
	segment := tcpFromFrame(t, replies)
	if segment.flags != tcpSYN|tcpACK || segment.ack != 101 || segment.mss != tcpPeerMSS {
		t.Fatalf("SYN+ACK %+v", segment)
	}
	ack := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 101, segment.seq+1, tcpACK, 16384, nil)
	if got := h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 2, ack)); len(got) != 0 {
		t.Fatalf("ACK produced %d frames", len(got))
	}
	data := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 101, segment.seq+1, tcpACK|tcpPSH, 16384, []byte("hello"))
	echo := tcpFromFrame(t, h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 3, data)))
	if echo.flags != tcpACK|tcpPSH || echo.ack != 106 || string(echo.payload) != "hello" {
		t.Fatalf("echo %+v %q", echo, echo.payload)
	}
	reordered := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 106, echo.seq+5, tcpACK|tcpPSH, 16384, []byte("reorder"))
	frames := h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 4, reordered))
	if len(frames) != 2 {
		t.Fatalf("reorder frames %d", len(frames))
	}
	second, first := tcpFrameAt(t, frames[0]), tcpFrameAt(t, frames[1])
	if second.seq != first.seq+uint32(len(first.payload)) || string(first.payload)+string(second.payload) != "reorder" {
		t.Fatalf("reordered first=%+v %q second=%+v %q", first, first.payload, second, second.payload)
	}
	duplicate := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 113, second.seq+uint32(len(second.payload)), tcpACK|tcpPSH, 16384, []byte("duplicate"))
	frames = h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 5, duplicate))
	if len(frames) != 2 || string(tcpFrameAt(t, frames[0]).payload) != "duplicate" || string(tcpFrameAt(t, frames[1]).payload) != "duplicate" {
		t.Fatalf("duplicate frames %d", len(frames))
	}
	closed := buildTCP(clientIP, serverIP, 50001, 9, 7, 0, tcpSYN, 1024, nil)
	reset := tcpFromFrame(t, h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 6, closed)))
	if reset.flags != tcpRST|tcpACK || reset.ack != 8 {
		t.Fatalf("reset %+v", reset)
	}
}

func TestTCPZeroWindowReopens(t *testing.T) {
	serverIP, clientIP := ipv4{192, 0, 2, 53}, ipv4{192, 0, 2, 10}
	serverMAC, clientMAC := mac{0x52, 0x54, 0, 0, 2, 53}, mac{0x52, 0x54, 0, 0, 2, 10}
	h := newHost(config{ip: serverIP, mac: serverMAC}, nil)
	h.tcpZeroWindow = 2 * slowAnswer
	var delayed [][]byte
	h.later = func(_ time.Duration, frames [][]byte) { delayed = frames }
	syn := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 100, 0, tcpSYN, 16384, nil)
	synack := tcpFromFrame(t, h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 1, syn)))
	if synack.window != 0 {
		t.Fatalf("SYN+ACK window %d", synack.window)
	}
	ack := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 101, synack.seq+1, tcpACK, 16384, nil)
	h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 2, ack))
	if len(delayed) != 1 || tcpFrameAt(t, delayed[0]).window != 16384 {
		t.Fatalf("window update frames %d", len(delayed))
	}
	probe := buildTCP(clientIP, serverIP, 50000, tcpEchoPort, 101, synack.seq+1, tcpACK|tcpPSH, 16384, []byte("x"))
	reply := tcpFromFrame(t, h.handleIPv4(clientMAC, buildIPv4(clientIP, serverIP, 6, 3, probe)))
	if reply.window != 0 || reply.ack != 101 || len(reply.payload) != 0 {
		t.Fatalf("zero-window reply %+v", reply)
	}
}

func tcpFromFrame(t *testing.T, frames [][]byte) tcpSegment {
	t.Helper()
	if len(frames) != 1 || len(frames[0]) < 14 {
		t.Fatalf("frames %d", len(frames))
	}
	return tcpFrameAt(t, frames[0])
}

func tcpFrameAt(t *testing.T, frame []byte) tcpSegment {
	t.Helper()
	if len(frame) < 14 {
		t.Fatalf("frame length %d", len(frame))
	}
	packet, ok := parseIPv4(frame[14:])
	if !ok {
		t.Fatal("bad IPv4")
	}
	segment, ok := parseTCP(packet)
	if !ok {
		t.Fatal("bad TCP")
	}
	return segment
}
