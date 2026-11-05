package network

import (
	"bytes"
	"testing"
)

// pattern returns length stream bytes starting at stream offset start.
func pattern(start, length int) []byte {
	out := make([]byte, length)
	for index := range out {
		position := start + index
		out[index] = byte(position*31 + position>>9)
	}
	return out
}

func TestTCPRingSpanHelpers(t *testing.T) {
	var ring [16]byte
	ringStore(ring[:], 12, 2, []byte("abcdef"))
	if string(ring[14:]) != "ab" || string(ring[:4]) != "cdef" {
		t.Fatalf("store across the end: %q", ring)
	}
	first, second := ringSpan(ring[:], 12, 2, 6)
	if string(first) != "ab" || string(second) != "cdef" {
		t.Fatalf("span %q %q", first, second)
	}
	first, second = ringSpan(ring[:], 12, 4, 4)
	if string(first) != "cdef" || second != nil {
		t.Fatalf("span past the end %q %q", first, second)
	}
	var dst [8]byte
	if n := ringLoad(dst[:], ring[:], 14, 6); string(dst[:n]) != "abcdef" {
		t.Fatalf("load %q", dst[:n])
	}
	if head := ringAdvance(14, 6, 16); head != 4 {
		t.Fatalf("advance %d", head)
	}
}

// peerData is a data segment from the peer at the next expected sequence.
func peerData(entry *tcpEndpoint, offset uint32, payload []byte, flags uint8) tcpSegment {
	return tcpSegment{sequence: entry.rcvNxt + offset, acknowledgment: entry.sndNxt, flags: tcpFlagACK | flags,
		window: 60000, payload: payload}
}

func joinPayloads(segments []tcpSegment) []byte {
	var out []byte
	for index := range segments {
		out = append(out, segments[index].payload...)
	}
	return out
}

func TestTCPSendRingWrapAckAndRetransmit(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50100)
	entry.peerWindow = 60000
	entry.sendHead = TCPSendCapacity - 700
	data := pattern(0, 2000)
	if written, result := h.stack.TCPWrite(0, id, data, h.now); result != ResultOK || written != len(data) {
		t.Fatalf("write %d %d", written, result)
	}
	sent := h.tcpSent()
	if len(sent) != 2 || len(sent[0].payload) != 1460 || sent[0].sequence != 100 || sent[1].sequence != 1560 {
		t.Fatalf("segments %d", len(sent))
	}
	if !bytes.Equal(joinPayloads(sent), data) {
		t.Fatal("sent bytes differ across the wrap")
	}
	if h.stack.tcpStats.SplitSegments != 1 {
		t.Fatalf("split segments %d", h.stack.tcpStats.SplitSegments)
	}

	// A partial ACK moves the head without moving bytes.
	h.tcpFromPeer(entry, ackSegment(entry, 600, 60000))
	if entry.sendHead != TCPSendCapacity-200 || entry.sendLen != 1500 || entry.sendSent != 1500 || entry.sendSeq != 600 {
		t.Fatalf("partial ACK head=%d len=%d sent=%d seq=%d", entry.sendHead, entry.sendLen, entry.sendSent, entry.sendSeq)
	}
	h.drain()

	// The RTO resends the oldest bytes by sequence, across the end of the ring.
	h.advance(entry.rtoDeadline - h.now)
	sent = h.tcpSent()
	if len(sent) != 1 || sent[0].sequence != 600 || !bytes.Equal(sent[0].payload, data[500:1960]) {
		t.Fatalf("retransmit %d segments", len(sent))
	}
	if h.stack.tcpStats.SplitSegments != 2 || h.stack.tcpStats.Retransmits != 1 {
		t.Fatalf("split=%d retransmits=%d", h.stack.tcpStats.SplitSegments, h.stack.tcpStats.Retransmits)
	}

	// An ACK across the end of the ring leaves the head near the start.
	h.tcpFromPeer(entry, ackSegment(entry, 1600, 60000))
	if entry.sendHead != 800 || entry.sendLen != 500 || entry.sendSeq != 1600 {
		t.Fatalf("ACK across wrap head=%d len=%d seq=%d", entry.sendHead, entry.sendLen, entry.sendSeq)
	}
	h.drain()
	h.tcpFromPeer(entry, ackSegment(entry, 2100, 60000))
	if entry.sendHead != 0 || entry.sendLen != 0 || entry.sendSent != 0 || entry.sndUna != entry.sndNxt {
		t.Fatalf("final ACK head=%d len=%d", entry.sendHead, entry.sendLen)
	}
}

