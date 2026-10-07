package main

import (
	"encoding/binary"
	"strings"
)

const (
	typeA     = 1
	typeCNAME = 5
	typeSOA   = 6
	typeOPT   = 41

	rcodeNoError  = 0
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeRefused  = 5

	flagQR = 0x8000
	flagAA = 0x0400
	flagTC = 0x0200
	flagRD = 0x0100
	flagRA = 0x0080

	ednsPayload = 1232
	soaTTL      = 60
	soaMinimum  = 30
	maxChain    = 8
	maxRecords  = 64
)

type dnsQuery struct {
	id     uint16
	flags  uint16
	labels []string // as received, case kept
	name   string   // lowercase, no trailing dot; "" for the root
	qtype  uint16
	qclass uint16
	hasOPT bool
}

func (q dnsQuery) logName() string {
	if q.name == "" {
		return "."
	}
	return q.name
}

// parseQuery accepts a standard query with exactly one question. The
// question name must use plain labels: no compression pointer can point
// anywhere useful before it. Records after the question are walked with
// bounds checks to find an OPT record. Anything malformed is rejected.
func parseQuery(data []byte) (dnsQuery, bool) {
	if len(data) < 12 {
		return dnsQuery{}, false
	}
	q := dnsQuery{id: binary.BigEndian.Uint16(data[0:2]), flags: binary.BigEndian.Uint16(data[2:4])}
	if q.flags&flagQR != 0 || (q.flags>>11)&0xf != 0 || binary.BigEndian.Uint16(data[4:6]) != 1 {
		return dnsQuery{}, false
	}
	offset := 12
	wire := 1
	for {
		if offset >= len(data) {
			return dnsQuery{}, false
		}
		length := int(data[offset])
		offset++
		if length == 0 {
			break
		}
		if length > 63 || offset+length > len(data) {
			return dnsQuery{}, false
		}
		wire += 1 + length
		if wire > 255 {
			return dnsQuery{}, false
		}
		label := string(data[offset : offset+length])
		if strings.IndexByte(label, '.') >= 0 {
			return dnsQuery{}, false
		}
		q.labels = append(q.labels, label)
		offset += length
	}
	if offset+4 > len(data) {
		return dnsQuery{}, false
	}
	q.qtype = binary.BigEndian.Uint16(data[offset:])
	q.qclass = binary.BigEndian.Uint16(data[offset+2:])
	offset += 4
	q.name = strings.ToLower(strings.Join(q.labels, "."))

	answers := int(binary.BigEndian.Uint16(data[6:8]))
	authority := int(binary.BigEndian.Uint16(data[8:10]))
	additional := int(binary.BigEndian.Uint16(data[10:12]))
	if answers+authority+additional > maxRecords {
		return dnsQuery{}, false
	}
	for index := 0; index < answers+authority+additional; index++ {
		next, ok := skipName(data, offset)
		if !ok || next+10 > len(data) {
			return dnsQuery{}, false
		}
		rtype := binary.BigEndian.Uint16(data[next:])
		rdlength := int(binary.BigEndian.Uint16(data[next+8:]))
		end := next + 10 + rdlength
		if end > len(data) {
			return dnsQuery{}, false
		}
		if rtype == typeOPT {
			if index < answers+authority || next != offset+1 || q.hasOPT {
				return dnsQuery{}, false
			}
			q.hasOPT = true
		}
		offset = end
	}
	return q, true
}

// skipName steps over a name in a record, stopping at a compression pointer.
func skipName(data []byte, offset int) (int, bool) {
	for {
		if offset >= len(data) {
			return 0, false
		}
		length := int(data[offset])
		switch {
		case length == 0:
			return offset + 1, true
		case length&0xc0 == 0xc0:
			if offset+2 > len(data) {
				return 0, false
			}
			return offset + 2, true
		case length > 63:
			return 0, false
		}
		offset += 1 + length
	}
}

type record struct {
	name   string
	rtype  uint16
	ttl    uint32
	addr   ipv4
	target string
}

type resolution struct {
	rcode     uint16
	aa        bool
	tc        bool
	drop      bool
	answers   []record
	authority []record
}

func soaRecord() record { return record{name: zoneSuffix, rtype: typeSOA, ttl: soaTTL} }

