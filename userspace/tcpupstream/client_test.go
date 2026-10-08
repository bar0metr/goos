package tcpupstream

import (
	"bytes"
	"encoding/binary"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/dns"
	"goos/userspace/hostname"
	"goos/userspace/tcpapi"
)

// These tests play netd by hand: every request the client sends is checked
// and every reply is scripted, so each split of the byte stream can be set
// up exactly.

var server = [4]byte{192, 0, 2, 53}

type sent struct {
	request     tcpapi.Request
	correlation uint64
}

type fixture struct {
	t        *testing.T
	resolver *dns.Resolver
	client   *Client
	now      uint64
	tx, rx   []byte
	out      []sent
	// datagram is the UDP query that drew TC=1.
	datagram []byte
	endpoint uint32
}

func (f *fixture) Send(request *tcpapi.Request, correlation uint64) bool {
	f.t.Helper()
	var wire ipc.Message
	if !tcpapi.EncodeRequest(request, correlation, &wire) {
		f.t.Fatalf("invalid request %+v correlation %d", request, correlation)
	}
	var decoded tcpapi.Request
	got, ok := tcpapi.DecodeRequest(&wire, &decoded)
	if !ok || got != correlation {
		f.t.Fatalf("request does not decode: %+v", request)
	}
	f.out = append(f.out, sent{decoded, got})
	return true
}

func newFixture(t *testing.T, attempts uint8) *fixture {
	t.Helper()
	f := &fixture{t: t, resolver: new(dns.Resolver), client: new(Client), now: 1_000_000_000_000,
		tx: make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), rx: make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)}
	policy := dns.DefaultPolicy()
	policy.Attempts = attempts
	f.resolver.Init(0x1234, policy)
	f.resolver.SetServers(nil, [][4]byte{server})
	f.client.Bind(f.tx, f.rx)
	return f
}

// truncated starts a lookup for text on client and answers its datagram
// with TC=1.
func (f *fixture) truncated(client int, text string) {
	f.t.Helper()
	name, _ := hostname.Parse([]byte(text))
	if _, done := f.resolver.Lookup(client, &name, f.now); done {
		f.t.Fatal("lookup answered at once")
	}
	var buffer [dns.DatagramSize]byte
	outgoing, ok := f.resolver.NextQuery(buffer[:])
	if !ok {
		f.t.Fatal("no datagram")
	}
	f.datagram = append([]byte(nil), buffer[:outgoing.Length]...)
	reply := question(f.datagram)
	binary.BigEndian.PutUint16(reply[2:], 0x8380)
	f.resolver.Receive(outgoing.Transaction, outgoing.Server, reply, f.now)
}

// question copies a query's header and question, with no other records.
func question(query []byte) []byte {
	end := 12
	for query[end] != 0 {
		end += int(query[end]) + 1
	}
	out := append([]byte(nil), query[:end+5]...)
	binary.BigEndian.PutUint16(out[10:], 0)
	return out
}

// response answers the datagram's question with address, padded with TXT
// records in the additional section to exactly size bytes when size is
// larger than the plain answer.
func (f *fixture) response(address [4]byte, size int) []byte {
	out := question(f.datagram)
	binary.BigEndian.PutUint16(out[2:], 0x8180)
	binary.BigEndian.PutUint16(out[6:], 1)
	out = append(out, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	out = append(out, address[:]...)
	additional := 0
	for len(out) < size {
		// Owner pointer, type TXT, class IN, TTL, length, then one string.
		data := min(size-len(out)-13, 255)
		if data < 0 {
			data = 0
		}
		out = append(out, 0xc0, 12, 0, 16, 0, 1, 0, 0, 0, 60)
		out = binary.BigEndian.AppendUint16(out, uint16(data+1))
		out = append(out, byte(data))
		out = append(out, make([]byte, data)...)
		additional++
	}
	binary.BigEndian.PutUint16(out[10:], uint16(additional))
	return out
}

func framed(message []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...)
}