func TestTCPSendRingFillsAfterWrap(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50101)
	entry.peerWindow = 0
	entry.sendHead = 5000
	data := pattern(0, TCPSendCapacity+10)
	written, result := h.stack.TCPWrite(0, id, data, h.now)
	if result != ResultOK || written != TCPSendCapacity {
		t.Fatalf("write %d %d", written, result)
	}
	if _, result = h.stack.TCPWrite(0, id, data[:1], h.now); result != ResultBusy {
		t.Fatalf("write to a full ring %d", result)
	}
	if !bytes.Equal(entry.send[:5000], data[TCPSendCapacity-5000:TCPSendCapacity]) ||
		!bytes.Equal(entry.send[5000:], data[:TCPSendCapacity-5000]) {
		t.Fatal("ring layout")
	}
}

func TestTCPPersistProbeWithWrappedSendData(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50102)
	entry.peerWindow = 0
	entry.sendHead = TCPSendCapacity - 1
	if written, result := h.stack.TCPWrite(0, id, []byte("xyz"), h.now); result != ResultOK || written != 3 {
		t.Fatalf("write %d %d", written, result)
	}
	h.stack.advanceTCP(h.now)
	if entry.rtoDeadline == 0 {
		t.Fatal("persist timer not armed")
	}
	h.advance(entry.rtoDeadline - h.now)
	sent := h.tcpSent()
	if len(sent) != 1 || sent[0].sequence != 100 || string(sent[0].payload) != "x" || entry.sndNxt != 100 || entry.persist != 1 {
		t.Fatalf("probe %d segments persist=%d", len(sent), entry.persist)
	}
	h.tcpFromPeer(entry, ackSegment(entry, 100, 4096))
	sent = h.tcpSent()
	if len(sent) != 1 || sent[0].sequence != 100 || string(sent[0].payload) != "xyz" || entry.sndNxt != 103 || entry.persist != 0 {
		t.Fatalf("resume %d segments", len(sent))
	}
	if h.stack.tcpStats.SplitSegments != 1 {
		t.Fatalf("split segments %d", h.stack.tcpStats.SplitSegments)
	}
}

func TestTCPReceiveRingWrapReadAndWindow(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50103)
	entry.recvHead = TCPReceiveCapacity - 100
	var stream []byte
	for index := 0; index < 12; index++ {
		chunk := pattern(len(stream), 1360)
		h.tcpFromPeer(entry, peerData(entry, 0, chunk, 0))
		stream = append(stream, chunk...)
		sent := h.tcpSent()
		want := TCPReceiveCapacity - len(stream)
		if len(sent) != 1 || int(sent[0].window) != want || stack(h).tcpAdvertisedWindow(entry) != want {
			t.Fatalf("chunk %d window %d want %d", index, sent[0].window, want)
		}
	}
	if int(entry.recvLen) != len(stream) {
		t.Fatalf("recvLen %d", entry.recvLen)
	}
	ready, _ := h.stack.TCPReadiness(0, id)
	if ready.Readable != uint32(len(stream)) {
		t.Fatalf("readable %d", ready.Readable)
	}

	peek := make([]byte, TCPReceiveCapacity)
	if n, result := h.stack.TCPPeek(0, id, peek); result != ResultOK || !bytes.Equal(peek[:n], stream) {
		t.Fatalf("peek %d %d", n, result)
	}
	if n, result := h.stack.TCPPeek(0, id, peek[:150]); result != ResultOK || !bytes.Equal(peek[:n], stream[:150]) {
		t.Fatalf("short peek across the end %d %d", n, result)
	}

	var got []byte
	updates := 0
	sizes := []int{7, 500, 4999, 1, 3333}
	for _, size := range sizes {
		buffer := make([]byte, size)
		n, result := h.stack.TCPRead(0, id, buffer, h.now)
		if result != ResultOK || n != size {
			t.Fatalf("read %d %d", n, result)
		}
		got = append(got, buffer[:n]...)
		// A read sends a window update only once the window opened by a
		// segment or half the buffer; one that goes names the window exactly.
		sent := h.tcpSent()
		want := TCPReceiveCapacity - (len(stream) - len(got))
		if len(sent) > 1 || len(sent) == 1 && int(sent[0].window) != want {
			t.Fatalf("after a read of %d: %d updates, window %v want %d", size, len(sent), sent, want)
		}
		updates += len(sent)
	}
	if updates == 0 {
		t.Fatal("reads that opened the window sent no update")
	}
	if entry.recvHead != uint16(TCPReceiveCapacity-100+len(got)-TCPReceiveCapacity) {
		t.Fatalf("head %d", entry.recvHead)
	}
	// New data lands after the wrapped tail while unread bytes remain.
	more := pattern(len(stream), 1000)
	h.tcpFromPeer(entry, peerData(entry, 0, more, 0))
	stream = append(stream, more...)
	h.drain()
	rest := make([]byte, TCPReceiveCapacity)
	n, result := h.stack.TCPRead(0, id, rest, h.now)
	got = append(got, rest[:n]...)
	if result != ResultOK || !bytes.Equal(got, stream) {
		t.Fatalf("stream differs after %d bytes", len(got))
	}
	if entry.recvLen != 0 || entry.recvHead != 0 {
		t.Fatalf("empty ring head=%d len=%d", entry.recvHead, entry.recvLen)
	}
}

