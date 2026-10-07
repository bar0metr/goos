package config

import (
	"goos/userspace/hostname"
	"goos/userspace/network"
)

// The hosts table maps host names to IPv4 addresses on this machine, before
// any DNS server is asked. It is data the configuration service keeps beside
// the typed settings, not a setting: it has its own file, its own protocol on
// the same session, and its own rights.
const (
	// MaxHosts bounds the table. localhost is not one of them.
	MaxHosts = 64
	// MaxHostName bounds a name in the table, below the 253 a DNS name may
	// have. A hosts request and a hosts reply have an 8-byte header, and
	// header and name together must fit one IPC payload of 256 bytes, so a
	// record always travels in one message.
	MaxHostName = 248
	// HostsFileSize bounds /state/hosts. Every record the service writes
	// fits MaxHostName+20 bytes; the rest is room for comments and blank
	// lines in a file edited by hand. Reading stops at the bound.
	HostsFileSize = MaxHosts*(MaxHostName+20) + 4096
)

// LocalhostAddress is what localhost always means. It is system policy, not
// a record: no file and no request can change it.
var LocalhostAddress = [4]byte{127, 0, 0, 1}

var localhostText = []byte("localhost")

// HostRecord is one name and its address.
type HostRecord struct {
	Name    hostname.Name
	Address [4]byte
}

// Hosts is the table. Records keep the order they were added in, so the file
// and a listing come out the same way every time. Generation changes on every
// change that succeeds, so a client holding an answer can tell whether it is
// still current.
type Hosts struct {
	records    [MaxHosts]HostRecord
	count      int
	Generation uint64
}

// Count reports the records in the table, without localhost.
func (hosts *Hosts) Count() int { return hosts.count }

// ValidHostAddress accepts an address one host can have: not in 0/8, not
// limited broadcast, not multicast and not class E. A loopback address is
// allowed for a name other than localhost.
func ValidHostAddress(address [4]byte) bool {
	return address[0] != 0 && address[0] < 224
}

// reservedHost reports localhost and the names under it. The resolver answers
// them itself (RFC 6761), so the table never holds them.
func reservedHost(name *hostname.Name) bool { return name.IsLocalhost() }

// Lookup returns the address for a name. localhost always answers, whatever
// the table holds; a name under .localhost is not found here.
func (hosts *Hosts) Lookup(name *hostname.Name) ([4]byte, Status) {
	if name.IsText("localhost") {
		return LocalhostAddress, StatusOK
	}
	if index := hosts.find(name); index >= 0 {
		return hosts.records[index].Address, StatusOK
	}
	return [4]byte{}, StatusNotFound
}

// Set adds a record or replaces the address of an existing one. Setting the
// address a name already has is not a change and leaves Generation alone.
func (hosts *Hosts) Set(name *hostname.Name, address [4]byte) Status {
	if name.Size == 0 || name.Size > MaxHostName || !ValidHostAddress(address) {
		return StatusInvalidArgument
	}
	if reservedHost(name) {
		return StatusReadOnly
	}
	if index := hosts.find(name); index >= 0 {
		if hosts.records[index].Address != address {
			hosts.records[index].Address = address
			hosts.Generation++
		}
		return StatusOK
	}
	if hosts.count == MaxHosts {
		return StatusResourceExhausted
	}
	record := &hosts.records[hosts.count]
	record.Name, record.Address = *name, address
	hosts.count++
	hosts.Generation++
	return StatusOK
}

// Remove deletes a record. The records after it move up, so the order of
// the rest is kept.
func (hosts *Hosts) Remove(name *hostname.Name) Status {
	if name.Size == 0 || name.Size > MaxHostName {
		return StatusInvalidArgument
	}
	if reservedHost(name) {
		return StatusReadOnly
	}
	index := hosts.find(name)
	if index < 0 {
		return StatusNotFound
	}
	for ; index+1 < hosts.count; index++ {
		hosts.records[index] = hosts.records[index+1]
	}
	hosts.count--
	hosts.records[hosts.count] = HostRecord{}
	hosts.Generation++
	return StatusOK
}

