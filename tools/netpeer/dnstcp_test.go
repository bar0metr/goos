package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

const tcpZone = `tcp-fallback.test 60 TCP 192.0.2.14 192.0.2.15
tcp-fallback-reset.test TCPRESET
host.test 300 A 192.0.2.80
`

func tcpZoneHost(t *testing.T, log *bytes.Buffer) *host {
	t.Helper()
	z, err := parseZone(strings.NewReader(tcpZone))
	if err != nil {
		t.Fatal(err)
	}
	return newHost(config{mac: peerMAC, ip: peerIP, mask: ipv4{255, 255, 255, 0}, zone: z}, &logger{w: log})
}

// dnsClient is the guest end of one connection to port 53.
type dnsClient struct {
	t        *testing.T
	h        *host
	port     uint16
	seq, ack uint32
	window   uint16
	ipID     uint16
}

func (c *dnsClient) send(flags byte, payload []byte) []tcpSegment {
	c.t.Helper()
	c.ipID++
	tcp := buildTCP(guestIP, peerIP, c.port, tcpDNSPort, c.seq, c.ack, flags, c.window, payload)
	var segments []tcpSegment
	for _, frame := range c.h.handleIPv4(guestMAC, buildIPv4(guestIP, peerIP, 6, c.ipID, tcp)) {
		segment := tcpFrameAt(c.t, frame)
		if segment.srcPort != tcpDNSPort || segment.dstPort != c.port {
			c.t.Fatalf("segment ports %d -> %d", segment.srcPort, segment.dstPort)
		}
		segments = append(segments, segment)
	}
	c.seq += uint32(len(payload))
	if flags&(tcpSYN|tcpFIN) != 0 {
		c.seq++
	}
	return segments
}

// dial connects with the given MSS in the SYN.
func dial(t *testing.T, h *host, port, mss, window uint16) *dnsClient {
	t.Helper()
	c := &dnsClient{t: t, h: h, port: port, seq: 1000, window: window}
	syn := buildTCP(guestIP, peerIP, port, tcpDNSPort, c.seq, 0, tcpSYN, window, nil)
	binary.BigEndian.PutUint16(syn[22:24], mss)
	syn[16], syn[17] = 0, 0
	binary.BigEndian.PutUint16(syn[16:18], checksum(syn, pseudoSum(guestIP, peerIP, 6, len(syn))))
	synack := tcpFromFrame(t, h.handleIPv4(guestMAC, buildIPv4(guestIP, peerIP, 6, 1, syn)))
	if synack.flags != tcpSYN|tcpACK || synack.ack != c.seq+1 {
		t.Fatalf("SYN+ACK %+v", synack)
	}
	c.seq++
	c.ack = synack.seq + 1
	if got := c.send(tcpACK, nil); len(got) != 0 {
		t.Fatalf("handshake ACK produced %d segments", len(got))
	}
	return c
}

func framed(message []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...)
}

func TestDNSTCPTruncatesOverUDP(t *testing.T) {
	var log bytes.Buffer
	h := tcpZoneHost(t, &log)
	for _, name := range []string{"tcp-fallback.test", "tcp-fallback-reset.test"} {
		for _, opt := range []bool{false, true} {
			r, _ := ask(t, h.zone, false, name, typeA, opt)
			if r.flags&flagTC == 0 || r.flags&0xf != 0 || len(r.answers)+len(r.authority) != 0 {
				t.Fatalf("%s opt=%v %+v", name, opt, r)
			}
			if opt && (len(r.additional) != 1 || r.additional[0].rtype != typeOPT) || !opt && len(r.additional) != 0 {
				t.Fatalf("%s opt=%v additional %+v", name, opt, r.additional)
			}
		}
	}
	query := dnsQueryBytes(9, "tcp-fallback.test", typeA, false)
	frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 40000, 53, query))
	_, datagram := udpOut(t, one(t, h.handle(frame)))
	if r := readResponse(t, datagram.payload); r.id != 9 || r.flags&flagTC == 0 || len(r.answers) != 0 {
		t.Fatalf("UDP response %+v", r)
	}
	if want := "query tcp-fallback.test 1 192.0.2.10:40000 id=9\n"; log.String() != want {
		t.Fatalf("log %q, want %q", log.String(), want)
	}
	for _, bad := range []string{
		"x.test 60 TCP",
		"x.test 60 TCP 1.2.3",
		"x.test 60 A 1.2.3.4 1.2.3.5",
		"x.test TCPRESET\nx.test 60 TCP 1.2.3.4",
		"x.test 60 TCP 1.2.3.4\nx.test 60 TCP 1.2.3.5",
	} {
		if _, err := parseZone(strings.NewReader(bad)); err == nil {
			t.Errorf("zone %q accepted", bad)
		}
	}
}