// expect takes the next request and checks its operation.
func (f *fixture) expect(opcode tcpapi.Opcode) sent {
	f.t.Helper()
	if len(f.out) == 0 {
		f.t.Fatalf("no request, want opcode %d", opcode)
	}
	next := f.out[0]
	f.out = f.out[1:]
	if next.request.Opcode != opcode {
		f.t.Fatalf("request %+v, want opcode %d", next.request, opcode)
	}
	return next
}

func (f *fixture) quiet() {
	f.t.Helper()
	if len(f.out) != 0 {
		f.t.Fatalf("unexpected requests %+v", f.out)
	}
}

// answer passes a reply through the GTCP1 codec to the client.
func (f *fixture) answer(reply tcpapi.Reply, correlation uint64) bool {
	f.t.Helper()
	var wire ipc.Message
	if !tcpapi.EncodeReply(&reply, correlation, &wire) {
		f.t.Fatalf("invalid reply %+v", reply)
	}
	var decoded tcpapi.Reply
	if _, ok := tcpapi.DecodeReply(&wire, &decoded); !ok {
		f.t.Fatalf("reply does not decode: %+v", reply)
	}
	return f.client.Answer(f.resolver, &decoded, correlation, f.now, f)
}

func (f *fixture) accept(reply tcpapi.Reply, correlation uint64) {
	f.t.Helper()
	if !f.answer(reply, correlation) {
		f.t.Fatalf("client refused %+v", reply)
	}
}

func (f *fixture) start() {
	f.t.Helper()
	if !f.client.Start(f.resolver, f.now, f) {
		f.t.Fatal("start failed")
	}
}

// connect starts the fallback and opens and connects its endpoint.
func (f *fixture) connect() {
	f.t.Helper()
	f.start()
	open := f.expect(tcpapi.OpOpen)
	f.endpoint++
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: f.endpoint}, open.correlation)
	connect := f.expect(tcpapi.OpConnect)
	if connect.request.Endpoint != f.endpoint || connect.request.Address != server || connect.request.Port != Port {
		f.t.Fatalf("connect %+v", connect.request)
	}
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpConnect, Endpoint: f.endpoint, Address: server, Port: Port,
		Local: [4]byte{192, 0, 2, 10}, LocalPort: 49152}, connect.correlation)
}

// written accepts the next Write, taking accept bytes of it, and returns
// what it carried.
func (f *fixture) written(accept int) []byte {
	f.t.Helper()
	write := f.expect(tcpapi.OpWrite)
	offset := tcpapi.Offset(write.request.Slot)
	data := append([]byte(nil), f.tx[offset:offset+uint64(write.request.Length)]...)
	accept = min(accept, len(data))
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpWrite, Endpoint: f.endpoint, Slot: write.request.Slot, Length: uint16(accept)}, write.correlation)
	return data[:accept]
}

// read answers the next Read with data in slot.
func (f *fixture) read(slot uint8, data []byte) {
	f.t.Helper()
	read := f.expect(tcpapi.OpRead)
	if read.request.Endpoint != f.endpoint || int(read.request.Length) < len(data) {
		f.t.Fatalf("read %+v for %d bytes", read.request, len(data))
	}
	copy(f.rx[tcpapi.Offset(slot):], data)
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: f.endpoint, Slot: slot, Length: uint16(len(data))}, read.correlation)
	if release := f.expect(tcpapi.OpRelease); release.request.Slot != slot {
		f.t.Fatalf("released slot %d, want %d", release.request.Slot, slot)
	}
}

func (f *fixture) fails(read tcpapi.Result) {
	f.t.Helper()
	request := f.expect(tcpapi.OpRead)
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpRead, Result: read, Endpoint: f.endpoint}, request.correlation)
}

func (f *fixture) closed() {
	f.t.Helper()
	if close := f.expect(tcpapi.OpClose); close.request.Endpoint != f.endpoint {
		f.t.Fatalf("closed %+v", close.request)
	}
}

func (f *fixture) completion(want dns.Result) dns.Answer {
	f.t.Helper()
	_, answer, ok := f.resolver.Completion()
	if !ok || answer.Result != want {
		f.t.Fatalf("completion %+v %v, want result %d", answer, ok, want)
	}
	return answer
}

