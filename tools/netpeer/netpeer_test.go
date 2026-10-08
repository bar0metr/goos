package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	guestMAC  = mac{0x52, 0x54, 0x00, 0x00, 0x02, 0x10}
	otherMAC  = mac{0x52, 0x54, 0x00, 0x00, 0x02, 0x11}
	guestIP   = ipv4{192, 0, 2, 10}
	peerMAC   = mac{0x52, 0x54, 0x00, 0x00, 0x02, 0x53}
	peerIP    = ipv4{192, 0, 2, 53}
	foreignHW = mac{0x52, 0x54, 0x00, 0xde, 0xad, 0x01}
)

const testZone = `# test zone
host.test 300 A 192.0.2.80
multi.test 300 A 192.0.2.81
multi.test 300 a 192.0.2.82
alias.test 120 CNAME Host.Test.
outside.test 120 CNAME example.com
missing.test NXDOMAIN
empty.test NODATA
broken.test SERVFAIL
big.test TRUNCATE
silent.test DROP
loop1.test 60 CNAME loop2.test
loop2.test 60 CNAME loop1.test
data.test 60 A 192.0.2.90
data.test 60 AAAA 2001:db8::90
data.test 60 MX 10 Mail.Test.
data.test 60 MX 20 backup.test
data.test 60 NS ns.test
data.test 60 TXT hello  world
data.test 60 TXT bell\007 end
ptr.test 60 PTR data.test
`

func testHost(t *testing.T, log io.Writer) *host {
	t.Helper()
	z, err := parseZone(strings.NewReader(testZone))
	if err != nil {
		t.Fatal(err)
	}
	return newHost(config{
		mac: peerMAC, ip: peerIP, mask: ipv4{255, 255, 255, 0},
		router: ipv4{192, 0, 2, 1}, hasRouter: true,
		dns: []ipv4{peerIP, {192, 0, 2, 54}}, leaseTime: 3600,
		leases:     map[mac]ipv4{guestMAC: guestIP},
		zone:       z,
		foreignMAC: foreignHW,
	}, &logger{w: log})
}

func one(t *testing.T, frames [][]byte) []byte {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	return frames[0]
}

func etherHeader(t *testing.T, frame []byte) (dst, src mac, etherType uint16) {
	t.Helper()
	if len(frame) < minFrame {
		t.Fatalf("frame of %d bytes is shorter than %d", len(frame), minFrame)
	}
	copy(dst[:], frame[0:6])
	copy(src[:], frame[6:12])
	return dst, src, binary.BigEndian.Uint16(frame[12:14])
}

func TestFraming(t *testing.T) {
	var buffer bytes.Buffer
	frames := [][]byte{bytes.Repeat([]byte{0xaa}, 60), {1, 2, 3}, {}}
	for _, frame := range frames {
		if err := writeFrame(&buffer, frame); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.Bytes()[:4]; !bytes.Equal(got, []byte{0, 0, 0, 60}) {
		t.Fatalf("length prefix % x, want 00 00 00 3c", got)
	}
	for _, want := range frames {
		got, err := readFrame(&buffer)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("readFrame = % x, %v; want % x", got, err, want)
		}
	}
	if _, err := readFrame(&buffer); err != io.EOF {
		t.Fatalf("readFrame at end = %v, want EOF", err)
	}
	if _, err := readFrame(bytes.NewReader([]byte{0, 0, 0, 9, 1, 2})); err != io.ErrUnexpectedEOF {
		t.Fatalf("short frame: %v, want unexpected EOF", err)
	}
	if _, err := readFrame(bytes.NewReader([]byte{0, 0x10, 0, 0})); err == nil {
		t.Fatal("oversized length accepted")
	}
}

func arpRequestFrame(sender mac, senderIP, targetIP ipv4) []byte {
	p := arpPacket{op: arpRequest, senderMAC: sender, senderIP: senderIP, targetIP: targetIP}
	return etherFrame(broadcastMAC, sender, etherTypeARP, p.bytes())
}

