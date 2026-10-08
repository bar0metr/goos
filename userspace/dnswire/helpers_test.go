package dnswire

import (
	"encoding/binary"
	"strings"
	"testing"
)

// wire encodes a dotted name without compression.
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

func named(t testing.TB, text string) *Name {
	t.Helper()
	var name Name
	if !name.SetText([]byte(text)) {
		t.Fatalf("name %q", text)
	}
	return &name
}

// rr is one record for the test encoder. A zero class means IN.
type rr struct {
	name  string
	kind  uint16
	class uint16
	ttl   uint32
	data  []byte
}

// message is a test response with question name and qtype.
type message struct {
	id                        uint16
	flags                     uint16
	name                      string
	qtype                     uint16
	noQuestion                bool
	answers, authority, extra []rr
}

func (m message) bytes() []byte {
	out := make([]byte, HeaderSize)
	binary.BigEndian.PutUint16(out[0:], m.id)
	binary.BigEndian.PutUint16(out[2:], FlagQR|FlagRD|FlagRA|m.flags)
	if !m.noQuestion {
		binary.BigEndian.PutUint16(out[4:], 1)
		out = append(out, wire(m.name)...)
		qtype := m.qtype
		if qtype == 0 {
			qtype = TypeA
		}
		out = binary.BigEndian.AppendUint16(out, qtype)
		out = binary.BigEndian.AppendUint16(out, ClassIN)
	}
	binary.BigEndian.PutUint16(out[6:], uint16(len(m.answers)))
	binary.BigEndian.PutUint16(out[8:], uint16(len(m.authority)))
	binary.BigEndian.PutUint16(out[10:], uint16(len(m.extra)))
	for _, section := range [][]rr{m.answers, m.authority, m.extra} {
		for _, record := range section {
			class := record.class
			if class == 0 {
				class = ClassIN
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

func opt(payload uint16, ttl uint32) rr {
	return rr{name: ".", kind: TypeOPT, class: payload, ttl: ttl}
}

func u16(value uint16) []byte { return binary.BigEndian.AppendUint16(nil, value) }

// text collects formatter output.
type text struct{ strings.Builder }

func (t *text) Byte(character byte) { t.WriteByte(character) }
func (t *text) Text(value string)   { t.WriteString(value) }