// idle checks that nothing is held: no stream, no lent TX slot.
func (f *fixture) idle() {
	f.t.Helper()
	if f.client.Live() != 0 {
		f.t.Fatalf("%d streams live", f.client.Live())
	}
	for slot := uint8(0); slot < tcpapi.TXSlots; slot++ {
		if f.client.txSlots.Lent(slot) {
			f.t.Fatalf("TX slot %d still lent", slot)
		}
	}
}

func TestFallbackWritesFramedQuery(t *testing.T) {
	f := newFixture(t, 2)
	f.truncated(0, "big.test")
	f.connect()
	if got := f.written(1 << 16); !bytes.Equal(got, framed(f.datagram)) {
		t.Fatalf("wrote % x, want the framed datagram % x", got, framed(f.datagram))
	}
	f.read(0, framed(f.response([4]byte{10, 0, 0, 1}, 0)))
	f.closed()
	f.quiet()
	answer := f.completion(dns.ResultOK)
	if answer.Count != 1 || answer.Addresses[0] != [4]byte{10, 0, 0, 1} {
		t.Fatalf("answer %+v", answer)
	}
	f.idle()
	if stats := f.resolver.Stats(); stats.TCPFallbacks != 1 || stats.TCPAnswers != 1 || stats.Queries != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestPartialWrites(t *testing.T) {
	for _, accepts := range [][]int{{1}, {1, 1}, {2}, {3, 5, 7}, {0, 1}} {
		f := newFixture(t, 2)
		f.truncated(0, "big.test")
		f.connect()
		want := framed(f.datagram)
		var got []byte
		for _, accept := range accepts {
			got = append(got, f.written(accept)...)
		}
		got = append(got, f.written(1<<16)...)
		if !bytes.Equal(got, want) {
			t.Fatalf("accepts %v: wrote % x, want % x", accepts, got, want)
		}
		f.read(0, framed(f.response([4]byte{10, 0, 0, 2}, 0)))
		f.closed()
		f.completion(dns.ResultOK)
		f.idle()
	}
}

func TestReadSplits(t *testing.T) {
	const size = 6000
	for _, test := range []struct {
		label  string
		pieces []int
	}{
		{"whole", nil},
		{"prefix byte by byte", []int{1, 1}},
		{"prefix then body", []int{2}},
		{"split prefix and body", []int{1, 101, 4096}},
		{"prefix with part of the body", []int{150}},
		{"many small", []int{3, 7, 11, 13, 17, 19, 23, 29, 31, 37}},
		{"full slots", []int{4096}},
	} {
		t.Run(test.label, func(t *testing.T) {
			f := newFixture(t, 2)
			f.truncated(0, "big.test")
			f.connect()
			f.written(1 << 16)
			stream := framed(f.response([4]byte{10, 0, 0, 3}, size))
			slot := uint8(0)
			for _, piece := range test.pieces {
				f.read(slot, stream[:piece])
				stream = stream[piece:]
				slot = (slot + 1) % tcpapi.RXSlots
			}
			for len(stream) > 0 {
				piece := min(len(stream), tcpapi.SlotSize)
				f.read(slot, stream[:piece])
				stream = stream[piece:]
				slot = (slot + 1) % tcpapi.RXSlots
			}
			f.closed()
			f.quiet()
			if answer := f.completion(dns.ResultOK); answer.Addresses[0] != [4]byte{10, 0, 0, 3} {
				t.Fatalf("answer %+v", answer)
			}
			f.idle()
		})
	}
}

// Bytes after the message are ignored: the connection is closed without
// another Read.
func TestTrailingBytesIgnored(t *testing.T) {
	f := newFixture(t, 2)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	f.read(0, append(framed(f.response([4]byte{10, 0, 0, 4}, 0)), 0, 12, 1, 2, 3))
	f.closed()
	f.quiet()
	f.completion(dns.ResultOK)
}

// Once the prefix is in, a Read asks for no more than the rest of the
// message.
func TestReadSizeFollowsPrefix(t *testing.T) {
	f := newFixture(t, 2)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	stream := framed(f.response([4]byte{10, 0, 0, 5}, 300))
	f.read(0, stream[:10])
	request := f.expect(tcpapi.OpRead)
	if int(request.request.Length) != len(stream)-10 {
		t.Fatalf("read asks %d, want %d", request.request.Length, len(stream)-10)
	}
	f.out = append([]sent{request}, f.out...)
	f.read(1, stream[10:])
	f.closed()
	f.completion(dns.ResultOK)
}

func TestReadFailures(t *testing.T) {
	for _, test := range []struct {
		label  string
		before []byte
		result tcpapi.Result
	}{
		{"eof before prefix", nil, tcpapi.ResultEOF},
		{"eof inside prefix", []byte{0}, tcpapi.ResultEOF},
		{"eof inside body", []byte{0, 40, 1, 2, 3}, tcpapi.ResultEOF},
		{"reset", []byte{0, 40}, tcpapi.ResultReset},
		{"timeout", nil, tcpapi.ResultTimeout},
		{"reconfigured", nil, tcpapi.ResultReconfigured},
	} {
		t.Run(test.label, func(t *testing.T) {
			f := newFixture(t, 1)
			f.truncated(0, "big.test")
			f.connect()
			f.written(1 << 16)
			if len(test.before) > 0 {
				f.read(0, test.before)
			}
			f.fails(test.result)
			f.closed()
			f.quiet()
			want := dns.ResultTemporaryFailure
			if test.result == tcpapi.ResultTimeout {
				want = dns.ResultTimeout
			}
			f.completion(want)
			f.idle()
			stats := f.resolver.Stats()
			if test.result == tcpapi.ResultTimeout && (stats.TCPTimeouts != 1 || stats.TCPFailures != 0) ||
				test.result != tcpapi.ResultTimeout && (stats.TCPFailures != 1 || stats.TCPTimeouts != 0) {
				t.Fatalf("stats %+v", stats)
			}
		})
	}
}

func TestBadLengthPrefix(t *testing.T) {
	for _, length := range []uint16{0, 1, dns.MinMessage - 1} {
		f := newFixture(t, 1)
		f.truncated(0, "big.test")
		f.connect()
		f.written(1 << 16)
		f.read(0, binary.BigEndian.AppendUint16(nil, length))
		f.closed()
		f.quiet()
		f.completion(dns.ResultTemporaryFailure)
		f.idle()
	}
}

// The largest message the prefix can describe is taken whole.
func TestLargestMessage(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	stream := framed(f.response([4]byte{10, 0, 0, 6}, dns.MaxStreamMessage))
	if len(stream) != dns.MaxStreamMessage+2 {
		t.Fatalf("stream %d bytes", len(stream))
	}
	for slot := uint8(0); len(stream) > 0; slot = (slot + 1) % tcpapi.RXSlots {
		piece := min(len(stream), tcpapi.SlotSize)
		f.read(slot, stream[:piece])
		stream = stream[piece:]
	}
	f.closed()
	f.completion(dns.ResultOK)
}

// A message that does not answer the query ends the fallback; nothing is
// cached and no further read waits for a better one.
func TestMismatchedMessage(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	message := f.response([4]byte{10, 0, 0, 7}, 0)
	message[0] ^= 0xff
	f.read(0, framed(message))
	f.closed()
	f.quiet()
	f.completion(dns.ResultTemporaryFailure)
	if stats := f.resolver.Stats(); stats.Mismatched != 1 || stats.Positive != 0 || stats.TCPFailures != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestWriteFailures(t *testing.T) {
	for _, result := range []tcpapi.Result{tcpapi.ResultReset, tcpapi.ResultTimeout, tcpapi.ResultReconfigured,
		tcpapi.ResultNeighborUnreachable, tcpapi.ResultClosed} {
		f := newFixture(t, 1)
		f.truncated(0, "big.test")
		f.connect()
		f.written(3)
		write := f.expect(tcpapi.OpWrite)
		f.accept(tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: result, Endpoint: f.endpoint, Slot: write.request.Slot}, write.correlation)
		f.closed()
		f.quiet()
		want := dns.ResultTemporaryFailure
		if result == tcpapi.ResultTimeout {
			want = dns.ResultTimeout
		}
		f.completion(want)
		f.idle()
	}
}

func TestConnectAndOpenFailures(t *testing.T) {
	for _, result := range []tcpapi.Result{tcpapi.ResultConnectionRefused, tcpapi.ResultNeighborUnreachable,
		tcpapi.ResultReset, tcpapi.ResultTimeout, tcpapi.ResultReconfigured} {
		f := newFixture(t, 1)
		f.truncated(0, "big.test")
		f.start()
		open := f.expect(tcpapi.OpOpen)
		f.endpoint++
		f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: f.endpoint}, open.correlation)
		connect := f.expect(tcpapi.OpConnect)
		f.accept(tcpapi.Reply{Opcode: tcpapi.OpConnect, Result: result, Endpoint: f.endpoint}, connect.correlation)
		f.closed()
		f.quiet()
		want := dns.ResultTemporaryFailure
		if result == tcpapi.ResultTimeout {
			want = dns.ResultTimeout
		}
		f.completion(want)
		f.idle()
	}

	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.start()
	open := f.expect(tcpapi.OpOpen)
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Result: tcpapi.ResultBusy}, open.correlation)
	f.quiet()
	f.completion(dns.ResultTemporaryFailure)
	f.idle()
}