func TestARPReply(t *testing.T) {
	h := testHost(t, io.Discard)
	frame := one(t, h.handle(arpRequestFrame(guestMAC, guestIP, peerIP)))
	dst, src, etherType := etherHeader(t, frame)
	if dst != guestMAC || src != peerMAC || etherType != etherTypeARP {
		t.Fatalf("header %s %s %#x", dst, src, etherType)
	}
	p, ok := parseARP(frame[14:])
	want := arpPacket{op: arpReply, senderMAC: peerMAC, senderIP: peerIP, targetMAC: guestMAC, targetIP: guestIP}
	if !ok || p != want {
		t.Fatalf("reply %+v, want %+v", p, want)
	}
	if out := h.handle(arpRequestFrame(guestMAC, guestIP, ipv4{192, 0, 2, 99})); len(out) != 0 {
		t.Fatal("answered a request for another address")
	}
}

func ipv4Frame(dst mac, src ipv4, dstIP ipv4, protocol byte, payload []byte) []byte {
	return etherFrame(dst, guestMAC, etherTypeIPv4, buildIPv4(src, dstIP, protocol, 7, payload))
}

func TestICMPEchoReply(t *testing.T) {
	h := testHost(t, io.Discard)
	request := []byte{8, 0, 0, 0, 0x12, 0x34, 0, 1, 'p', 'i', 'n', 'g', '!'}
	binary.BigEndian.PutUint16(request[2:4], checksum(request, 0))
	frame := one(t, h.handle(ipv4Frame(peerMAC, guestIP, peerIP, 1, request)))
	dst, src, _ := etherHeader(t, frame)
	if dst != guestMAC || src != peerMAC {
		t.Fatalf("header %s %s", dst, src)
	}
	packet, ok := parseIPv4(frame[14:])
	if !ok || packet.src != peerIP || packet.dst != guestIP || packet.protocol != 1 {
		t.Fatalf("IPv4 %+v ok=%v", packet, ok)
	}
	reply := packet.payload
	if reply[0] != 0 || reply[1] != 0 || checksum(reply, 0) != 0 {
		t.Fatalf("reply type %d code %d checksum %#x", reply[0], reply[1], checksum(reply, 0))
	}
	if !bytes.Equal(reply[4:], request[4:]) {
		t.Fatalf("reply body % x, want % x", reply[4:], request[4:])
	}
	if out := h.handle(ipv4Frame(peerMAC, guestIP, ipv4{192, 0, 2, 99}, 1, request)); len(out) != 0 {
		t.Fatal("answered an echo for another address")
	}
}

func dhcpRequestFrame(client mac, msgType byte, ciaddr ipv4, flags uint16, options ...[]byte) []byte {
	b := make([]byte, 240)
	b[0], b[1], b[2] = 1, 1, 6
	binary.BigEndian.PutUint32(b[4:8], 0xdeadbeef)
	binary.BigEndian.PutUint16(b[10:12], flags)
	copy(b[12:16], ciaddr[:])
	copy(b[28:34], client[:])
	copy(b[236:240], dhcpCookie[:])
	b = append(b, 53, 1, msgType)
	for _, option := range options {
		b = append(b, option...)
	}
	b = append(b, 255)
	dstIP := broadcastIPv4
	if ciaddr != (ipv4{}) {
		dstIP = peerIP
	}
	udp := buildUDP(ciaddr, dstIP, 68, 67, b)
	return etherFrame(broadcastMAC, client, etherTypeIPv4, buildIPv4(ciaddr, dstIP, 17, 1, udp))
}

func option(code byte, value ...byte) []byte { return append([]byte{code, byte(len(value))}, value...) }

type dhcpReplyView struct {
	dstMAC  mac
	dstIP   ipv4
	yiaddr  ipv4
	xid     uint32
	options map[byte][]byte
}