func stack(h *harness) *Stack { return h.stack }

func TestTCPOutOfOrderPromotionIntoWrappedSpace(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50104)
	entry.recvHead = TCPReceiveCapacity - 4
	h.tcpFromPeer(entry, peerData(entry, 3, []byte("defgh"), 0))
	if entry.recvLen != 0 || !entry.reorder[0].used {
		t.Fatal("held segment delivered early")
	}
	sent := h.tcpSent()
	if len(sent) != 1 || int(sent[0].window) != TCPReceiveCapacity-5 {
		t.Fatalf("window with a held segment %d", sent[0].window)
	}
	h.tcpFromPeer(entry, peerData(entry, 0, []byte("abc"), 0))
	if entry.recvLen != 8 || entry.reorder[0].used || string(entry.receive[TCPReceiveCapacity-4:]) != "abcd" ||
		string(entry.receive[:4]) != "efgh" {
		t.Fatalf("promotion len=%d", entry.recvLen)
	}
	var buffer [16]byte
	if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); result != ResultOK || string(buffer[:n]) != "abcdefgh" {
		t.Fatalf("read %q %d", buffer[:n], result)
	}
}

func TestTCPFINAtAndAcrossRingEnd(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
	}{{"at the end", "hello"}, {"across the end", "hello world"}} {
		t.Run(test.name, func(t *testing.T) {
			h := readyWithPeer(t)
			id, entry := establishedWithPeer(t, h, 50105)
			entry.recvHead = TCPReceiveCapacity - 5
			h.tcpFromPeer(entry, peerData(entry, 0, []byte(test.payload), tcpFlagFIN))
			if !entry.finReceived || entry.state != TCPCloseWait || entry.rcvNxt != 200+uint32(len(test.payload))+1 {
				t.Fatalf("FIN state=%d rcvNxt=%d", entry.state, entry.rcvNxt)
			}
			var buffer [32]byte
			if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); result != ResultOK || string(buffer[:n]) != test.payload {
				t.Fatalf("read %q %d", buffer[:n], result)
			}
			if _, result := h.stack.TCPRead(0, id, buffer[:], h.now); result != ResultEOF {
				t.Fatalf("EOF %d", result)
			}
		})
	}
}

func TestTCPHeldFINClipsAcrossRingEnd(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50106)
	entry.recvHead = TCPReceiveCapacity - 2
	// "lo" and the FIN wait for the gap; a later copy of the start carries
	// bytes past the FIN, which the stream must not take.
	h.tcpFromPeer(entry, peerData(entry, 3, []byte("lo"), tcpFlagFIN))
	if !entry.peerFIN || entry.peerFINAt != 205 || entry.finReceived {
		t.Fatalf("held FIN peerFIN=%v at=%d", entry.peerFIN, entry.peerFINAt)
	}
	h.tcpFromPeer(entry, peerData(entry, 0, []byte("helloXYZ"), 0))
	if !entry.finReceived || entry.rcvNxt != 206 || entry.recvLen != 5 {
		t.Fatalf("FIN after promotion rcvNxt=%d len=%d", entry.rcvNxt, entry.recvLen)
	}
	var buffer [16]byte
	if n, result := h.stack.TCPRead(0, id, buffer[:], h.now); result != ResultOK || string(buffer[:n]) != "hello" {
		t.Fatalf("read %q %d", buffer[:n], result)
	}
}