// A lookup cancelled mid-exchange closes its connection, and the replies
// still in flight for it give back their slots and change nothing.
func TestCancelClosesAndLateRepliesSettle(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	read := f.expect(tcpapi.OpRead)
	f.resolver.Cancel(0)
	if !f.client.ReleaseFinished(f.resolver, f) {
		t.Fatal("release failed")
	}
	f.closed()
	copy(f.rx, framed(f.response([4]byte{10, 0, 0, 8}, 0)))
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: f.endpoint, Slot: 2, Length: 10}, read.correlation)
	if release := f.expect(tcpapi.OpRelease); release.request.Slot != 2 {
		t.Fatalf("release %+v", release.request)
	}
	f.quiet()
	if _, _, ok := f.resolver.Completion(); ok {
		t.Fatal("cancelled lookup completed")
	}
	f.idle()

	// An endpoint that opens after its lookup went away is closed.
	f.truncated(0, "big.test")
	f.start()
	open := f.expect(tcpapi.OpOpen)
	f.resolver.Cancel(0)
	f.client.ReleaseFinished(f.resolver, f)
	f.quiet()
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: 99}, open.correlation)
	if close := f.expect(tcpapi.OpClose); close.request.Endpoint != 99 {
		t.Fatalf("close %+v", close.request)
	}
	f.idle()

	// A Write reply after its stream is gone still returns the TX slot.
	f.truncated(0, "big.test")
	f.connect()
	write := f.expect(tcpapi.OpWrite)
	f.resolver.Cancel(0)
	f.client.ReleaseFinished(f.resolver, f)
	f.closed()
	f.accept(tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: tcpapi.ResultClosed, Endpoint: f.endpoint, Slot: write.request.Slot}, write.correlation)
	f.quiet()
	f.idle()
}

