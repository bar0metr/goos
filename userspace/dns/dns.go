// Package dns is the protocol core of the resolver service: a stub resolver
// that forwards A queries to recursive servers and caches what they answer.
// It owns no clock and no socket. The service passes monotonic time, client
// lookups and datagrams in, and takes datagrams and completions out, so every
// parser and timer here runs on the host.
package dns

import "goos/userspace/network"

const (
	// DatagramSize is the largest query or response handled, and the UDP
	// payload size advertised in EDNS(0).
	DatagramSize = 1232
	// ClassicSize is the payload limit of a query without EDNS(0).
	ClassicSize     = 512
	MaxAddresses    = 8
	MaxServers      = 8 // 4 from DHCP and 4 configured
	MaxCacheEntries = 256
	// MaxClients bounds the client lookups waiting at one time.
	MaxClients = 16
	// MaxTransactions bounds concurrent upstream resolutions. Each has at
	// most one datagram outstanding.
	MaxTransactions = 4
	// MaxWaiters bounds the clients attached to one transaction.
	MaxWaiters    = 8
	MaxCNAMEChain = 8
	PolicyFields  = 6

	millisecond = uint64(1_000_000)
	second      = 1000 * millisecond
	maxCounter  = ^uint64(0)

	// minWait is the shortest time one transmission waits for an answer.
	minWait = 500 * millisecond
	// maxAttempts bounds the normal attempt policy for one queried name.
	maxAttempts = 3
	// maxTransmissions bounds all datagrams sent to one server during one
	// resolution, including EDNS fallback and CNAME target queries.
	maxTransmissions = 3
)

// Result values are wire values of the resolver client protocol; keep order.
type Result uint8

const (
	ResultOK Result = iota
	// ResultNotFound is NXDOMAIN.
	ResultNotFound
	// ResultNoData means the name exists and has no A record.
	ResultNoData
	ResultNoServers
	ResultTimeout
	// ResultTemporaryFailure covers SERVFAIL, REFUSED, unusable answers and
	// send failures from every server, and broken CNAME chains.
	ResultTemporaryFailure
	// ResultTruncated means every server that answered set TC=1. There is no
	// TCP fallback.
	ResultTruncated
	ResultInvalidName
	// ResultUnsupportedNameScope is .local, which needs multicast DNS.
	ResultUnsupportedNameScope
	ResultPermissionDenied
	ResultBusy
	// ResultReconfigured means the network changed while the lookup waited.
	ResultReconfigured
)

type Source uint8

const (
	SourceUpstream Source = iota
	SourceCache
	SourceHosts
	SourceLocalhost
)

// Answer is the outcome of one lookup.
type Answer struct {
	Result Result
	Source Source
	Count  uint8
	// Addresses holds Count addresses in the order the server sent them.
	Addresses [MaxAddresses][4]byte
	// TTL is how many seconds the answer stays valid: the remaining cache
	// lifetime for a hit, the lifetime it was cached with for an upstream
	// answer, and 0 for localhost and answers that were not cached.
	TTL uint32
}

type ServerSource uint8

const (
	ServerDHCP ServerSource = iota
	ServerConfig
)

type Server struct {
	Address [4]byte
	Source  ServerSource
}

// Policy is the cache and retry policy. It sizes nothing: the cache stays a
// fixed table of MaxCacheEntries and CacheEntries only limits how much of it
// is used.
type Policy struct {
	// CacheEntries is the active cache limit, 1..MaxCacheEntries.
	CacheEntries uint16
	// MaxTTL caps the lifetime of a positive answer, in seconds.
	MaxTTL uint32
	// MaxNegative caps the lifetime of NXDOMAIN and NODATA, in seconds.
	MaxNegative uint32
	// FailureTTL is how long a failed resolution is remembered, in seconds.
	FailureTTL uint32
	// QueryTimeout is in nanoseconds; see Resolver for how it shapes the
	// retransmission schedule.
	QueryTimeout uint64
	// Attempts is the number of normal attempts each server gets for one
	// queried name. Resolver also enforces maxTransmissions across the whole
	// resolution.
	Attempts uint8
}