// TestTCPLoopbackLongTransfer moves several MiB each way through loopback
// with odd write and read sizes, so both rings wrap many times.
func TestTCPLoopbackLongTransfer(t *testing.T) {
	const total = 4 << 20
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	if _, _, result := h.stack.TCPBind(0, listener, LoopbackAddress, 8090); result != ResultOK {
		t.Fatal(result)
	}
	if result := h.stack.TCPListen(0, listener, 1); result != ResultOK {
		t.Fatal(result)
	}
	client, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 8090, h.now); result != ResultOK {
		t.Fatal(result)
	}
	server, _, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK {
		t.Fatal(result)
	}
	type side struct {
		owner          int
		id             TCPEndpointID
		written, read  int
		writes, reads  int
		writeSizes     []int
		readSizes      []int
		mismatchOffset int
	}
	sides := [2]side{
		{owner: 1, id: client, writeSizes: []int{1, 7, 333, 1461, 4099, 9001}, readSizes: []int{3, 517, 1999, 7777, 13}},
		{owner: 0, id: server, writeSizes: []int{2921, 5, 16383, 77}, readSizes: []int{1460, 1, 4097, 611, 9999}},
	}
	source := pattern(0, total+16384)
	buffer := make([]byte, 16384)
	for stalls := 0; sides[0].read < total || sides[1].read < total; {
		progress := false
		for index := range sides {
			writer := &sides[index]
			if writer.written < total {
				size := min(writer.writeSizes[writer.writes%len(writer.writeSizes)], total-writer.written)
				n, result := h.stack.TCPWrite(writer.owner, writer.id, source[writer.written:writer.written+size], h.now)
				if result != ResultOK && result != ResultBusy {
					t.Fatalf("write %d", result)
				}
				writer.written += n
				writer.writes++
				progress = progress || n != 0
			}
			reader := &sides[1-index]
			size := reader.readSizes[reader.reads%len(reader.readSizes)]
			n, result := h.stack.TCPRead(reader.owner, reader.id, buffer[:size], h.now)
			if result != ResultOK && result != ResultBusy {
				t.Fatalf("read %d", result)
			}
			if !bytes.Equal(buffer[:n], source[reader.read:reader.read+n]) {
				t.Fatalf("owner %d stream differs near offset %d", reader.owner, reader.read)
			}
			reader.read += n
			reader.reads++
			progress = progress || n != 0
		}
		if progress {
			stalls = 0
			continue
		}
		if stalls++; stalls > 100 {
			t.Fatalf("transfer stalled at %d and %d bytes", sides[0].read, sides[1].read)
		}
		h.advance(second)
	}
	// A local peer takes each segment whole, so none is split: segments
	// stop at the end of the ring and the rest follows in the next one.
	if h.stack.tcpStats.SplitSegments != 0 || h.stack.tcpStats.LocalSegments == 0 {
		t.Fatalf("split %d local %d", h.stack.tcpStats.SplitSegments, h.stack.tcpStats.LocalSegments)
	}
	for index := range sides {
		if result := h.stack.TCPShutdownWrite(sides[index].owner, sides[index].id, h.now); result != ResultOK {
			t.Fatal(result)
		}
	}
	for index := range sides {
		if _, result := h.stack.TCPRead(sides[index].owner, sides[index].id, buffer, h.now); result != ResultEOF {
			t.Fatalf("EOF %d", result)
		}
	}
	t.Logf("split segments %d, retransmits %d", h.stack.tcpStats.SplitSegments, h.stack.tcpStats.Retransmits)
}