// The resolver's deadline ends a fallback the server never answers.
func TestDeadlineClosesStream(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	f.expect(tcpapi.OpRead)
	deadline, _ := f.resolver.Deadline()
	f.now = deadline
	f.resolver.Advance(f.now)
	f.client.ReleaseFinished(f.resolver, f)
	f.closed()
	f.completion(dns.ResultTimeout)
	f.idle()
}

func TestUnboundFallbacksFail(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.client.Unbind(f.resolver, f.now)
	f.out = nil
	f.completion(dns.ResultTemporaryFailure)
	f.idle()
	if f.resolver.Stats().TCPFailures != 1 {
		t.Fatalf("stats %+v", f.resolver.Stats())
	}

	// Without a session a fallback fails at once.
	f.truncated(1, "other.test")
	f.start()
	f.quiet()
	f.completion(dns.ResultTemporaryFailure)
}

// More truncated lookups than streams wait for a free one; repeated rounds
// leave nothing behind.
func TestStreamsBounded(t *testing.T) {
	f := newFixture(t, 2)
	for round := 0; round < 20; round++ {
		names := []string{"a.test", "b.test", "c.test"}
		datagrams := make([][]byte, len(names))
		for index, text := range names {
			f.truncated(index, text)
			datagrams[index] = f.datagram
		}
		f.start()
		opens := []sent{f.expect(tcpapi.OpOpen), f.expect(tcpapi.OpOpen)}
		f.quiet()
		if f.client.Live() != MaxStreams {
			t.Fatalf("live %d", f.client.Live())
		}
		for index, open := range opens {
			f.endpoint++
			f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: f.endpoint}, open.correlation)
			connect := f.expect(tcpapi.OpConnect)
			f.accept(tcpapi.Reply{Opcode: tcpapi.OpConnect, Endpoint: f.endpoint, Address: server, Port: Port,
				Local: [4]byte{192, 0, 2, 10}, LocalPort: 49152}, connect.correlation)
			f.written(1 << 16)
			f.datagram = datagrams[index]
			f.read(0, framed(f.response([4]byte{10, 0, 1, byte(index)}, 0)))
			f.closed()
		}
		f.start()
		f.endpoint++
		f.accept(tcpapi.Reply{Opcode: tcpapi.OpOpen, Endpoint: f.endpoint}, f.expect(tcpapi.OpOpen).correlation)
		connect := f.expect(tcpapi.OpConnect)
		f.accept(tcpapi.Reply{Opcode: tcpapi.OpConnect, Endpoint: f.endpoint, Address: server, Port: Port,
			Local: [4]byte{192, 0, 2, 10}, LocalPort: 49152}, connect.correlation)
		f.written(1 << 16)
		f.datagram = datagrams[2]
		f.read(0, framed(f.response([4]byte{10, 0, 1, 2}, 0)))
		f.closed()
		f.quiet()
		for range names {
			f.completion(dns.ResultOK)
		}
		f.idle()
		f.resolver.Flush()
	}
}

