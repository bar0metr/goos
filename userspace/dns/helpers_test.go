package dns

import (
	"encoding/binary"
	"strings"
	"testing"

	"goos/userspace/hostname"
)

var (
	serverA = [4]byte{192, 0, 2, 53}
	serverB = [4]byte{198, 51, 100, 53}
	serverC = [4]byte{203, 0, 113, 53}
)

// rr is one resource record for the test encoder. A zero class means IN.
type rr struct {
	name  string
	kind  uint16
	class uint16
	ttl   uint32
	data  []byte
}

func aRecord(name string, ttl uint32, address [4]byte) rr {
	return rr{name: name, kind: typeA, ttl: ttl, data: address[:]}
}

func cnameRecord(name, target string, ttl uint32) rr {
	return rr{name: name, kind: typeCNAME, ttl: ttl, data: wire(target)}
}

func soaRecord(zone string, ttl, minimum uint32) rr {
	suffix := "." + zone
	if zone == "." {
		suffix = ""
	}
	data := append(wire("ns"+suffix), wire("admin"+suffix)...)
	fixed := make([]byte, soaFixed)
	binary.BigEndian.PutUint32(fixed[0:], 1)
	binary.BigEndian.PutUint32(fixed[4:], 3600)
	binary.BigEndian.PutUint32(fixed[8:], 600)
	binary.BigEndian.PutUint32(fixed[12:], 86400)
	binary.BigEndian.PutUint32(fixed[16:], minimum)
	return rr{name: zone, kind: typeSOA, ttl: ttl, data: append(data, fixed...)}
}

// wire encodes a dotted name without compression. "" and "." are the root.
func wire(name string) []byte {
	var out []byte
	name = strings.TrimSuffix(name, ".")
	if name != "" {
		for _, label := range strings.Split(name, ".") {
			out = append(out, byte(len(label)))
			out = append(out, label...)
		}
	}
	return append(out, 0)
}

// reply is a test response. flags holds everything but QR and the ID; the
// rcode goes in its low bits.
type reply struct {
	id         uint16
	flags      uint16
	question   string
	qtype      uint16
	noQuestion bool
	answers    []rr
	authority  []rr
	additional []rr
	opt        bool
}

func (m reply) bytes() []byte {
	out := make([]byte, headerSize)
	binary.BigEndian.PutUint16(out[0:], m.id)
	binary.BigEndian.PutUint16(out[2:], flagQR|flagRD|0x0080|m.flags)
	if !m.noQuestion {
		binary.BigEndian.PutUint16(out[4:], 1)
		out = append(out, wire(m.question)...)
		qtype := m.qtype
		if qtype == 0 {
			qtype = typeA
		}
		out = binary.BigEndian.AppendUint16(out, qtype)
		out = binary.BigEndian.AppendUint16(out, classIN)
	}
	additional := m.additional
	if m.opt {
		additional = append(additional, rr{name: ".", kind: typeOPT, class: DatagramSize})
	}
	binary.BigEndian.PutUint16(out[6:], uint16(len(m.answers)))
	binary.BigEndian.PutUint16(out[8:], uint16(len(m.authority)))
	binary.BigEndian.PutUint16(out[10:], uint16(len(additional)))
	for _, section := range [][]rr{m.answers, m.authority, additional} {
		for _, record := range section {
			class := record.class
			if class == 0 {
				class = classIN
			}
			out = append(out, wire(record.name)...)
			out = binary.BigEndian.AppendUint16(out, record.kind)
			out = binary.BigEndian.AppendUint16(out, class)
			out = binary.BigEndian.AppendUint32(out, record.ttl)
			out = binary.BigEndian.AppendUint16(out, uint16(len(record.data)))
			out = append(out, record.data...)
		}
	}
	return out
}

// query is a datagram NextQuery produced, decoded.
type query struct {
	Outgoing
	id      uint16
	name    string
	opt     bool
	payload uint16
	flags   uint16
	raw     []byte
}

type harness struct {
	t   testing.TB
	r   *Resolver
	now uint64
	buf [DatagramSize]byte
}

func newHarness(t testing.TB, servers ...[4]byte) *harness {
	t.Helper()
	h := &harness{t: t, r: new(Resolver), now: 1_000 * second}
	h.r.Init(0x1234_5678_9abc_def0, DefaultPolicy())
	h.r.SetServers(nil, servers)
	return h
}

func name(t testing.TB, text string) *hostname.Name {
	t.Helper()
	parsed, ok := hostname.Parse([]byte(text))
	if !ok {
		t.Fatalf("bad test name %q", text)
	}
	return &parsed
}

func (h *harness) lookup(client int, text string) (Answer, bool) {
	h.t.Helper()
	answer, done := h.r.Lookup(client, name(h.t, text), h.now)
	h.checkDeadline()
	return answer, done
}

// pending starts a lookup that must wait for upstream.
func (h *harness) pending(client int, text string) {
	h.t.Helper()
	if answer, done := h.lookup(client, text); done {
		h.t.Fatalf("lookup %q answered at once: %+v", text, answer)
	}
}

func (h *harness) checkDeadline() {
	h.t.Helper()
	if deadline, ok := h.r.Deadline(); ok && deadline <= h.now {
		h.t.Fatalf("deadline %d not after now %d", deadline, h.now)
	}
}