// DefaultPolicy is the policy a resolver runs with until configuration says
// otherwise.
func DefaultPolicy() Policy {
	return Policy{
		CacheEntries: 64,
		MaxTTL:       86400,
		MaxNegative:  3600,
		FailureTTL:   5,
		QueryTimeout: 3000 * millisecond,
		Attempts:     2,
	}
}

// policyRange is the accepted range of each stored value, in ParsePolicy
// order. The query timeout is in milliseconds, as it is stored.
var policyRange = [PolicyFields]struct{ min, max uint64 }{
	{1, MaxCacheEntries}, // cache.entries
	{1, 604800},          // cache.max-ttl-sec
	{1, 86400},           // cache.max-negative-sec
	{1, 300},             // failure-cache-sec
	{500, 30_000},        // query-timeout-ms
	{1, maxAttempts},     // attempts
}

// ParsePolicy builds a policy from the stored values: cache.entries,
// cache.max-ttl-sec, cache.max-negative-sec, failure-cache-sec,
// query-timeout-ms and attempts. Range checks run before any conversion, so
// no value can wrap into an accepted one. It returns the index of the first
// value out of range, or -1.
func ParsePolicy(values *[PolicyFields]uint64) (Policy, int) {
	for index := range values {
		if values[index] < policyRange[index].min || values[index] > policyRange[index].max {
			return Policy{}, index
		}
	}
	return Policy{
		CacheEntries: uint16(values[0]),
		MaxTTL:       uint32(values[1]),
		MaxNegative:  uint32(values[2]),
		FailureTTL:   uint32(values[3]),
		QueryTimeout: values[4] * millisecond,
		Attempts:     uint8(values[5]),
	}, -1
}

// Validate returns the index of the first value outside its ParsePolicy
// range, or -1.
func (policy Policy) Validate() int {
	values := [PolicyFields]uint64{uint64(policy.CacheEntries), uint64(policy.MaxTTL), uint64(policy.MaxNegative),
		uint64(policy.FailureTTL), policy.QueryTimeout, uint64(policy.Attempts)}
	for index := range values {
		limits := policyRange[index]
		if index == 4 {
			limits.min, limits.max = limits.min*millisecond, limits.max*millisecond
		}
		if values[index] < limits.min || values[index] > limits.max {
			return index
		}
	}
	return -1
}

// ParseServers parses the four configured server slots. "0.0.0.0" or an
// empty slot means unused. An address must be strict dotted decimal (see
// network.ParseIPv4) naming one host: not 0.0.0.0/8, loopback, multicast,
// class E or broadcast. A slot that repeats an earlier one is refused. It
// returns the index of the first bad slot, or -1.
func ParseServers(texts *[4][]byte) ([4][4]byte, int) {
	var servers [4][4]byte
	for index := range texts {
		text := texts[index]
		if len(text) == 0 {
			continue
		}
		address, ok := network.ParseIPv4(text)
		if !ok {
			return [4][4]byte{}, index
		}
		if address == (network.IPv4{}) {
			continue
		}
		if !usableServer(address) {
			return [4][4]byte{}, index
		}
		for earlier := 0; earlier < index; earlier++ {
			if servers[earlier] == address {
				return [4][4]byte{}, index
			}
		}
		servers[index] = address
	}
	return servers, -1
}

// usableServer reports an address that can name one remote host.
func usableServer(address [4]byte) bool {
	return network.IPv4(address).IsUnicast() && address[0] != 0
}

func increment(counter *uint64) {
	if *counter != maxCounter {
		*counter++
	}
}

func addTime(now, delay uint64) uint64 {
	if now > maxCounter-delay {
		return maxCounter
	}
	return now + delay
}