func readDHCPReply(t *testing.T, frame []byte) dhcpReplyView {
	t.Helper()
	dst, src, _ := etherHeader(t, frame)
	if src != peerMAC {
		t.Fatalf("source MAC %s", src)
	}
	packet, ok := parseIPv4(frame[14:])
	if !ok || packet.src != peerIP {
		t.Fatalf("IPv4 %+v ok=%v", packet, ok)
	}
	udp, ok := parseUDP(packet.payload)
	if !ok || udp.srcPort != 67 || udp.dstPort != 68 {
		t.Fatalf("UDP %+v ok=%v", udp, ok)
	}
	b := udp.payload
	if len(b) < 300 || b[0] != 2 || [4]byte(b[236:240]) != dhcpCookie {
		t.Fatalf("BOOTREPLY of %d bytes, op %d", len(b), b[0])
	}
	view := dhcpReplyView{dstMAC: dst, dstIP: packet.dst, xid: binary.BigEndian.Uint32(b[4:8]), options: map[byte][]byte{}}
	copy(view.yiaddr[:], b[16:20])
	for i := 240; b[i] != 255; i += 2 + int(b[i+1]) {
		view.options[b[i]] = b[i+2 : i+2+int(b[i+1])]
	}
	return view
}

func TestDHCPOfferAndAck(t *testing.T) {
	var log bytes.Buffer
	h := testHost(t, &log)
	offer := readDHCPReply(t, one(t, h.handle(dhcpRequestFrame(guestMAC, dhcpDiscover, ipv4{}, 0))))
	if offer.yiaddr != guestIP || offer.xid != 0xdeadbeef || offer.dstIP != broadcastIPv4 || offer.dstMAC != broadcastMAC {
		t.Fatalf("offer %+v", offer)
	}
	want := map[byte][]byte{
		53: {dhcpOffer},
		54: peerIP[:],
		51: {0, 0, 0x0e, 0x10},
		1:  {255, 255, 255, 0},
		3:  {192, 0, 2, 1},
		6:  {192, 0, 2, 53, 192, 0, 2, 54},
	}
	for code, value := range want {
		if !bytes.Equal(offer.options[code], value) {
			t.Errorf("offer option %d = %v, want %v", code, offer.options[code], value)
		}
	}
	if len(offer.options) != len(want) {
		t.Errorf("offer options %v", offer.options)
	}

	selecting := dhcpRequestFrame(guestMAC, dhcpRequest, ipv4{}, 0, option(50, guestIP[:]...), option(54, peerIP[:]...))
	ack := readDHCPReply(t, one(t, h.handle(selecting)))
	if !bytes.Equal(ack.options[53], []byte{dhcpAck}) || ack.yiaddr != guestIP || ack.dstIP != broadcastIPv4 {
		t.Fatalf("selecting ack %+v", ack)
	}

	renewing := dhcpRequestFrame(guestMAC, dhcpRequest, guestIP, 0)
	ack = readDHCPReply(t, one(t, h.handle(renewing)))
	if !bytes.Equal(ack.options[53], []byte{dhcpAck}) || ack.dstIP != guestIP || ack.dstMAC != guestMAC {
		t.Fatalf("renewing ack %+v", ack)
	}
	rebinding := dhcpRequestFrame(guestMAC, dhcpRequest, guestIP, dhcpBroadcastFlag)
	ack = readDHCPReply(t, one(t, h.handle(rebinding)))
	if ack.dstIP != broadcastIPv4 || ack.dstMAC != broadcastMAC {
		t.Fatalf("broadcast flag ignored: %+v", ack)
	}

	wrong := dhcpRequestFrame(guestMAC, dhcpRequest, ipv4{}, 0, option(50, 192, 0, 2, 99))
	nak := readDHCPReply(t, one(t, h.handle(wrong)))
	if !bytes.Equal(nak.options[53], []byte{dhcpNak}) || nak.yiaddr != (ipv4{}) || len(nak.options) != 2 {
		t.Fatalf("nak %+v", nak)
	}
	other := dhcpRequestFrame(guestMAC, dhcpRequest, ipv4{}, 0, option(50, guestIP[:]...), option(54, 192, 0, 2, 1))
	if out := h.handle(other); len(out) != 0 {
		t.Fatal("answered a request for another server")
	}
	if out := h.handle(dhcpRequestFrame(otherMAC, dhcpDiscover, ipv4{}, 0)); len(out) != 0 {
		t.Fatal("answered an unknown MAC")
	}
	if out := h.handle(dhcpRequestFrame(guestMAC, dhcpRelease, guestIP, 0)); len(out) != 0 {
		t.Fatal("answered a release")
	}
	for _, line := range []string{
		"dhcp DISCOVER 52:54:00:00:02:10 0.0.0.0",
		"dhcp OFFER 52:54:00:00:02:10 192.0.2.10",
		"dhcp REQUEST 52:54:00:00:02:10 192.0.2.10",
		"dhcp ACK 52:54:00:00:02:10 192.0.2.10",
		"dhcp NAK 52:54:00:00:02:10 192.0.2.99",
		"dhcp DISCOVER 52:54:00:00:02:11 0.0.0.0",
		"dhcp RELEASE 52:54:00:00:02:10 192.0.2.10",
	} {
		if !strings.Contains(log.String(), line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, log.String())
		}
	}
}