func (h *harness) advance(delay uint64) {
	h.t.Helper()
	h.now += delay
	h.r.Advance(h.now)
	h.checkDeadline()
}

// toDeadline jumps to the next timer and runs it.
func (h *harness) toDeadline() {
	h.t.Helper()
	deadline, ok := h.r.Deadline()
	if !ok {
		h.t.Fatal("no deadline")
	}
	h.now = deadline
	h.r.Advance(h.now)
	h.checkDeadline()
}

// sent takes the next query and decodes it.
func (h *harness) sent() query {
	h.t.Helper()
	out, ok := h.r.NextQuery(h.buf[:])
	if !ok {
		h.t.Fatal("no query to send")
	}
	return decodeQuery(h.t, out, h.buf[:out.Length])
}

func (h *harness) noQuery() {
	h.t.Helper()
	if out, ok := h.r.NextQuery(h.buf[:]); ok {
		h.t.Fatalf("unexpected query %+v", out)
	}
}

func decodeQuery(t testing.TB, out Outgoing, data []byte) query {
	t.Helper()
	q := query{Outgoing: out, raw: append([]byte(nil), data...)}
	if len(data) < headerSize {
		t.Fatalf("short query %d", len(data))
	}
	q.id = binary.BigEndian.Uint16(data[0:])
	q.flags = binary.BigEndian.Uint16(data[2:])
	if binary.BigEndian.Uint16(data[4:]) != 1 || binary.BigEndian.Uint16(data[6:]) != 0 ||
		binary.BigEndian.Uint16(data[8:]) != 0 {
		t.Fatalf("query counts % x", data[:headerSize])
	}
	position := headerSize
	var labels []string
	for data[position] != 0 {
		length := int(data[position])
		labels = append(labels, string(data[position+1:position+1+length]))
		position += 1 + length
	}
	position++
	q.name = strings.Join(labels, ".")
	if binary.BigEndian.Uint16(data[position:]) != typeA || binary.BigEndian.Uint16(data[position+2:]) != classIN {
		t.Fatalf("query type/class % x", data[position:position+4])
	}
	position += questionFixed
	switch binary.BigEndian.Uint16(data[10:]) {
	case 0:
	case 1:
		if data[position] != 0 || binary.BigEndian.Uint16(data[position+1:]) != typeOPT ||
			binary.BigEndian.Uint32(data[position+5:]) != 0 || binary.BigEndian.Uint16(data[position+9:]) != 0 {
			t.Fatalf("bad OPT % x", data[position:])
		}
		q.opt = true
		q.payload = binary.BigEndian.Uint16(data[position+3:])
		position += optRecordSize
	default:
		t.Fatal("additional count")
	}
	if position != len(data) {
		t.Fatalf("query has %d trailing bytes", len(data)-position)
	}
	return q
}

// answer completes m with the query's ID and question and delivers it.
func (h *harness) answer(q query, m reply) {
	h.t.Helper()
	m.id = q.id
	if m.question == "" {
		m.question = q.name
	}
	h.deliver(q, m.bytes())
}

func (h *harness) deliver(q query, payload []byte) {
	h.t.Helper()
	h.r.Receive(q.Transaction, q.Server, payload, h.now)
	h.checkDeadline()
}

func (h *harness) completion(client int) Answer {
	h.t.Helper()
	got, answer, ok := h.r.Completion()
	if !ok {
		h.t.Fatalf("no completion, want client %d", client)
	}
	if got != client {
		h.t.Fatalf("completion for client %d, want %d", got, client)
	}
	return answer
}

func (h *harness) noCompletion() {
	h.t.Helper()
	if client, answer, ok := h.r.Completion(); ok {
		h.t.Fatalf("unexpected completion client %d %+v", client, answer)
	}
}

func expectResult(t testing.TB, answer Answer, result Result, source Source) {
	t.Helper()
	if answer.Result != result || answer.Source != source {
		t.Fatalf("answer %+v, want result %d source %d", answer, result, source)
	}
}

func expectAddresses(t testing.TB, answer Answer, want ...[4]byte) {
	t.Helper()
	if int(answer.Count) != len(want) {
		t.Fatalf("answer has %d addresses, want %d: %+v", answer.Count, len(want), answer)
	}
	for index := range want {
		if answer.Addresses[index] != want[index] {
			t.Fatalf("address %d is %v, want %v", index, answer.Addresses[index], want[index])
		}
	}
}

// resolve runs one lookup for client 0 through a single answer.
func (h *harness) resolve(text string, m reply) Answer {
	h.t.Helper()
	h.pending(0, text)
	h.answer(h.sent(), m)
	return h.completion(0)
}

// cachedOK checks that text answers from the cache.
func (h *harness) cachedOK(text string) {
	h.t.Helper()
	answer, done := h.lookup(MaxClients-1, text)
	if !done || answer.Source != SourceCache {
		h.t.Fatalf("%s not cached: %+v", text, answer)
	}
}

// notCached checks that text needs a query, then cancels the lookup.
func (h *harness) notCached(text string) {
	h.t.Helper()
	if answer, done := h.lookup(MaxClients-1, text); done {
		h.t.Fatalf("%s answered at once: %+v", text, answer)
	}
	h.r.Cancel(MaxClients - 1)
}

func addr(last byte) [4]byte { return [4]byte{10, 1, 1, last} }