func TestProtocolViolations(t *testing.T) {
	f := newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	write := f.expect(tcpapi.OpWrite)
	reply := tcpapi.Reply{Opcode: tcpapi.OpWrite, Endpoint: f.endpoint, Slot: write.request.Slot, Length: write.request.Length + 1}
	if f.answer(reply, write.correlation) {
		t.Fatal("took a Write of more bytes than sent")
	}

	f = newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	if f.answer(tcpapi.Reply{Opcode: tcpapi.OpWrite, Endpoint: f.endpoint, Slot: 3}, 12345) {
		t.Fatal("took back a TX slot it never lent")
	}

	f = newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	write = f.expect(tcpapi.OpWrite)
	if f.answer(tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: f.endpoint, Slot: 0, Length: 1}, write.correlation) {
		t.Fatal("took a Read reply for a Write")
	}

	f = newFixture(t, 1)
	f.truncated(0, "big.test")
	f.connect()
	f.written(1 << 16)
	f.read(0, []byte{0, 100})
	read := f.expect(tcpapi.OpRead)
	if f.answer(tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: f.endpoint, Slot: 0, Length: 101}, read.correlation) {
		t.Fatal("took more bytes than asked for")
	}
}

func FuzzStreamPieces(f *testing.F) {
	f.Add([]byte{1, 1, 50, 200}, uint16(0))
	f.Add([]byte{255}, uint16(9))
	f.Fuzz(func(t *testing.T, cuts []byte, prefix uint16) {
		x := newFixture(t, 1)
		x.truncated(0, "big.test")
		x.connect()
		x.written(1 << 16)
		stream := framed(x.response([4]byte{10, 0, 0, 9}, 700))
		if prefix != 0 {
			binary.BigEndian.PutUint16(stream, prefix)
		}
		for index := 0; len(stream) > 0 && x.client.Live() > 0; index++ {
			piece := tcpapi.SlotSize
			if index < len(cuts) {
				piece = int(cuts[index]) + 1
			}
			request := x.expect(tcpapi.OpRead)
			piece = min(piece, len(stream), int(request.request.Length))
			copy(x.rx, stream[:piece])
			x.accept(tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: x.endpoint, Length: uint16(piece)}, request.correlation)
			x.expect(tcpapi.OpRelease)
			stream = stream[piece:]
		}
		if x.client.Live() != 0 {
			x.fails(tcpapi.ResultEOF)
		}
		x.idle()
		if _, _, ok := x.resolver.Completion(); !ok {
			t.Fatal("lookup never finished")
		}
	})
}