func TestDHCPWithoutRouter(t *testing.T) {
	h := testHost(t, io.Discard)
	h.hasRouter = false
	offer := readDHCPReply(t, one(t, h.handle(dhcpRequestFrame(guestMAC, dhcpDiscover, ipv4{}, 0))))
	if _, ok := offer.options[3]; ok {
		t.Fatal("router option without -router")
	}
}

func TestZoneParsing(t *testing.T) {
	z, err := parseZone(strings.NewReader(testZone))
	if err != nil {
		t.Fatal(err)
	}
	if e := z.entries["multi.test"]; e.kind != kindA || len(e.addrs) != 2 || e.ttl != 300 {
		t.Fatalf("multi.test %+v", e)
	}
	if e := z.entries["alias.test"]; e.kind != kindCNAME || e.target != "host.test" {
		t.Fatalf("alias.test %+v", e)
	}
	if e := z.entries["silent.test"]; e.kind != kindDROP {
		t.Fatalf("silent.test %+v", e)
	}
	for _, bad := range []string{
		"host.example 60 A 1.2.3.4",
		"host.test sixty A 1.2.3.4",
		"host.test 60 A 1.2.3",
		"host.test 60 MX mail.test",
		"host.test BOGUS",
		"host.test 60 A 1.2.3.4\nhost.test 60 CNAME x.test",
		"host.test NODATA\nhost.test NXDOMAIN",
		"host.test 60 A",
		"host.test 60 AAAA 192.0.2.1",
		"host.test 60 AAAA ::ffff:192.0.2.1",
		"host.test 60 MX ten mail.test",
		"host.test 60 MX 10 mail.test extra",
		"host.test 60 TXT bad\\07",
		"host.test 60 TXT bad\\999",
		"host.test 60 CNAME x.test\nhost.test 60 TXT y",
		"host.test 60 SLOW 1.2.3.4\nhost.test 60 TXT y",
	} {
		if _, err := parseZone(strings.NewReader(bad)); err == nil {
			t.Errorf("zone %q accepted", bad)
		}
	}
}

func dnsQueryBytes(id uint16, name string, qtype uint16, opt bool) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	b = binary.BigEndian.AppendUint16(b, flagRD)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, 0)
	additional := uint16(0)
	if opt {
		additional = 1
	}
	b = binary.BigEndian.AppendUint16(b, additional)
	for _, label := range splitName(name) {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, qtype)
	b = binary.BigEndian.AppendUint16(b, 1)
	if opt {
		b = append(b, 0, 0, 41, 0x10, 0, 0, 0, 0, 0, 0, 0)
	}
	return b
}

type dnsRR struct {
	name  string
	rtype uint16
	ttl   uint32
	data  string
}

type dnsResponse struct {
	id                 uint16
	flags              uint16
	question           string
	answers, authority []dnsRR
	additional         []dnsRR
}

