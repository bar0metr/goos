package main

import (
	"bufio"
	"fmt"
	"io"
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
)

var directives = map[string]entryKind{
	"NXDOMAIN": kindNXDOMAIN,
	"NODATA":   kindNODATA,
	"SERVFAIL": kindSERVFAIL,
	"TRUNCATE": kindTRUNCATE,
	"DROP":     kindDROP,
}

type zoneEntry struct {
	kind   entryKind
	ttl    uint32
	addrs  []ipv4
	target string
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
//	name TTL CNAME target
//	name NXDOMAIN|NODATA|SERVFAIL|TRUNCATE|DROP
//
// A name may have several A lines but no other line beside them. Text after
// '#' is a comment.
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
	if len(fields) != 4 {
		return fmt.Errorf("want 'name TTL A|CNAME value' or 'name DIRECTIVE'")
	}
	ttl, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return fmt.Errorf("invalid TTL %q", fields[1])
	}
	switch kind := strings.ToUpper(fields[2]); kind {
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
