// Package resolvapi defines GRESOLV1, the protocol between commands and the
// resolver service (ADR 0194). A request names one operation and, for a
// lookup, one host name; a reply has one fixed layout whose fields each
// operation fills or leaves zero.
package resolvapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
	"goos/userspace/hostname"
)

const (
	ProtocolID = uint64(0x475245534f4c5631)
	Version    = uint16(1)
	// A request is the opcode, the name's length and the name. A 253-byte
	// name fills it to 255 bytes, inside the 256-byte payload.
	requestHeader = 2
	ReplySize     = 180

	MaxAddresses = 8
	MaxServers   = 8
	// Keys counts the settings Reload reads, numbered from 1 in Key's order.
	Keys = 10

	replyOpcode     = 0
	replyResult     = 1
	replyCount      = 2
	replySource     = 3
	replyTTL        = 4
	replyAddresses  = 8
	replyServers    = 40
	replyGeneration = 80
	replyCache      = 88
	replyField      = 98
	replyReserved   = 99
	replyCounters   = 100
	counterCount    = 10
)

type Opcode uint8

const (
	OpResolve Opcode = iota + 1
	OpStatus
	OpFlush
	OpReload
	lastOpcode = OpReload
)

// Result values 0..11 are the resolver's own (userspace/dns.Result).
type Result uint8

const (
	ResultOK Result = iota
	ResultNotFound
	ResultNoData
	ResultNoServers
	ResultTimeout
	ResultTemporaryFailure
	ResultTruncated
	ResultInvalidName
	ResultUnsupportedNameScope
	ResultPermissionDenied
	ResultBusy
	ResultReconfigured
	// ResultConfigInvalid answers a Reload whose settings fail; Field names
	// the first bad key and the previous settings stay.
	ResultConfigInvalid
	lastResult = ResultConfigInvalid
)

// Source says where an answer came from.
type Source uint8

const (
	SourceUpstream Source = iota
	SourceCache
	SourceHosts
	SourceLocalhost
	lastSource = SourceLocalhost
)

// ServerSource says where an upstream server came from.
type ServerSource uint8

const (
	ServerDHCP ServerSource = iota
	ServerConfig
)

type Server struct {
	Address [4]byte
	Source  ServerSource
}

type Request struct {
	Opcode Opcode
	Name   hostname.Name
}

// Reply is one decoded reply. Resolve sets Count addresses, Source and TTL;
// Status sets Count servers, Generation, the cache figures and the
// counters; Reload with ConfigInvalid sets Field; anything else is zero.
type Reply struct {
	Opcode     Opcode
	Result     Result
	Count      uint8
	Source     Source
	TTL        uint32
	Addresses  [MaxAddresses][4]byte
	Servers    [MaxServers]Server
	Generation uint64
	CacheUsed  uint16
	CacheLimit uint16
	Positive   uint16
	Negative   uint16
	Failures   uint16
	Field      uint8

	PositiveHits   uint64
	NegativeHits   uint64
	FailureHits    uint64
	Misses         uint64
	Queries        uint64
	Timeouts       uint64
	Malformed      uint64
	Mismatched     uint64
	Truncated      uint64
	ServerFailures uint64
}

func (r *Reply) counters() [counterCount]*uint64 {
	return [counterCount]*uint64{&r.PositiveHits, &r.NegativeHits, &r.FailureHits, &r.Misses, &r.Queries,
		&r.Timeouts, &r.Malformed, &r.Mismatched, &r.Truncated, &r.ServerFailures}
}

// prepare clears out field by field: a literal of the whole message would be
// a temporary TinyGo keeps on the heap.
func prepare(out *ipc.Message, kind ipc.Kind, correlation uint64, size int) {
	for index := range out.Payload[:] {
		out.Payload[index] = 0
	}
	for index := range out.Handles[:] {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = ProtocolID, Version, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, uint16(size), 0
}

// EncodeRequest carries the name only for Resolve; the name must already be
// normalized.
func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || r.Opcode < OpResolve || r.Opcode > lastOpcode || (r.Opcode == OpResolve) != (r.Name.Size != 0) {
		return false
	}
	prepare(out, ipc.KindRequest, correlation, requestHeader+int(r.Name.Size))
	out.Payload[0] = byte(r.Opcode)
	out.Payload[1] = r.Name.Size
	copy(out.Payload[requestHeader:], r.Name.Bytes())
	return true
}

// DecodeRequest accepts a name in any case and with a trailing dot and
// normalizes it. A Resolve whose name fails the syntax decodes with an empty
// name, so the service can answer InvalidName rather than drop it.
func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindRequest || in.Correlation == 0 ||
		in.HandleCount != 0 || in.PayloadSize < requestHeader {
		return 0, false
	}
	opcode, size := Opcode(in.Payload[0]), int(in.Payload[1])
	if opcode < OpResolve || opcode > lastOpcode || requestHeader+size != int(in.PayloadSize) || (opcode == OpResolve) != (size != 0) {
		return 0, false
	}
	r := Request{Opcode: opcode}
	if size != 0 {
		r.Name, _ = hostname.Parse(in.Payload[requestHeader : requestHeader+size])
	}
	*out = r
	return in.Correlation, true
}