func readName(t *testing.T, b []byte, offset int) (string, int) {
	t.Helper()
	var labels []string
	end := -1
	for jumps := 0; ; {
		length := int(b[offset])
		if length == 0 {
			offset++
			break
		}
		if length&0xc0 == 0xc0 {
			if end < 0 {
				end = offset + 2
			}
			offset = int(binary.BigEndian.Uint16(b[offset:]) & 0x3fff)
			if jumps++; jumps > 20 {
				t.Fatal("pointer loop")
			}
			continue
		}
		labels = append(labels, string(b[offset+1:offset+1+length]))
		offset += 1 + length
	}
	if end < 0 {
		end = offset
	}
	return strings.Join(labels, "."), end
}

func readResponse(t *testing.T, b []byte) dnsResponse {
	t.Helper()
	r := dnsResponse{id: binary.BigEndian.Uint16(b), flags: binary.BigEndian.Uint16(b[2:])}
	if binary.BigEndian.Uint16(b[4:]) != 1 {
		t.Fatal("question count not 1")
	}
	name, offset := readName(t, b, 12)
	r.question = name
	offset += 4
	sections := []*[]dnsRR{&r.answers, &r.authority, &r.additional}
	for index, section := range sections {
		count := int(binary.BigEndian.Uint16(b[6+2*index:]))
		for range count {
			var rr dnsRR
			rr.name, offset = readName(t, b, offset)
			rr.rtype = binary.BigEndian.Uint16(b[offset:])
			rr.ttl = binary.BigEndian.Uint32(b[offset+4:])
			length := int(binary.BigEndian.Uint16(b[offset+8:]))
			data := offset + 10
			switch rr.rtype {
			case typeA:
				rr.data = ipv4(b[data : data+4]).String()
			case typeCNAME:
				rr.data, _ = readName(t, b, data)
			case typeSOA:
				mname, next := readName(t, b, data)
				rname, next := readName(t, b, next)
				minimum := binary.BigEndian.Uint32(b[next+16:])
				rr.data = mname + " " + rname + " " + itoa(minimum)
			case typeOPT:
				rr.data = itoa(uint32(binary.BigEndian.Uint16(b[offset+2:])))
			}
			offset = data + length
			*section = append(*section, rr)
		}
	}
	if offset != len(b) {
		t.Fatalf("response has %d bytes past its records", len(b)-offset)
	}
	return r
}

