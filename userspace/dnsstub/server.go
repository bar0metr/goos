// Package dnsstub is the resolver's local DNS server: 127.0.0.1:53 over UDP
// and TCP, for programs that speak DNS themselves, such as a static C
// library that reads /etc/resolv.conf. It answers from the resolver core
// (userspace/dns) with the policy every native lookup gets: localhost, the
// hosts table, the cache, then the upstream servers over UDP with the TCP
// fallback and the resolver's own deadlines. A client sends a question and
// gets back an answer; it never reaches an upstream server, a resolver
// session or anything else the resolver holds.
//
// The server runs on the resolver's own GUDP1 and GTCP1 sessions, beside
// userspace/upstream and userspace/tcpupstream. Every correlation it issues
// has the top bit set (Owns), so the service hands each reply to the right
// client. It shares their TX slot trackers and takes a slot only when that
// leaves the other client what it can need: on UDP one slot for every query
// still opening, on TCP one for each of the tcpupstream.MaxStreams
// fallbacks. The service owns the channels and pools and sends what this
// package asks it to, so the whole path runs in host tests against a real
// stack.
//
// Queries. Only A and AAAA in class IN are looked up. A gets the addresses;
// AAAA looks up the same name over IPv4 and answers with the rcode of that
// lookup and no records, because GoOS has no IPv6: a C library asking for
// both gets its IPv4 answer at once instead of waiting out a timeout. Other
// types and opcodes are NOTIMP, other classes and names outside host-name
// syntax REFUSED, and an EDNS version other than 0 BADVERS. A message too
// short for a header, or with QR set, gets no reply; any other malformed
// query gets FORMERR. Lookup results map to rcodes in rcodeOf. Every reply
// fits 512 bytes (dnswire.MaxReply), so none is ever truncated.
//
// Bounds. MaxUDPRequests datagram queries and MaxConnections TCP
// connections, each with one query at a time, are in progress at once; each
// owns one resolver client slot from the base given to Init. The server
// keeps one RecvFrom out, and only while a request slot is free, so a flood
// waits in the network service's bounded queue and is dropped there; it
// keeps one Accept out while a connection slot is free, behind a backlog of
// Backlog. A connection must deliver each query, and take each reply, within
// IdleTimeout; while its query is resolved, the resolver's deadline bounds
// it instead. Everything is fixed storage; nothing allocates.
package dnsstub

import (
	"encoding/binary"

	"goos/userspace/dns"
	"goos/userspace/dnstcp"
	"goos/userspace/dnswire"
	"goos/userspace/hostname"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpupstream"
	"goos/userspace/udpapi"
	"goos/userspace/upstream"
)

const (
	// Port is where the server listens, on Address.
	Port = 53
	// MaxUDPRequests bounds datagram queries in progress.
	MaxUDPRequests = 6
	// MaxConnections bounds accepted TCP connections.
	MaxConnections = 2
	// MaxRequests is the resolver client slots the server uses.
	MaxRequests = MaxUDPRequests + MaxConnections
	// Backlog is the listener's backlog of connections not yet accepted.
	Backlog = 2
	// MaxStreamQuery is the longest query taken over TCP. A longer length
	// prefix closes the connection.
	MaxStreamQuery = dns.DatagramSize
	// IdleTimeout bounds how long a connection may take to deliver a query
	// or take a reply, in nanoseconds.
	IdleTimeout = 5 * second
	// RetryDelay is how long a failed endpoint waits before it is opened
	// again.
	RetryDelay = second

	second = uint64(1_000_000_000)
	// maxTCPSlots is the TX slots the server may hold on the TCP session.
	maxTCPSlots = tcpapi.TXSlots - tcpupstream.MaxStreams
	ownBit      = uint64(1) << 63
	framedReply = dnstcp.PrefixSize + dnswire.MaxReply
)

// Address is the loopback address the server binds.
var Address = [4]byte{127, 0, 0, 1}

// Hosts answers names from the hosts table. A nil Hosts skips the table.
type Hosts interface {
	Address(name *hostname.Name) ([4]byte, bool)
}

// UDPSender sends one GUDP1 request and reports whether it went.
type UDPSender interface {
	Send(request *udpapi.Request, correlation uint64) bool
}

// TCPSender sends one GTCP1 request and reports whether it went.
type TCPSender interface {
	Send(request *tcpapi.Request, correlation uint64) bool
}

// Owns reports a correlation the server issued.
func Owns(correlation uint64) bool { return correlation&ownBit != 0 }