func TestDNSTCPAnswer(t *testing.T) {
	query := framed(dnsQueryBytes(0x1234, "TCP-fallback.test", typeA, true))
	for _, split := range []struct {
		name  string
		parts [][]byte
	}{
		{"whole", [][]byte{query}},
		{"prefix byte", [][]byte{query[:1], query[1:]}},
		{"prefix", [][]byte{query[:2], query[2:]}},
	} {
		t.Run(split.name, func(t *testing.T) {
			var log bytes.Buffer
			h := tcpZoneHost(t, &log)
			const mss = 536
			c := dial(t, h, 41000, mss, 65535)
			var got []tcpSegment
			for index, part := range split.parts {
				out := c.send(tcpACK|tcpPSH, part)
				if index < len(split.parts)-1 {
					if len(out) != 1 || out[0].flags != tcpACK || len(out[0].payload) != 0 || out[0].ack != c.seq {
						t.Fatalf("partial query reply %+v", out)
					}
					continue
				}
				got = out
			}
			if len(got) < 4 || len(got[0].payload) != 2 || got[0].ack != c.seq {
				t.Fatalf("first of %d segments %+v", len(got), got[0])
			}
			var stream []byte
			next := c.ack
			for _, segment := range got[:len(got)-1] {
				if segment.seq != next || len(segment.payload) == 0 || len(segment.payload) > mss || segment.flags&tcpFIN != 0 {
					t.Fatalf("segment seq %d want %d, %d bytes, flags %#x", segment.seq, next, len(segment.payload), segment.flags)
				}
				stream = append(stream, segment.payload...)
				next += uint32(len(segment.payload))
			}
			fin := got[len(got)-1]
			if fin.flags != tcpFIN|tcpACK || fin.seq != next || len(fin.payload) != 0 {
				t.Fatalf("FIN %+v", fin)
			}
			length := int(binary.BigEndian.Uint16(stream))
			if length != len(stream)-2 || length <= 4096 {
				t.Fatalf("prefix %d, body %d", length, len(stream)-2)
			}
			r := readResponse(t, stream[2:])
			if r.id != 0x1234 || r.flags&flagTC != 0 || r.flags&0xf != 0 || r.question != "TCP-fallback.test" || len(r.answers) != 2 ||
				r.answers[0] != (dnsRR{name: "TCP-fallback.test", rtype: typeA, ttl: 60, data: "192.0.2.14"}) ||
				r.answers[1].data != "192.0.2.15" {
				t.Fatalf("response %+v", r)
			}
			opts := 0
			for index, rr := range r.additional {
				switch rr.rtype {
				case typeOPT:
					opts++
				case typeTXT:
				default:
					t.Fatalf("additional %d %+v", index, rr)
				}
			}
			if opts != 1 || len(r.additional) != padRecords+1 {
				t.Fatalf("additional %+v", r.additional)
			}

			c.ack = fin.seq + 1
			if out := c.send(tcpACK, nil); len(out) != 0 {
				t.Fatalf("ACK of FIN produced %d segments", len(out))
			}
			last := c.send(tcpFIN|tcpACK, nil)
			if len(last) != 1 || last[0].flags != tcpACK || last[0].ack != c.seq || last[0].seq != c.ack {
				t.Fatalf("ACK of client FIN %+v", last)
			}
			if len(h.tcp) != 0 {
				t.Fatalf("%d connections left", len(h.tcp))
			}
			want := "dns-tcp-accept 192.0.2.10:41000\n" +
				"dns-tcp-query tcp-fallback.test 1 192.0.2.10:41000 id=4660 len=" + itoa(uint32(len(query)-2)) + "\n" +
				"dns-tcp-answer tcp-fallback.test 192.0.2.10:41000 len=" + itoa(uint32(length)) + "\n"
			if log.String() != want {
				t.Fatalf("log %q, want %q", log.String(), want)
			}
		})
	}
}