func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ipc.KindReply, correlation, ReplySize)
	payload := out.Payload[:ReplySize]
	payload[replyOpcode], payload[replyResult], payload[replyCount], payload[replySource] =
		byte(r.Opcode), byte(r.Result), r.Count, byte(r.Source)
	binary.LittleEndian.PutUint32(payload[replyTTL:], r.TTL)
	for index, address := range r.Addresses {
		copy(payload[replyAddresses+4*index:], address[:])
	}
	for index, server := range r.Servers {
		copy(payload[replyServers+5*index:], server.Address[:])
		payload[replyServers+5*index+4] = byte(server.Source)
	}
	binary.LittleEndian.PutUint64(payload[replyGeneration:], r.Generation)
	for index, value := range [...]uint16{r.CacheUsed, r.CacheLimit, r.Positive, r.Negative, r.Failures} {
		binary.LittleEndian.PutUint16(payload[replyCache+2*index:], value)
	}
	payload[replyField] = r.Field
	for index, counter := range r.counters() {
		binary.LittleEndian.PutUint64(payload[replyCounters+8*index:], *counter)
	}
	return true
}

// DecodeReply accepts only the reply to the caller's request.
func DecodeReply(in *ipc.Message, correlation uint64, opcode Opcode, out *Reply) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || correlation == 0 ||
		in.Correlation != correlation || in.PayloadSize != ReplySize || in.HandleCount != 0 {
		return false
	}
	payload := in.Payload[:ReplySize]
	if Opcode(payload[replyOpcode]) != opcode || payload[replyReserved] != 0 {
		return false
	}
	r := Reply{Opcode: opcode, Result: Result(payload[replyResult]), Count: payload[replyCount],
		Source: Source(payload[replySource]), TTL: binary.LittleEndian.Uint32(payload[replyTTL:]),
		Generation: binary.LittleEndian.Uint64(payload[replyGeneration:]), Field: payload[replyField]}
	for index := range r.Addresses {
		copy(r.Addresses[index][:], payload[replyAddresses+4*index:])
	}
	for index := range r.Servers {
		copy(r.Servers[index].Address[:], payload[replyServers+5*index:])
		r.Servers[index].Source = ServerSource(payload[replyServers+5*index+4])
	}
	cache := [...]*uint16{&r.CacheUsed, &r.CacheLimit, &r.Positive, &r.Negative, &r.Failures}
	for index, value := range cache {
		*value = binary.LittleEndian.Uint16(payload[replyCache+2*index:])
	}
	for index, counter := range r.counters() {
		*counter = binary.LittleEndian.Uint64(payload[replyCounters+8*index:])
	}
	if !validReply(&r) {
		return false
	}
	*out = r
	return true
}

func validReply(r *Reply) bool {
	if r.Opcode < OpResolve || r.Opcode > lastOpcode || r.Result > lastResult || r.Source > lastSource {
		return false
	}
	noStatus := r.Servers == [MaxServers]Server{} && r.Generation == 0 && r.CacheUsed == 0 && r.CacheLimit == 0 &&
		r.Positive == 0 && r.Negative == 0 && r.Failures == 0
	for _, counter := range r.counters() {
		noStatus = noStatus && *counter == 0
	}
	noAnswer := r.Addresses == [MaxAddresses][4]byte{} && r.Source == 0 && r.TTL == 0
	switch r.Opcode {
	case OpResolve:
		if !noStatus || r.Field != 0 || r.Count > MaxAddresses || (r.Result == ResultOK) != (r.Count != 0) {
			return false
		}
		for index := int(r.Count); index < MaxAddresses; index++ {
			if r.Addresses[index] != ([4]byte{}) {
				return false
			}
		}
		return true
	case OpStatus:
		if !noAnswer || r.Field != 0 || r.Count > MaxServers {
			return false
		}
		for index := int(r.Count); index < MaxServers; index++ {
			if r.Servers[index] != (Server{}) {
				return false
			}
		}
		return r.Result == ResultOK || noStatus && r.Count == 0
	case OpFlush:
		return noStatus && noAnswer && r.Count == 0 && r.Field == 0
	case OpReload:
		return noStatus && noAnswer && r.Count == 0 && (r.Field == 0 || r.Result == ResultConfigInvalid && r.Field <= Keys)
	}
	return false
}

// Key names the setting a Reload failure's Field refers to.
func Key(field uint8) string {
	switch field {
	case 1:
		return "system.dns.server1"
	case 2:
		return "system.dns.server2"
	case 3:
		return "system.dns.server3"
	case 4:
		return "system.dns.server4"
	case 5:
		return "system.dns.cache.entries"
	case 6:
		return "system.dns.cache.max-ttl-sec"
	case 7:
		return "system.dns.cache.max-negative-sec"
	case 8:
		return "system.dns.failure-cache-sec"
	case 9:
		return "system.dns.query-timeout-ms"
	case 10:
		return "system.dns.attempts"
	}
	return ""
}

// Text returns short lowercase text for a result.
func (r Result) Text() string {
	switch r {
	case ResultOK:
		return "ok"
	case ResultNotFound:
		return "name not found"
	case ResultNoData:
		return "no IPv4 address"
	case ResultNoServers:
		return "no DNS servers"
	case ResultTimeout:
		return "timeout"
	case ResultTemporaryFailure:
		return "temporary failure"
	case ResultTruncated:
		return "answer truncated; DNS over TCP is not supported"
	case ResultInvalidName:
		return "invalid name"
	case ResultUnsupportedNameScope:
		return ".local names need mDNS, which is not supported"
	case ResultPermissionDenied:
		return "permission denied"
	case ResultBusy:
		return "busy"
	case ResultReconfigured:
		return "network changed"
	case ResultConfigInvalid:
		return "invalid configuration"
	}
	return ""
}

func (s Source) Text() string {
	switch s {
	case SourceUpstream:
		return "dns"
	case SourceCache:
		return "cache"
	case SourceHosts:
		return "hosts"
	case SourceLocalhost:
		return "localhost"
	}
	return ""
}

func (s ServerSource) Text() string {
	if s == ServerDHCP {
		return "dhcp"
	}
	return "config"
}