// Stats counts what the server did; the counters saturate.
type Stats struct {
	// UDPQueries and TCPQueries count the queries taken, Ignored those that
	// got no reply and Malformed those that got FORMERR.
	UDPQueries, TCPQueries, Ignored, Malformed uint64
	// Replies counts replies sent, Failures those with SERVFAIL.
	Replies, Failures uint64
	// Connections counts connections accepted, Timeouts those closed by
	// IdleTimeout.
	Connections, Timeouts uint64
}

type requestPhase uint8

const (
	requestFree requestPhase = iota
	requestResolving
	// requestReady holds a reply not sent yet.
	requestReady
	// requestSending has a SendTo out.
	requestSending
)

// request is one query in progress. The reply is written behind room for the
// TCP length prefix, so either transport sends it from the same bytes.
type request struct {
	phase   requestPhase
	query   dnswire.Query
	address [4]byte
	port    uint16
	send    uint64
	length  int
	reply   [framedReply]byte
}

type endpointPhase uint8

const (
	endpointClosed endpointPhase = iota
	endpointOpening
	endpointBinding
	endpointListening
	endpointReady
)

type udpSide struct {
	online   bool
	tx, rx   []byte
	slots    *udpapi.Slots
	upstream *upstream.Client
	phase    endpointPhase
	endpoint uint32
	pending  uint64
	recv     uint64
	held     int
	retry    uint64
}

type connectionPhase uint8

const (
	connectionFree connectionPhase = iota
	connectionReading
	connectionResolving
	connectionWriting
)

// connection is one accepted client. pending is the correlation of its one
// Read or Write out, so a reply for a closed or reused connection matches
// nothing.
type connection struct {
	phase    connectionPhase
	endpoint uint32
	pending  uint64
	deadline uint64
	receiver dnstcp.Receiver
	// written counts the framed reply bytes the connection took, offered
	// those in the Write out.
	written, offered int
	buffer           [MaxStreamQuery]byte
}

type tcpSide struct {
	online      bool
	tx, rx      []byte
	slots       *tcpapi.Slots
	phase       endpointPhase
	listener    uint32
	pending     uint64
	accept      uint64
	held        int
	retry       uint64
	connections [MaxConnections]connection
}

// Server is the whole stub state, fixed storage a service keeps statically.
type Server struct {
	base        int
	hosts       Hosts
	correlation uint64
	udp         udpSide
	tcp         tcpSide
	requests    [MaxRequests]request
	stats       Stats
	host        hostname.Name
	udpRequest  udpapi.Request
	tcpRequest  tcpapi.Request
}

// Init resets the server. Its requests are resolver clients base through
// base+MaxRequests-1, which nothing else may use.
func (s *Server) Init(base int, hosts Hosts) {
	s.base, s.hosts = base, hosts
	s.correlation = 0
	s.udp = udpSide{}
	s.tcp.online, s.tcp.tx, s.tcp.rx, s.tcp.slots = false, nil, nil, nil
	s.tcp.phase, s.tcp.listener, s.tcp.pending, s.tcp.accept, s.tcp.held, s.tcp.retry = endpointClosed, 0, 0, 0, 0, 0
	for index := range s.tcp.connections {
		s.tcp.connections[index].phase = connectionFree
		s.tcp.connections[index].pending = 0
	}
	for index := range s.requests {
		s.requests[index].phase = requestFree
	}
	s.stats = Stats{}
}

// Stats returns the counters.
func (s *Server) Stats() Stats { return s.stats }

// Live counts the requests in progress and the connections accepted.
func (s *Server) Live() (requests, connections int) {
	for index := range s.requests {
		if s.requests[index].phase != requestFree {
			requests++
		}
	}
	for index := range s.tcp.connections {
		if s.tcp.connections[index].phase != connectionFree {
			connections++
		}
	}
	return requests, connections
}

func (s *Server) next() uint64 {
	s.correlation = (s.correlation + 1) &^ ownBit
	if s.correlation == 0 {
		s.correlation = 1
	}
	return s.correlation | ownBit
}

func increment(counter *uint64) {
	if *counter != ^uint64(0) {
		*counter++
	}
}

// Deadline is the earliest time Flush has work that no message brings: an
// endpoint to open again or a connection to time out.
func (s *Server) Deadline() (uint64, bool) {
	earliest, found := uint64(0), false
	if s.udp.online && s.udp.phase == endpointClosed {
		earliest, found = earlier(earliest, found, s.udp.retry)
	}
	if s.tcp.online && s.tcp.phase == endpointClosed {
		earliest, found = earlier(earliest, found, s.tcp.retry)
	}
	for index := range s.tcp.connections {
		current := &s.tcp.connections[index]
		if current.phase == connectionReading || current.phase == connectionWriting {
			earliest, found = earlier(earliest, found, current.deadline)
		}
	}
	return earliest, found
}