// localScenario runs one loopback connection through a handshake, streams
// of odd sizes both ways that wrap both rings, a half close each way and
// TIME_WAIT, and returns the stack's endpoint and tombstone tables.
func localScenario(t *testing.T, wire bool) ([len(Stack{}.tcp)]tcpEndpoint, TCPStats) {
	h := newHarness(t)
	if wire {
		h.stack.localWire = make([]byte, 1<<17)
	}
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, LoopbackAddress, 8091)
	h.stack.TCPListen(0, listener, 1)
	client, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, client, LoopbackAddress, 8091, h.now); result != ResultOK {
		t.Fatal(result)
	}
	server, _, result := h.stack.TCPAccept(0, listener)
	if result != ResultOK {
		t.Fatal(result)
	}
	source := pattern(0, 1<<20)
	buffer := make([]byte, 20000)
	sizes := []int{1, 16384, 333, 9001, 5000, 16000}
	moved := 0
	for round := 0; moved < 600000; round++ {
		size := sizes[round%len(sizes)]
		n, _ := h.stack.TCPWrite(1, client, source[moved:moved+size], h.now)
		back, _ := h.stack.TCPWrite(0, server, source[:round%7+1], h.now)
		h.stack.TCPRead(0, server, buffer[:sizes[(round+1)%len(sizes)]], h.now)
		h.stack.TCPRead(1, client, buffer[:back+3], h.now)
		moved += n
		h.advance(1_000_000)
	}
	h.stack.TCPShutdownWrite(1, client, h.now)
	h.stack.TCPShutdownWrite(0, server, h.now)
	for range 4 {
		h.stack.TCPRead(0, server, buffer, h.now)
		h.stack.TCPRead(1, client, buffer, h.now)
		h.advance(1_000_000)
	}
	h.stack.TCPClose(1, client, h.now)
	h.stack.TCPClose(0, server, h.now)
	return h.stack.tcp, h.stack.tcpStats
}

// TestLocalSegmentsMatchParsedOnes runs the same connection with local
// segments handed over and with each one built as a packet and parsed: the
// endpoints end in exactly the same state.
func TestLocalSegmentsMatchParsedOnes(t *testing.T) {
	logical, logicalStats := localScenario(t, false)
	parsed, parsedStats := localScenario(t, true)
	for index := range logical {
		if logical[index] != parsed[index] {
			t.Fatalf("endpoint %d differs:\nlogical %+v\nparsed  %+v", index, logical[index], parsed[index])
		}
	}
	if logicalStats.LocalSegments == 0 || parsedStats.LocalSegments != 0 || logicalStats.TX != parsedStats.TX || logicalStats.RX != parsedStats.RX {
		t.Fatalf("stats logical %+v parsed %+v", logicalStats, parsedStats)
	}
}

// TestLocalSegmentCarriesWholeSlot sends one 16 KiB write to a loopback peer
// as one segment.
func TestLocalSegmentCarriesWholeSlot(t *testing.T) {
	h := newHarness(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, LoopbackAddress, 8092)
	h.stack.TCPListen(0, listener, 1)
	client, _ := h.stack.TCPOpen(1)
	h.stack.TCPConnect(1, client, LoopbackAddress, 8092, h.now)
	server, _, _ := h.stack.TCPAccept(0, listener)
	before := h.stack.tcpStats.LocalSegments
	data := pattern(0, TCPSendCapacity)
	if n, result := h.stack.TCPWrite(1, client, data, h.now); result != ResultOK || n != len(data) {
		t.Fatalf("write %d %d", n, result)
	}
	// One data segment and one ACK.
	if sent := h.stack.tcpStats.LocalSegments - before; sent != 2 {
		t.Fatalf("%d local segments for one 16 KiB write", sent)
	}
	buffer := make([]byte, TCPReceiveCapacity)
	if n, _ := h.stack.TCPRead(0, server, buffer, h.now); n != len(data) || !bytes.Equal(buffer[:n], data) {
		t.Fatalf("read %d", n)
	}
	// More than a segment: still in order.
	big := pattern(5, 3*TCPSendCapacity)
	got := 0
	for sent := 0; got < len(big); {
		n, _ := h.stack.TCPWrite(1, client, big[sent:], h.now)
		sent += n
		m, _ := h.stack.TCPRead(0, server, buffer, h.now)
		if !bytes.Equal(buffer[:m], big[got:got+m]) {
			t.Fatalf("stream differs at %d", got)
		}
		got += m
	}
}
