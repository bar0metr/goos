package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// zoneSuffix is the only zone the peer serves. Names outside it are refused.
const zoneSuffix = "test"

type entryKind int

const (
	kindA entryKind = iota
	kindCNAME
	kindNXDOMAIN
	kindNODATA
	kindSERVFAIL
	kindTRUNCATE
	kindDROP
	kindTCP
	kindTCPRESET
)

var directives = map[string]entryKind{
	"NXDOMAIN": kindNXDOMAIN,
	"NODATA":   kindNODATA,
	"SERVFAIL": kindSERVFAIL,
	"TRUNCATE": kindTRUNCATE,
	"DROP":     kindDROP,
	"TCPRESET": kindTCPRESET,
}

type zoneEntry struct {
	kind  entryKind
	ttl   uint32
	addrs []ipv4
	// records holds the name's AAAA, NS, MX, TXT and PTR records; only a
	// kindA entry has them, beside or instead of addresses.
	records []record
	target  string
	// slow delays every answer for the name by slowAnswer.
	slow bool
}

type zone struct {
	entries map[string]*zoneEntry
}

// normalizeName lowercases a name and drops one trailing dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// lookup returns the entry for a normalized name, or nil.
func (z *zone) lookup(name string) *zoneEntry {
	if z == nil {
		return nil
	}
	return z.entries[name]
}

func inZone(name string) bool {
	return name == zoneSuffix || strings.HasSuffix(name, "."+zoneSuffix)
}