// At returns the record at cursor and the cursor after it. Cursor 0 is
// localhost; cursor n is the nth stored record. Past the end it answers
// StatusNotFound. A change between two calls can move a record past the
// cursor, which the caller sees as a different Generation.
func (hosts *Hosts) At(cursor int) ([]byte, [4]byte, int, Status) {
	if cursor < 0 {
		return nil, [4]byte{}, cursor, StatusInvalidArgument
	}
	if cursor == 0 {
		return localhostText, LocalhostAddress, 1, StatusOK
	}
	if cursor > hosts.count {
		return nil, [4]byte{}, cursor, StatusNotFound
	}
	record := &hosts.records[cursor-1]
	return record.Name.Bytes(), record.Address, cursor + 1, StatusOK
}

func (hosts *Hosts) find(name *hostname.Name) int {
	for index := 0; index < hosts.count; index++ {
		if hosts.records[index].Name.Equal(name) {
			return index
		}
	}
	return -1
}

// The hosts file is one record per line:
//
//	192.0.2.10 build.example
//	10.0.0.2 nas
//
// The address is strict dotted decimal and comes first, as in the hosts file
// of other systems. Fields are separated by one or more spaces or tabs, and a
// line may begin or end with them. A name may end in a dot and may use upper
// case; it is stored without either. Everything from a # to the end of the
// line is a comment, and a line that is empty after that is ignored. A line
// with a third field (an alias) is malformed: a record has one name.

// AppendHosts appends the file form of every stored record, in table order.
// localhost is not written: it is policy, not a record.
func AppendHosts(target []byte, hosts *Hosts) []byte {
	for index := 0; index < hosts.count; index++ {
		record := &hosts.records[index]
		target = network.AppendIPv4(target, network.IPv4(record.Address))
		target = append(target, ' ')
		target = append(target, record.Name.Bytes()...)
		target = append(target, '\n')
	}
	return target
}

// ParseHosts replaces the table's records with those data holds and returns
// the lines it skipped: malformed, a reserved name, a name already seen, or
// past a full table. A damaged line costs its own record and nothing else.
// Data longer than HostsFileSize is cut at the bound, and a line the cut
// splits is skipped rather than read as a shorter name.
func ParseHosts(data []byte, hosts *Hosts) int {
	for index := 0; index < hosts.count; index++ {
		hosts.records[index] = HostRecord{}
	}
	hosts.count = 0
	hosts.Generation++
	skipped := 0
	if len(data) > HostsFileSize {
		data = data[:HostsFileSize]
		end := len(data)
		for end > 0 && data[end-1] != '\n' {
			end--
		}
		if end != len(data) {
			skipped++
		}
		data = data[:end]
	}
	start := 0
	for index := 0; index <= len(data); index++ {
		if index != len(data) && data[index] != '\n' {
			continue
		}
		if !hosts.parseLine(data[start:index]) {
			skipped++
		}
		start = index + 1
	}
	return skipped
}

// parseLine adds one line's record. A line with nothing but a comment or
// blanks is accepted and adds nothing.
func (hosts *Hosts) parseLine(line []byte) bool {
	for index, character := range line {
		if character == '#' {
			line = line[:index]
			break
		}
	}
	address, rest := hostsField(line)
	if len(address) == 0 {
		return true
	}
	text, rest := hostsField(rest)
	if extra, _ := hostsField(rest); len(text) == 0 || len(extra) != 0 {
		return false
	}
	parsed, ok := network.ParseIPv4(address)
	if !ok || !ValidHostAddress(parsed) {
		return false
	}
	name, ok := hostname.Parse(text)
	if !ok || name.Size > MaxHostName || reservedHost(&name) || hosts.find(&name) >= 0 || hosts.count == MaxHosts {
		return false
	}
	record := &hosts.records[hosts.count]
	record.Name, record.Address = name, parsed
	hosts.count++
	return true
}

// hostsField returns the next field of blanks-separated text and what
// follows it.
func hostsField(text []byte) ([]byte, []byte) {
	start := 0
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		start++
	}
	end := start
	for end < len(text) && text[end] != ' ' && text[end] != '\t' {
		end++
	}
	return text[start:end], text[end:]
}