func TestDNSTCPWindowAndRetransmit(t *testing.T) {
	var log bytes.Buffer
	h := tcpZoneHost(t, &log)
	var timer func() [][]byte
	h.after = func(delay time.Duration, fn func() [][]byte) {
		if delay != dnsRetransmit {
			t.Fatalf("retransmit delay %s", delay)
		}
		timer = fn
	}
	c := dial(t, h, 41001, 1460, 1500)
	got := c.send(tcpACK|tcpPSH, framed(dnsQueryBytes(1, "tcp-fallback.test", typeA, false)))
	sent := 0
	for _, segment := range got {
		if segment.flags&tcpFIN != 0 || len(segment.payload) > tcpPeerMSS {
			t.Fatalf("segment %+v", segment)
		}
		sent += len(segment.payload)
	}
	if sent != 1500 || len(got[0].payload) != 2 || timer == nil {
		t.Fatalf("sent %d bytes in %d segments", sent, len(got))
	}
	// The client takes the prefix and nothing more; the timer goes back to
	// the first unacknowledged byte.
	c.ack += 2
	if out := c.send(tcpACK, nil); len(out) != 1 || len(out[0].payload) != 2 {
		t.Fatalf("after window slide %+v", out)
	}
	again := timer()
	if len(again) == 0 || tcpFrameAt(t, again[0]).seq != c.ack {
		t.Fatalf("retransmit %d frames", len(again))
	}
	c.ack += 1500
	c.window = 65535
	rest := c.send(tcpACK, nil)
	if len(rest) == 0 || rest[len(rest)-1].flags != tcpFIN|tcpACK {
		t.Fatalf("rest %+v", rest)
	}
}

func TestDNSTCPReset(t *testing.T) {
	var log bytes.Buffer
	h := tcpZoneHost(t, &log)
	c := dial(t, h, 41002, 1460, 65535)
	query := framed(dnsQueryBytes(7, "tcp-fallback-reset.test", typeA, false))
	got := c.send(tcpACK|tcpPSH, query)
	if len(got) != 3 || len(got[0].payload) != 2 || len(got[1].payload) != dnsResetBody || got[2].flags != tcpRST|tcpACK {
		t.Fatalf("reset segments %+v", got)
	}
	if length := binary.BigEndian.Uint16(got[0].payload); length <= dnsResetBody {
		t.Fatalf("prefix %d", length)
	}
	if binary.BigEndian.Uint16(got[1].payload) != 7 || got[2].seq != got[1].seq+dnsResetBody || got[2].ack != c.seq {
		t.Fatalf("partial answer %+v RST %+v", got[1], got[2])
	}
	if len(h.tcp) != 0 {
		t.Fatal("connection kept after reset")
	}
	if !strings.Contains(log.String(), "dns-tcp-query tcp-fallback-reset.test 1 192.0.2.10:41002 id=7 len=") ||
		!strings.HasSuffix(log.String(), "dns-tcp-reset tcp-fallback-reset.test 192.0.2.10:41002\n") {
		t.Fatalf("log %q", log.String())
	}

	c = dial(t, h, 41003, 1460, 65535)
	if got := c.send(tcpACK|tcpPSH, []byte{0, 3, 1, 2, 3}); len(got) != 1 || got[0].flags != tcpRST|tcpACK || len(h.tcp) != 0 {
		t.Fatalf("garbage query reply %+v", got)
	}
}