func earlier(earliest uint64, found bool, at uint64) (uint64, bool) {
	if !found || at < earliest {
		return at, true
	}
	return earliest, true
}

// --- queries ---------------------------------------------------------------

// rcodeOf maps a lookup result to the reply code. A failure that may pass,
// including a busy resolver and a network change, is SERVFAIL, so the client
// may ask again; a name the resolver will not look up is REFUSED.
func rcodeOf(result dns.Result) uint16 {
	switch result {
	case dns.ResultOK, dns.ResultNoData:
		return dnswire.RcodeNoError
	case dns.ResultNotFound:
		return dnswire.RcodeNXDomain
	case dns.ResultInvalidName, dns.ResultUnsupportedNameScope, dns.ResultPermissionDenied:
		return dnswire.RcodeRefused
	}
	return dnswire.RcodeServFail
}

// take parses one query into request index and answers it, or starts its
// lookup. It reports false for a message that gets no reply.
func (s *Server) take(resolver *dns.Resolver, index int, message []byte, now uint64) bool {
	current := &s.requests[index]
	switch dnswire.ParseQuery(message, &current.query) {
	case dnswire.QueryIgnore:
		increment(&s.stats.Ignored)
		return false
	case dnswire.QueryFormErr:
		increment(&s.stats.Malformed)
		s.reply(index, dnswire.RcodeFormErr, nil)
		return true
	case dnswire.QueryNotImp:
		s.reply(index, dnswire.RcodeNotImp, nil)
		return true
	}
	query := &current.query
	switch {
	case query.HasOPT && query.EDNSVersion != 0:
		s.reply(index, dnswire.RcodeBadVers, nil)
		return true
	case query.Class != dnswire.ClassIN:
		s.reply(index, dnswire.RcodeRefused, nil)
		return true
	case query.Type != dnswire.TypeA && query.Type != dnswire.TypeAAAA:
		s.reply(index, dnswire.RcodeNotImp, nil)
		return true
	case !query.Name.Host(&s.host):
		s.reply(index, dnswire.RcodeRefused, nil)
		return true
	}
	if !s.host.IsLocalhost() && s.hosts != nil {
		if address, found := s.hosts.Address(&s.host); found {
			answer := dns.Answer{Result: dns.ResultOK, Source: dns.SourceHosts, Count: 1}
			answer.Addresses[0] = address
			s.reply(index, dnswire.RcodeNoError, &answer)
			return true
		}
	}
	answer, done := resolver.Lookup(s.base+index, &s.host, now)
	if !done {
		current.phase = requestResolving
		return true
	}
	s.reply(index, rcodeOf(answer.Result), &answer)
	return true
}

// reply writes request index's reply and makes it ready to send.
func (s *Server) reply(index int, rcode uint16, answer *dns.Answer) {
	current := &s.requests[index]
	var ttl uint32
	var addresses [][4]byte
	if answer != nil && answer.Result == dns.ResultOK {
		ttl, addresses = answer.TTL, answer.Addresses[:answer.Count]
	}
	current.length = dnswire.EncodeReply(current.reply[dnstcp.PrefixSize:], &current.query, rcode, ttl, addresses)
	binary.BigEndian.PutUint16(current.reply[:dnstcp.PrefixSize], uint16(current.length))
	current.phase = requestReady
	if rcode == dnswire.RcodeServFail {
		increment(&s.stats.Failures)
	}
}

// Complete takes a completion the resolver gave for client and reports
// whether the client is one of the server's.
func (s *Server) Complete(client int, answer *dns.Answer, now uint64) bool {
	index := client - s.base
	if index < 0 || index >= MaxRequests {
		return false
	}
	if s.requests[index].phase != requestResolving {
		return true
	}
	s.reply(index, rcodeOf(answer.Result), answer)
	if index >= MaxUDPRequests {
		current := &s.tcp.connections[index-MaxUDPRequests]
		current.phase, current.deadline = connectionWriting, now+IdleTimeout
		current.written, current.offered = 0, 0
	}
	return true
}

// cancel ends request index, telling the resolver if it waits there.
func (s *Server) cancel(resolver *dns.Resolver, index int) {
	if s.requests[index].phase == requestResolving {
		resolver.Cancel(s.base + index)
	}
	s.requests[index].phase = requestFree
}
