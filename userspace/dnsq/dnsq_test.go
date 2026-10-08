package dnsq

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"goos/userspace/dnswire"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

func parsed(t *testing.T, line ...string) *Options {
	t.Helper()
	var options Options
	if problem, culprit := Parse(args(line), &options); problem != "" {
		t.Fatalf("%v: %s %q", line, problem, culprit)
	}
	return &options
}

func TestParseDefaults(t *testing.T) {
	options := parsed(t, "@192.0.2.53", "Example.Test")
	if options.Server != [4]byte{192, 0, 2, 53} || options.Type != dnswire.TypeA || options.Mode != Auto ||
		!options.EDNS || options.Timeout != DefaultTimeout || options.Short || options.Raw ||
		!bytes.Equal(options.Name.Bytes(), wire("example.test")) {
		t.Fatalf("%+v", options)
	}
}

func TestParseOptions(t *testing.T) {
	options := parsed(t, "--tcp", "@192.0.2.53", "--no-edns", "a.test", "mx", "--timeout", "3000", "--raw")
	if options.Mode != TCPOnly || options.EDNS || options.Type != dnswire.TypeMX || options.Timeout != 3000 || !options.Raw {
		t.Fatalf("%+v", options)
	}
	if parsed(t, "-u", "@192.0.2.53", "a.test", "--short").Mode != UDPOnly {
		t.Fatal("-u")
	}
	if parsed(t, "@192.0.2.53", "-t", "a.test").Mode != TCPOnly {
		t.Fatal("-t")
	}
	for text, kind := range map[string]uint16{"A": dnswire.TypeA, "AAAA": dnswire.TypeAAAA, "CNAME": dnswire.TypeCNAME,
		"NS": dnswire.TypeNS, "MX": dnswire.TypeMX, "TXT": dnswire.TypeTXT, "SOA": dnswire.TypeSOA, "PTR": dnswire.TypePTR} {
		if got := parsed(t, "@192.0.2.53", "a.test", text).Type; got != kind {
			t.Fatalf("%s: %d", text, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, line := range [][]string{
		{"a.test"},                                 // no server
		{"@192.0.2.53"},                            // no name
		{"@192.0.2", "a.test"},                     // bad server
		{"@example.test", "a.test"},                // names are not servers
		{"@0.0.0.0", "a.test"},                     // unspecified
		{"@192.0.2.53", "@192.0.2.54", "a.test"},   // two servers
		{"@192.0.2.53", "a.test", "ANY"},           // unsupported type
		{"@192.0.2.53", "a.test", "OPT"},           // not a query type
		{"@192.0.2.53", "a.test", "A", "extra"},    // too many
		{"@192.0.2.53", "a..test"},                 // bad name
		{"@192.0.2.53", "--tcp", "--udp", "a"},     // conflict
		{"@192.0.2.53", "-u", "-t", "a"},           // conflict
		{"@192.0.2.53", "--short", "--raw", "a"},   // conflict
		{"@192.0.2.53", "--timeout", "0", "a"},     // too short
		{"@192.0.2.53", "--timeout", "60001", "a"}, // too long
		{"@192.0.2.53", "a", "--timeout"},          // no value
		{"@192.0.2.53", "--edns", "a"},             // unknown option
	} {
		var options Options
		if problem, _ := Parse(args(line), &options); problem == "" {
			t.Errorf("%v accepted", line)
		}
	}
}

func run(t *testing.T, f *fake, line ...string) (int, Failure, string) {
	t.Helper()
	var query Query
	var out screen
	status, failure := query.Run(parsed(t, line...), 0x9906, f, &out)
	f.settled()
	return status, failure, out.String()
}

func aAnswer(name string) rr {
	return rr{name: name, kind: dnswire.TypeA, ttl: 300, data: []byte{192, 0, 2, 80}}
}

func TestUDPAnswer(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = reply{answers: []rr{aAnswer("example.test")}, opt: true}.answer
	status, failure, output := run(t, f, "@192.0.2.53", "example.test")
	if status != 0 || failure.Failed() || f.tcpConnects != 0 {
		t.Fatalf("%d %+v", status, failure)
	}
	want := `;; server: 192.0.2.53:53
;; transport: UDP
;; id: 39174
;; status: NOERROR
;; flags: qr rd ra

;; question:
example.test.    IN    A

;; answer:
example.test.    300    IN    A    192.0.2.80

;; 1 answer, 69 bytes, 5 ms
`
	if output != want {
		t.Fatalf("got\n%s\nwant\n%s", output, want)
	}
	// The query asked for A with RD and EDNS(0) at 1232 bytes, DO clear.
	query := f.udpQueries[0]
	if binary.BigEndian.Uint16(query) != 0x9906 || binary.BigEndian.Uint16(query[2:]) != dnswire.FlagRD ||
		binary.BigEndian.Uint16(query[10:]) != 1 ||
		!bytes.Equal(query[len(query)-dnswire.OPTSize:], []byte{0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("% x", query)
	}
}

func TestNoEDNS(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = reply{answers: []rr{aAnswer("example.test")}}.answer
	if status, _, _ := run(t, f, "--no-edns", "@192.0.2.53", "example.test", "AAAA"); status != 0 {
		t.Fatal(status)
	}
	query := f.udpQueries[0]
	if binary.BigEndian.Uint16(query[10:]) != 0 || len(query) != dnswire.HeaderSize+len(wire("example.test"))+4 ||
		binary.BigEndian.Uint16(query[len(query)-4:]) != dnswire.TypeAAAA {
		t.Fatalf("% x", query)
	}
}

// Every query type goes out as asked and its answer is shown.
func TestQueryTypes(t *testing.T) {
	soa := append(append(wire("ns.test"), wire("root.test")...), make([]byte, 20)...)
	soa[len(soa)-1] = 9
	cases := []struct {
		kind  string
		code  uint16
		data  []byte
		short string
	}{
		{"A", dnswire.TypeA, []byte{192, 0, 2, 1}, "192.0.2.1"},
		{"AAAA", dnswire.TypeAAAA, append([]byte{0x20, 0x01, 0x0d, 0xb8}, append(make([]byte, 11), 1)...), "2001:db8::1"},
		{"CNAME", dnswire.TypeCNAME, wire("host.example.test"), "host.example.test."},
		{"NS", dnswire.TypeNS, wire("ns.test"), "ns.test."},
		{"MX", dnswire.TypeMX, append([]byte{0, 10}, wire("mail.example.test")...), "10 mail.example.test."},
		{"TXT", dnswire.TypeTXT, []byte{5, 'h', 'e', 'l', 'l', 'o'}, `"hello"`},
		{"SOA", dnswire.TypeSOA, soa, "ns.test. root.test. 0 0 0 0 9"},
		{"PTR", dnswire.TypePTR, wire("host.test"), "host.test."},
	}
	for _, c := range cases {
		f := newFake(t)
		f.udpAnswer = reply{answers: []rr{{name: "q.test", kind: c.code, ttl: 60, data: c.data}}}.answer
		status, failure, output := run(t, f, "--short", "@192.0.2.53", "q.test", c.kind)
		if status != 0 || output != c.short+"\n" {
			t.Fatalf("%s: %d %+v %q", c.kind, status, failure, output)
		}
		query := f.udpQueries[0]
		if binary.BigEndian.Uint16(query[dnswire.HeaderSize+len(wire("q.test")):]) != c.code {
			t.Fatalf("%s: qtype", c.kind)
		}
	}
}

func TestShortSkipsEverythingElse(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = reply{answers: []rr{
		{name: "www.test", kind: dnswire.TypeCNAME, ttl: 60, data: wire("web.test")},
		aAnswer("web.test"),
	}, authority: []rr{{name: "test", kind: dnswire.TypeNS, data: wire("ns.test")}}, opt: true}.answer
	_, _, output := run(t, f, "--short", "@192.0.2.53", "www.test")
	if output != "web.test.\n192.0.2.80\n" {
		t.Fatalf("%q", output)
	}
}

// Valid responses with an error rcode are shown and are success.
func TestErrorRcodes(t *testing.T) {
	for rcode, text := range map[uint16]string{dnswire.RcodeNXDomain: "NXDOMAIN", dnswire.RcodeServFail: "SERVFAIL",
		dnswire.RcodeRefused: "REFUSED", dnswire.RcodeFormErr: "FORMERR"} {
		f := newFake(t)
		f.udpAnswer = reply{flags: rcode}.answer
		status, failure, output := run(t, f, "@192.0.2.53", "missing.test")
		if status != 0 || failure.Failed() || !strings.Contains(output, ";; status: "+text+"\n") ||
			strings.Contains(output, ";; answer:") || !strings.Contains(output, ";; 0 answers,") {
			t.Fatalf("%s: %d %+v\n%s", text, status, failure, output)
		}
	}
	// A FORMERR without a question section is shown without one.
	f := newFake(t)
	f.udpAnswer = reply{flags: dnswire.RcodeFormErr, noQuestion: true}.answer
	status, _, output := run(t, f, "@192.0.2.53", "a.test")
	if status != 0 || strings.Contains(output, ";; question:") {
		t.Fatalf("%d\n%s", status, output)
	}
}

func TestBadResponses(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"id":       reply{idDelta: 1}.answer,
		"question": reply{otherName: "other.test"}.answer,
		"short":    func(query []byte) []byte { return query[:5] },
		"query":    func(query []byte) []byte { return append([]byte(nil), query...) },
		"cut": func(query []byte) []byte {
			out := reply{answers: []rr{aAnswer("a.test")}}.answer(query)
			return out[:len(out)-2]
		},
		"bad MX":  reply{answers: []rr{{name: "a.test", kind: dnswire.TypeMX, data: []byte{0, 1, 9}}}}.answer,
		"bad TXT": reply{answers: []rr{{name: "a.test", kind: dnswire.TypeTXT, data: []byte{9, 'x'}}}}.answer,
	}
	for what, answer := range cases {
		f := newFake(t)
		f.udpAnswer = answer
		status, failure, output := run(t, f, "@192.0.2.53", "a.test")
		if status != 1 || !failure.Failed() || output != "" {
			t.Errorf("%s: %d %+v %q", what, status, failure, output)
		}
	}
}

func truncated(query []byte) []byte {
	return reply{flags: dnswire.FlagTC, answers: []rr{aAnswer("big.test")}}.answer(query)
}

func bigAnswer(query []byte) []byte {
	var answers []rr
	for index := 0; index < 300; index++ {
		answers = append(answers, rr{name: "big.test", kind: dnswire.TypeA, ttl: 60, data: []byte{10, 0, byte(index >> 8), byte(index)}})
	}
	return reply{answers: answers, opt: true}.answer(query)
}

func TestFallbackToTCP(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = truncated
	f.tcpAnswer = func(query []byte) []byte { return framed(bigAnswer(query)) }
	f.readSizes = []int{1, 3, 700, 4096}
	status, failure, output := run(t, f, "@192.0.2.53", "big.test")
	if status != 0 || failure.Failed() {
		t.Fatalf("%d %+v", status, failure)
	}
	// The same query went over TCP: same ID, question, RD and OPT.
	if !bytes.Equal(f.tcpWritten, framed(f.udpQueries[0])) {
		t.Fatal("TCP query differs")
	}
	if !strings.HasPrefix(output, ";; UDP response truncated; retrying over TCP\n;; server: 192.0.2.53:53\n;; transport: TCP\n") ||
		strings.Count(output, "    IN    A    10.0.") != 300 || !strings.Contains(output, ";; 300 answers, ") {
		t.Fatalf("\n%s", output[:400])
	}
	// The response is bigger than one RX slot.
	if len(bigAnswer(f.udpQueries[0])) <= tcpapi.SlotSize {
		t.Fatal("answer fits one slot")
	}
}

func TestUDPOnlyKeepsTruncation(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = truncated
	status, failure, output := run(t, f, "--udp", "@192.0.2.53", "big.test")
	if status != 1 || failure.Step != "udp" || !strings.Contains(failure.Reason, "truncated") || output != "" ||
		f.tcpConnects != 0 {
		t.Fatalf("%d %+v %q", status, failure, output)
	}
}

func TestShortFallbackIsQuiet(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = truncated
	f.tcpAnswer = func(query []byte) []byte { return framed(reply{answers: []rr{aAnswer("big.test")}}.answer(query)) }
	if _, _, output := run(t, f, "--short", "@192.0.2.53", "big.test"); output != "192.0.2.80\n" {
		t.Fatalf("%q", output)
	}
}

func TestForcedTCP(t *testing.T) {
	f := newFake(t)
	f.tcpAnswer = func(query []byte) []byte { return framed(reply{answers: []rr{aAnswer("a.test")}}.answer(query)) }
	f.writeLimit = 3
	f.readSizes = []int{1}
	status, failure, output := run(t, f, "--tcp", "@192.0.2.53", "a.test")
	if status != 0 || failure.Failed() || len(f.udpQueries) != 0 || !strings.Contains(output, ";; transport: TCP\n") {
		t.Fatalf("%d %+v\n%s", status, failure, output)
	}
	if f.writes < 10 || f.readCount < 10 {
		t.Fatalf("writes %d reads %d", f.writes, f.readCount)
	}
}

func TestTCPFailures(t *testing.T) {
	answer := func(query []byte) []byte { return framed(reply{answers: []rr{aAnswer("a.test")}}.answer(query)) }
	cases := map[string]func(f *fake){
		"refused":     func(f *fake) { f.tcpResult[tcpapi.OpConnect] = tcpapi.ResultConnectionRefused },
		"unreachable": func(f *fake) { f.tcpResult[tcpapi.OpConnect] = tcpapi.ResultNeighborUnreachable },
		"timeout":     func(f *fake) { f.tcpResult[tcpapi.OpConnect] = tcpapi.ResultTimeout },
		"reset":       func(f *fake) { f.tcpResult[tcpapi.OpRead] = tcpapi.ResultReset },
		"reconfig":    func(f *fake) { f.tcpResult[tcpapi.OpWrite] = tcpapi.ResultReconfigured },
		"open":        func(f *fake) { f.tcpResult[tcpapi.OpOpen] = tcpapi.ResultBusy },
		"silent":      func(f *fake) { f.tcpSilent = true },
		"eof prefix":  func(f *fake) { f.tcpCut = 1 },
		"eof body":    func(f *fake) { f.tcpCut = 20 },
		"eof at once": func(f *fake) { f.tcpAnswer = func([]byte) []byte { return nil } },
		"short prefix": func(f *fake) {
			f.tcpAnswer = func([]byte) []byte { return []byte{0, 5, 1, 2, 3, 4, 5} }
		},
		"truncated": func(f *fake) {
			f.tcpAnswer = func(query []byte) []byte { return framed(truncated(query)) }
		},
	}
	for what, setup := range cases {
		f := newFake(t)
		f.tcpAnswer = answer
		setup(f)
		status, failure, output := run(t, f, "--tcp", "@192.0.2.53", "a.test")
		if status != 1 || !failure.Failed() || output != "" {
			t.Errorf("%s: %d %+v %q", what, status, failure, output)
		}
		if what == "open" && f.tcpOpen != 0 {
			t.Errorf("open failure left an endpoint")
		}
	}
}

func TestUDPFailures(t *testing.T) {
	cases := map[string]func(f *fake){
		"no route": func(f *fake) { f.udpResult[udpapi.OpConnect] = udpapi.ResultNoRoute },
		"send":     func(f *fake) { f.udpResult[udpapi.OpSend] = udpapi.ResultNeighborUnreachable },
		"receive":  func(f *fake) { f.udpResult[udpapi.OpRecvFrom] = udpapi.ResultReconfigured },
		"silent":   func(f *fake) { f.udpSilent = true },
	}
	for what, setup := range cases {
		f := newFake(t)
		f.udpAnswer = reply{}.answer
		setup(f)
		status, failure, _ := run(t, f, "@192.0.2.53", "a.test")
		if status != 1 || !failure.Failed() || f.tcpConnects != 0 {
			t.Errorf("%s: %d %+v", what, status, failure)
		}
	}
}

// One deadline covers UDP and the fallback: time spent waiting for the
// datagram is not given back to TCP.
func TestOneDeadline(t *testing.T) {
	f := newFake(t)
	f.step = 400 * millisecond
	f.udpAnswer = truncated
	f.tcpAnswer = func(query []byte) []byte { return framed(reply{}.answer(query)) }
	var deadlines []uint64
	recorder := &deadlineFake{fake: f, deadlines: &deadlines}
	var query Query
	var out screen
	status, failure := query.Run(parsed(t, "--timeout", "3000", "@192.0.2.53", "a.test"), 1, recorder, &out)
	f.settled()
	if status != 0 {
		t.Fatalf("%d %+v", status, failure)
	}
	start := uint64(1_000_000_000)
	for _, deadline := range deadlines {
		if deadline != start+3000*millisecond {
			t.Fatalf("deadline %d", deadline-start)
		}
	}
	// UDP takes 4.5 s of the default 5: the fallback connects, and times out,
	// with the time left.
	f = newFake(t)
	f.step = 900 * millisecond
	f.udpAnswer = truncated
	f.tcpAnswer = func(query []byte) []byte { return framed(reply{}.answer(query)) }
	expired := &deadlineFake{fake: f, deadlines: &deadlines, enforce: true}
	status, failure = query.Run(parsed(t, "@192.0.2.53", "a.test"), 1, expired, &out)
	f.settled()
	if status != 1 || failure.Step != "tcp connect" || failure.Reason != "timed out" {
		t.Fatalf("%d %+v", status, failure)
	}
}

// deadlineFake records the deadline of every call but Close, and with
// enforce fails a call made after it.
type deadlineFake struct {
	*fake
	deadlines *[]uint64
	enforce   bool
}

func (d *deadlineFake) UDPCall(request *udpapi.Request, deadline uint64, reply *udpapi.Reply) bool {
	if request.Opcode != udpapi.OpClose {
		*d.deadlines = append(*d.deadlines, deadline)
		if d.enforce && d.now >= deadline {
			return false
		}
	}
	return d.fake.UDPCall(request, deadline, reply)
}

func (d *deadlineFake) TCPCall(request *tcpapi.Request, deadline uint64, reply *tcpapi.Reply) bool {
	if request.Opcode != tcpapi.OpClose {
		*d.deadlines = append(*d.deadlines, deadline)
		if d.enforce && d.now >= deadline {
			return false
		}
	}
	return d.fake.TCPCall(request, deadline, reply)
}

func TestRawOutput(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = reply{flags: dnswire.FlagAA, answers: []rr{aAnswer("a.test")},
		authority: []rr{{name: "test", kind: dnswire.TypeNS, ttl: 60, data: wire("ns.test")}}, opt: true}.answer
	status, _, output := run(t, f, "--raw", "@192.0.2.53", "a.test")
	want := `;; server: 192.0.2.53:53
;; transport: UDP
;; id: 0x9906
;; opcode: QUERY
;; rcode: NOERROR
;; flags: QR AA RD RA
;; questions: 1
;; answers: 1
;; authority: 1
;; additional: 1
;; edns: version 0 udp 1232

QUESTION
a.test.    IN    A

ANSWER
a.test.    300    IN    A    192.0.2.80

AUTHORITY
test.    60    IN    NS    ns.test.

ADDITIONAL

;; 1 answer, 82 bytes, 5 ms
`
	if status != 0 || output != want {
		t.Fatalf("got\n%s\nwant\n%s", output, want)
	}
}

func TestTXTEscaping(t *testing.T) {
	f := newFake(t)
	f.udpAnswer = reply{answers: []rr{{name: "text.test", kind: dnswire.TypeTXT, ttl: 1,
		data: []byte{9, 0x1b, ']', '0', ';', 'x', 0x07, '"', '\r', '\n'}}}}.answer
	_, _, output := run(t, f, "@192.0.2.53", "text.test", "TXT")
	if !strings.Contains(output, `TXT    "\027]0;x\007\"\013\010"`) {
		t.Fatalf("%q", output)
	}
	for _, character := range []byte(output) {
		if character != '\n' && (character < ' ' || character > '~') {
			t.Fatalf("raw byte %#x", character)
		}
	}
}