// resolve answers name and qtype from the zone. CNAMEs inside the zone are
// followed for every qtype except CNAME, and the chain goes in the answer
// section ahead of what the last name yields.
func (z *zone) resolve(name string, qtype uint16) resolution {
	if !inZone(name) {
		return resolution{rcode: rcodeRefused}
	}
	r := resolution{aa: true}
	current := name
	for hops := 0; ; hops++ {
		if hops > maxChain {
			return resolution{rcode: rcodeServFail, aa: true}
		}
		var entry *zoneEntry
		if z != nil {
			entry = z.entries[current]
		}
		if entry == nil {
			switch {
			case current != zoneSuffix:
				r.rcode = rcodeNXDomain
				r.authority = append(r.authority, soaRecord())
			case qtype == typeSOA:
				r.answers = append(r.answers, soaRecord())
			default:
				r.authority = append(r.authority, soaRecord())
			}
			return r
		}
		switch entry.kind {
		case kindNXDOMAIN:
			r.rcode = rcodeNXDomain
			r.authority = append(r.authority, soaRecord())
		case kindNODATA:
			r.authority = append(r.authority, soaRecord())
		case kindSERVFAIL:
			return resolution{rcode: rcodeServFail, aa: true}
		case kindTRUNCATE:
			return resolution{aa: true, tc: true}
		case kindDROP:
			return resolution{drop: true}
		case kindA:
			switch {
			case qtype == typeA:
				for _, addr := range entry.addrs {
					r.answers = append(r.answers, record{name: current, rtype: typeA, ttl: entry.ttl, addr: addr})
				}
			case qtype == typeSOA && current == zoneSuffix:
				r.answers = append(r.answers, soaRecord())
			default:
				r.authority = append(r.authority, soaRecord())
			}
		case kindCNAME:
			r.answers = append(r.answers, record{name: current, rtype: typeCNAME, ttl: entry.ttl, target: entry.target})
			if qtype == typeCNAME || !inZone(entry.target) {
				return r
			}
			current = entry.target
			continue
		}
		return r
	}
}

// answer builds the response to a query, or nil when the query must go
// unanswered.
func answer(q dnsQuery, z *zone, noEDNS bool) []byte {
	var r resolution
	withOPT := q.hasOPT
	if r = z.resolve(q.name, q.qtype); r.drop {
		return nil
	}
	if q.hasOPT && noEDNS {
		r = resolution{rcode: rcodeFormErr}
		withOPT = false
	}
	flags := uint16(flagQR|flagRA) | q.flags&flagRD | r.rcode
	if r.aa {
		flags |= flagAA
	}
	if r.tc {
		flags |= flagTC
	}
	b := &dnsBuilder{names: map[string]int{}}
	b.u16(q.id)
	b.u16(flags)
	b.u16(1)
	b.u16(uint16(len(r.answers)))
	b.u16(uint16(len(r.authority)))
	additional := uint16(0)
	if withOPT {
		additional = 1
	}
	b.u16(additional)
	b.name(q.labels)
	b.u16(q.qtype)
	b.u16(q.qclass)
	for _, rr := range r.answers {
		b.record(rr)
	}
	for _, rr := range r.authority {
		b.record(rr)
	}
	if withOPT {
		b.buf = append(b.buf, 0)
		b.u16(typeOPT)
		b.u16(ednsPayload)
		b.u32(0)
		b.u16(0)
	}
	return b.buf
}

// dnsBuilder writes a message and compresses names against the suffixes
// already written, matching labels without regard to case.
type dnsBuilder struct {
	buf   []byte
	names map[string]int
}

func (b *dnsBuilder) u16(v uint16) { b.buf = binary.BigEndian.AppendUint16(b.buf, v) }
func (b *dnsBuilder) u32(v uint32) { b.buf = binary.BigEndian.AppendUint32(b.buf, v) }

func (b *dnsBuilder) name(labels []string) {
	for index := range labels {
		suffix := strings.ToLower(strings.Join(labels[index:], "."))
		if offset, ok := b.names[suffix]; ok {
			b.u16(0xc000 | uint16(offset))
			return
		}
		if len(b.buf) < 0x3fff {
			b.names[suffix] = len(b.buf)
		}
		b.buf = append(b.buf, byte(len(labels[index])))
		b.buf = append(b.buf, labels[index]...)
	}
	b.buf = append(b.buf, 0)
}

func splitName(name string) []string {
	if name == "" {
		return nil
	}
	return strings.Split(name, ".")
}

func (b *dnsBuilder) record(rr record) {
	b.name(splitName(rr.name))
	b.u16(rr.rtype)
	b.u16(1)
	b.u32(rr.ttl)
	lengthAt := len(b.buf)
	b.u16(0)
	switch rr.rtype {
	case typeA:
		b.buf = append(b.buf, rr.addr[:]...)
	case typeCNAME:
		b.name(splitName(rr.target))
	case typeSOA:
		b.name([]string{"ns", zoneSuffix})
		b.name([]string{"hostmaster", zoneSuffix})
		b.u32(1)     // serial
		b.u32(3600)  // refresh
		b.u32(600)   // retry
		b.u32(86400) // expire
		b.u32(soaMinimum)
	}
	binary.BigEndian.PutUint16(b.buf[lengthAt:], uint16(len(b.buf)-lengthAt-2))
}