func itoa(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

func ask(t *testing.T, z *zone, noEDNS bool, name string, qtype uint16, opt bool) (dnsResponse, bool) {
	t.Helper()
	q, ok := parseQuery(dnsQueryBytes(0x4242, name, qtype, opt))
	if !ok {
		t.Fatalf("query for %s did not parse", name)
	}
	b := answer(q, z, noEDNS)
	if b == nil {
		return dnsResponse{}, false
	}
	r := readResponse(t, b)
	if r.id != 0x4242 || r.flags&flagQR == 0 || r.flags&flagRD == 0 || r.flags&flagRA == 0 || r.question != name {
		t.Fatalf("header %+v", r)
	}
	return r, true
}

var soaWant = dnsRR{name: "test", rtype: typeSOA, ttl: 60, data: "ns.test hostmaster.test 30"}

func TestDNSAnswers(t *testing.T) {
	z, err := parseZone(strings.NewReader(testZone))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ask(t, z, false, "Host.TEST", typeA, false)
	if r.flags&0xf != 0 || r.flags&flagAA == 0 || len(r.answers) != 1 ||
		r.answers[0] != (dnsRR{name: "Host.TEST", rtype: typeA, ttl: 300, data: "192.0.2.80"}) || len(r.additional) != 0 {
		t.Fatalf("A %+v", r)
	}
	r, _ = ask(t, z, false, "multi.test", typeA, false)
	if len(r.answers) != 2 || r.answers[1].data != "192.0.2.82" {
		t.Fatalf("two A %+v", r)
	}
	r, _ = ask(t, z, false, "alias.test", typeA, false)
	if len(r.answers) != 2 ||
		r.answers[0] != (dnsRR{name: "alias.test", rtype: typeCNAME, ttl: 120, data: "host.test"}) ||
		r.answers[1] != (dnsRR{name: "host.test", rtype: typeA, ttl: 300, data: "192.0.2.80"}) {
		t.Fatalf("CNAME %+v", r)
	}
	r, _ = ask(t, z, false, "outside.test", typeA, false)
	if r.flags&0xf != 0 || len(r.answers) != 1 || r.answers[0].data != "example.com" {
		t.Fatalf("CNAME out of zone %+v", r)
	}
	for _, name := range []string{"missing.test", "nowhere.test"} {
		r, _ = ask(t, z, false, name, typeA, false)
		if r.flags&0xf != rcodeNXDomain || len(r.answers) != 0 || len(r.authority) != 1 || r.authority[0] != soaWant {
			t.Fatalf("NXDOMAIN %s %+v", name, r)
		}
	}
	for _, query := range []struct {
		name  string
		qtype uint16
	}{{"empty.test", typeA}, {"host.test", 28}, {"test", typeA}} {
		r, _ = ask(t, z, false, query.name, query.qtype, false)
		if r.flags&0xf != 0 || len(r.answers) != 0 || len(r.authority) != 1 || r.authority[0] != soaWant {
			t.Fatalf("NODATA %v %+v", query, r)
		}
	}
	r, _ = ask(t, z, false, "example.com", typeA, false)
	if r.flags&0xf != rcodeRefused || len(r.answers)+len(r.authority) != 0 {
		t.Fatalf("REFUSED %+v", r)
	}
	r, _ = ask(t, z, false, "broken.test", typeA, false)
	if r.flags&0xf != rcodeServFail || len(r.answers) != 0 {
		t.Fatalf("SERVFAIL %+v", r)
	}
	r, _ = ask(t, z, false, "loop1.test", typeA, false)
	if r.flags&0xf != rcodeServFail {
		t.Fatalf("CNAME loop %+v", r)
	}
	r, _ = ask(t, z, false, "big.test", typeA, false)
	if r.flags&flagTC == 0 || r.flags&0xf != 0 || len(r.answers) != 0 {
		t.Fatalf("TRUNCATE %+v", r)
	}
	if _, answered := ask(t, z, false, "silent.test", typeA, false); answered {
		t.Fatal("DROP answered")
	}
	if _, answered := ask(t, z, true, "silent.test", typeA, true); answered {
		t.Fatal("DROP answered with -no-edns")
	}
}

// Records beside or instead of addresses answer their own type only.
func TestDNSDataRecords(t *testing.T) {
	z, err := parseZone(strings.NewReader(testZone))
	if err != nil {
		t.Fatal(err)
	}
	raw := func(name string, qtype uint16) []byte {
		q, ok := parseQuery(dnsQueryBytes(1, name, qtype, false))
		if !ok {
			t.Fatal("query")
		}
		return answer(q, z, false)
	}
	for _, c := range []struct {
		qtype uint16
		want  []string
	}{
		{typeA, []string{"\xc0\x00\x02\x5a"}},
		{typeAAAA, []string{"\x20\x01\x0d\xb8\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x90"}},
		{typeMX, []string{"\x00\x0a\x04mail", "\x00\x14\x06backup"}},
		{typeNS, []string{"\x02ns"}},
		{typeTXT, []string{"\x0bhello world", "\x09bell\x07 end"}},
	} {
		r, _ := ask(t, z, false, "data.test", c.qtype, false)
		if len(r.answers) != len(c.want) || len(r.authority) != 0 {
			t.Fatalf("type %d: %+v", c.qtype, r)
		}
		body := string(raw("data.test", c.qtype))
		for _, want := range c.want {
			if !strings.Contains(body, want) {
				t.Fatalf("type %d: no %q in % x", c.qtype, want, body)
			}
		}
	}
	r, _ := ask(t, z, false, "ptr.test", typePTR, false)
	if len(r.answers) != 1 || !strings.Contains(string(raw("ptr.test", typePTR)), "\x04data") {
		t.Fatalf("PTR %+v", r)
	}
	for _, query := range []struct {
		name  string
		qtype uint16
	}{{"data.test", typeSOA}, {"data.test", typeCNAME}, {"ptr.test", typeA}} {
		r, _ = ask(t, z, false, query.name, query.qtype, false)
		if r.flags&0xf != 0 || len(r.answers) != 0 || len(r.authority) != 1 || r.authority[0] != soaWant {
			t.Fatalf("NODATA %v %+v", query, r)
		}
	}
}

func TestDNSEDNS(t *testing.T) {
	z, err := parseZone(strings.NewReader(testZone))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ask(t, z, false, "host.test", typeA, true)
	if len(r.answers) != 1 || len(r.additional) != 1 ||
		r.additional[0] != (dnsRR{name: "", rtype: typeOPT, ttl: 0, data: "1232"}) {
		t.Fatalf("OPT %+v", r)
	}
	r, _ = ask(t, z, false, "missing.test", typeA, true)
	if len(r.additional) != 1 || len(r.authority) != 1 {
		t.Fatalf("OPT on NXDOMAIN %+v", r)
	}
	r, _ = ask(t, z, true, "host.test", typeA, true)
	if r.flags&0xf != rcodeFormErr || len(r.answers)+len(r.authority)+len(r.additional) != 0 {
		t.Fatalf("FORMERR %+v", r)
	}
	r, _ = ask(t, z, true, "host.test", typeA, false)
	if r.flags&0xf != 0 || len(r.answers) != 1 || len(r.additional) != 0 {
		t.Fatalf("fallback without OPT %+v", r)
	}
}

func TestDNSMalformed(t *testing.T) {
	good := dnsQueryBytes(1, "host.test", typeA, true)
	if _, ok := parseQuery(good); !ok {
		t.Fatal("good query rejected")
	}
	for length := range len(good) {
		if _, ok := parseQuery(good[:length]); ok {
			t.Fatalf("truncated query of %d bytes accepted", length)
		}
	}
	pointer := append([]byte(nil), good[:12]...)
	pointer = append(pointer, 0xc0, 12, 0, 1, 0, 1)
	pointer[11] = 0
	if _, ok := parseQuery(pointer); ok {
		t.Fatal("compressed question accepted")
	}
	response := append([]byte(nil), good...)
	response[2] |= 0x80
	if _, ok := parseQuery(response); ok {
		t.Fatal("response accepted as a query")
	}
	two := append([]byte(nil), good...)
	two[5] = 2
	if _, ok := parseQuery(two); ok {
		t.Fatal("two questions accepted")
	}
	long := append([]byte(nil), good[:12]...)
	long = append(long, 64)
	long = append(long, bytes.Repeat([]byte{'a'}, 64)...)
	long = append(long, 0, 0, 1, 0, 1)
	long[11] = 0
	if _, ok := parseQuery(long); ok {
		t.Fatal("64-byte label accepted")
	}
}

func TestDNSOverUDPAndLog(t *testing.T) {
	var log bytes.Buffer
	h := testHost(t, &log)
	query := dnsQueryBytes(77, "Host.test", typeA, false)
	frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 40000, 53, query))
	reply := one(t, h.handle(frame))
	dst, _, _ := etherHeader(t, reply)
	packet, ok := parseIPv4(reply[14:])
	udp, ok2 := parseUDP(packet.payload)
	if dst != guestMAC || !ok || !ok2 || packet.dst != guestIP || udp.srcPort != 53 || udp.dstPort != 40000 {
		t.Fatalf("reply %s %+v %+v", dst, packet, udp)
	}
	if r := readResponse(t, udp.payload); len(r.answers) != 1 {
		t.Fatalf("response %+v", r)
	}
	if want := "query host.test 1 192.0.2.10:40000 id=77\n"; log.String() != want {
		t.Fatalf("log %q, want %q", log.String(), want)
	}
}