// parseZone reads one record per line:
//
//	name TTL A 1.2.3.4
//	name TTL SLOW 1.2.3.4
//	name TTL AAAA 2001:db8::1
//	name TTL NS|PTR target
//	name TTL MX preference target
//	name TTL TXT text with spaces and \DDD escapes
//	name TTL CNAME target
//	name TTL TCP 1.2.3.4 [5.6.7.8 ...]
//	name NXDOMAIN|NODATA|SERVFAIL|TRUNCATE|DROP|TCPRESET
//
// TCP and TCPRESET names answer UDP with TC=1. Over TCP a TCP name gets its
// addresses padded past 4096 bytes, and a TCPRESET name gets part of an
// answer and a reset.
//
// A name may have several A, AAAA, NS, MX, TXT and PTR lines but no other
// line beside them. Text after '#' is a comment.
func parseZone(r io.Reader) (*zone, error) {
	z := &zone{entries: map[string]*zoneEntry{}}
	scanner := bufio.NewScanner(r)
	number := 0
	for scanner.Scan() {
		number++
		line := scanner.Text()
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = line[:index]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if err := z.add(fields); err != nil {
			return nil, fmt.Errorf("zone line %d: %v", number, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return z, nil
}

func (z *zone) add(fields []string) error {
	name := normalizeName(fields[0])
	if !inZone(name) {
		return fmt.Errorf("name %q is outside zone %s", fields[0], zoneSuffix)
	}
	existing := z.entries[name]
	if len(fields) == 2 {
		kind, ok := directives[strings.ToUpper(fields[1])]
		if !ok {
			return fmt.Errorf("unknown directive %q", fields[1])
		}
		if existing != nil {
			return fmt.Errorf("name %s already defined", name)
		}
		z.entries[name] = &zoneEntry{kind: kind}
		return nil
	}
	if len(fields) < 4 || len(fields) > 4 && !strings.EqualFold(fields[2], "TCP") && !strings.EqualFold(fields[2], "TXT") && !strings.EqualFold(fields[2], "MX") {
		return fmt.Errorf("want 'name TTL TYPE value' or 'name DIRECTIVE'")
	}
	ttl, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return fmt.Errorf("invalid TTL %q", fields[1])
	}
	switch kind := strings.ToUpper(fields[2]); kind {
	case "TCP":
		if existing != nil {
			return fmt.Errorf("name %s already defined", name)
		}
		entry := &zoneEntry{kind: kindTCP, ttl: uint32(ttl)}
		for _, text := range fields[3:] {
			addr, err := parseIPv4Addr(text)
			if err != nil {
				return err
			}
			entry.addrs = append(entry.addrs, addr)
		}
		z.entries[name] = entry
		return nil
	case "A", "SLOW":
		addr, err := parseIPv4Addr(fields[3])
		if err != nil {
			return err
		}
		if existing == nil {
			z.entries[name] = &zoneEntry{kind: kindA, ttl: uint32(ttl), addrs: []ipv4{addr}, slow: kind == "SLOW"}
			return nil
		}
		if existing.kind != kindA || existing.slow != (kind == "SLOW") {
			return fmt.Errorf("name %s already defined", name)
		}
		existing.addrs = append(existing.addrs, addr)
		return nil
	case "AAAA", "NS", "PTR", "MX", "TXT":
		rr, err := dataRecord(name, kind, uint32(ttl), fields[3:])
		if err != nil {
			return err
		}
		if existing == nil {
			z.entries[name] = &zoneEntry{kind: kindA, ttl: uint32(ttl), records: []record{rr}}
			return nil
		}
		if existing.kind != kindA || existing.slow {
			return fmt.Errorf("name %s already defined", name)
		}
		existing.records = append(existing.records, rr)
		return nil
	case "CNAME":
		if existing != nil {
			return fmt.Errorf("name %s already defined", name)
		}
		target := normalizeName(fields[3])
		if target == "" {
			return fmt.Errorf("empty CNAME target")
		}
		z.entries[name] = &zoneEntry{kind: kindCNAME, ttl: uint32(ttl), target: target}
		return nil
	}
	return fmt.Errorf("unknown record type %q", fields[2])
}

// dataRecord reads the value of an AAAA, NS, PTR, MX or TXT line.
func dataRecord(name, kind string, ttl uint32, values []string) (record, error) {
	rr := record{name: name, ttl: ttl}
	switch kind {
	case "AAAA":
		addr := net.ParseIP(values[0])
		if addr == nil || addr.To4() != nil || strings.Contains(values[0], ".") {
			return record{}, fmt.Errorf("invalid IPv6 address %q", values[0])
		}
		rr.rtype = typeAAAA
		copy(rr.addr6[:], addr.To16())
	case "NS", "PTR":
		rr.rtype, rr.target = typeNS, normalizeName(values[0])
		if kind == "PTR" {
			rr.rtype = typePTR
		}
	case "MX":
		if len(values) != 2 {
			return record{}, fmt.Errorf("want 'name TTL MX preference target'")
		}
		preference, err := strconv.ParseUint(values[0], 10, 16)
		if err != nil {
			return record{}, fmt.Errorf("invalid MX preference %q", values[0])
		}
		rr.rtype, rr.preference, rr.target = typeMX, uint16(preference), normalizeName(values[1])
	case "TXT":
		text, err := unescape(strings.Join(values, " "))
		if err != nil {
			return record{}, err
		}
		rr.rtype, rr.text = typeTXT, text
	}
	if (rr.rtype == typeNS || rr.rtype == typePTR || rr.rtype == typeMX) && rr.target == "" {
		return record{}, fmt.Errorf("empty %s target", kind)
	}
	return rr, nil
}

// unescape turns \DDD into the byte it names, so a zone can hold control
// bytes in a TXT string. The result is one character-string of at most 255
// bytes.
func unescape(text string) (string, error) {
	var out []byte
	for index := 0; index < len(text); index++ {
		if text[index] != '\\' {
			out = append(out, text[index])
			continue
		}
		if index+4 > len(text) {
			return "", fmt.Errorf("short escape in %q", text)
		}
		value, err := strconv.ParseUint(text[index+1:index+4], 10, 8)
		if err != nil {
			return "", fmt.Errorf("invalid escape in %q", text)
		}
		out = append(out, byte(value))
		index += 3
	}
	if len(out) > 255 {
		return "", fmt.Errorf("TXT string longer than 255 bytes")
	}
	return string(out), nil
}