func TestClaimAndDefend(t *testing.T) {
	var log bytes.Buffer
	h := testHost(t, &log)
	target := ipv4{192, 0, 2, 10}
	frames, quit, err := h.command("claim 192.0.2.10")
	if err != nil || quit {
		t.Fatal(err, quit)
	}
	claim := one(t, frames)
	dst, src, etherType := etherHeader(t, claim)
	p, ok := parseARP(claim[14:])
	want := arpPacket{op: arpRequest, senderMAC: foreignHW, senderIP: target, targetIP: target}
	if dst != broadcastMAC || src != foreignHW || etherType != etherTypeARP || !ok || p != want {
		t.Fatalf("claim %s %s %+v", dst, src, p)
	}

	probe := arpRequestFrame(guestMAC, ipv4{}, target)
	if out := h.handle(probe); len(out) != 0 {
		t.Fatal("probe answered before defend-probe")
	}
	if _, _, err := h.command("defend-probe 192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	defend := one(t, h.handle(probe))
	dst, src, _ = etherHeader(t, defend)
	p, ok = parseARP(defend[14:])
	want = arpPacket{op: arpReply, senderMAC: foreignHW, senderIP: target, targetMAC: guestMAC}
	if dst != guestMAC || src != foreignHW || !ok || p != want {
		t.Fatalf("defend %s %s %+v", dst, src, p)
	}
	if out := h.handle(arpRequestFrame(guestMAC, guestIP, ipv4{192, 0, 2, 20})); len(out) != 0 {
		t.Fatal("non-probe request defended")
	}
	if len(h.handle(probe)) != 1 {
		t.Fatal("rule not kept after one reply")
	}
	if _, _, err := h.command("clear"); err != nil {
		t.Fatal(err)
	}
	if out := h.handle(probe); len(out) != 0 {
		t.Fatal("probe answered after clear")
	}
	if _, quit, _ := h.command("quit"); !quit {
		t.Fatal("quit did not quit")
	}
	for _, bad := range []string{"claim", "claim 1.2.3", "bogus"} {
		if _, _, err := h.command(bad); err == nil {
			t.Errorf("command %q accepted", bad)
		}
	}
	if want := "claim 192.0.2.10\ndefend-probe 192.0.2.10\ndefend 192.0.2.10\ndefend 192.0.2.10\nclear\nquit\n"; log.String() != want {
		t.Fatalf("log %q, want %q", log.String(), want)
	}
}

func TestServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "netpeer")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	srv, err := listen(socket, testHost(t, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	go srv.serve()
	left, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.guests() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("guests not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	left.SetDeadline(time.Now().Add(5 * time.Second))
	right.SetDeadline(time.Now().Add(5 * time.Second))

	hello := etherFrame(otherMAC, guestMAC, 0x88b5, []byte("hello"))
	if err := writeFrame(left, hello); err != nil {
		t.Fatal(err)
	}
	if got, err := readFrame(right); err != nil || !bytes.Equal(got, hello) {
		t.Fatalf("forwarded % x, %v", got, err)
	}

	query := dnsQueryBytes(9, "alias.test", typeA, true)
	frame := ipv4Frame(peerMAC, guestIP, peerIP, 17, buildUDP(guestIP, peerIP, 5353, 53, query))
	if err := writeFrame(left, frame); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(left)
	if err != nil {
		t.Fatal(err)
	}
	packet, _ := parseIPv4(got[14:])
	udp, _ := parseUDP(packet.payload)
	r := readResponse(t, udp.payload)
	if r.id != 9 || len(r.answers) != 2 || r.answers[1].data != "192.0.2.80" || len(r.additional) != 1 {
		t.Fatalf("DNS round trip %+v", r)
	}

	right.Close()
	srv.run("claim 192.0.2.10")
	if got, err := readFrame(left); err != nil || got[0] != 0xff {
		t.Fatalf("claim after a guest left: % x, %v", got, err)
	}
	srv.run("quit")
	select {
	case <-srv.quit:
	case <-time.After(time.Second):
		t.Fatal("quit not signalled")
	}
	srv.close()
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}
