package dns

import (
	"encoding/binary"

	"goos/userspace/hostname"
)

type txPhase uint8

const (
	txFree txPhase = iota
	// txSend has a transmission built into its state that NextQuery has not
	// handed out yet.
	txSend
	// txWaiting has one datagram outstanding.
	txWaiting
	// txStream got TC=1 for its datagram and waits for the transport to take
	// the same query over TCP with NextStream.
	txStream
	// txStreamWaiting has the query out over TCP.
	txStreamWaiting
)

// outcome is how a server's last transmission for a transaction ended.
type outcome uint8

const (
	outcomeNone outcome = iota
	outcomeTimeout
	outcomeFailed
)

// transaction is one upstream resolution. chain[0] is the name clients asked
// for and chain[links] the name being asked now; every CNAME target seen is
// kept so a loop is caught when a name repeats.
type transaction struct {
	phase    txPhase
	name     hostname.Name
	chain    [MaxCNAMEChain + 1]wireName
	links    int
	chainTTL uint32
	attempts uint8
	// servers is the server list taken when the transaction started, so a
	// SetServers call cannot shift the index of the server being asked.
	servers     [MaxServers][4]byte
	serverCount int
	// sent counts normal attempts for the name at chain[links]. transmissions
	// counts every datagram across the complete resolution, and streams
	// every query repeated over TCP.
	sent          [MaxServers]uint8
	transmissions [MaxServers]uint8
	streams       [MaxServers]uint8
	outcomes      [MaxServers]outcome
	// classic marks servers that rejected EDNS(0) for this transaction.
	classic [MaxServers]bool
	server  int
	id      uint16
	edns    bool
	// serial numbers the exchanges of the transaction, so a transport can
	// tell its own from a later one that reused the transaction slot.
	serial uint32
	// wait ends the outstanding transmission and deadline the whole
	// resolution; wait never lies past deadline.
	wait     uint64
	deadline uint64
	waiters  int
}

type clientPhase uint8

const (
	clientIdle clientPhase = iota
	clientWaiting
	// clientDone holds a completion until the service takes it.
	clientDone
)

type client struct {
	phase       clientPhase
	transaction int
	answer      Answer
}

// Outgoing describes the query NextQuery or NextStream wrote. Serial
// names the exchange in StreamWaiting, ReceiveStream and StreamFailed.
type Outgoing struct {
	Transaction int
	Server      [4]byte
	Length      int
	Serial      uint32
}

// Stats is the cache occupancy and the counters, which saturate.
type Stats struct {
	CacheUsed, CacheLimit, Positive, Negative, Failures uint16

	PositiveHits, NegativeHits, FailureHits, Misses uint64
	// Queries counts datagrams handed out by NextQuery, Timeouts the
	// transmissions that got no answer in time.
	Queries, Timeouts uint64
	// Truncated counts matching answers with TC=1, over UDP and over TCP.
	Malformed, Mismatched, Truncated uint64
	// ServerFailures counts SERVFAIL, REFUSED, other error codes and send
	// failures.
	ServerFailures uint64
	// TCPFallbacks counts queries repeated over TCP, TCPAnswers the valid
	// answers that came back, TCPFailures the exchanges that broke and
	// TCPTimeouts those cut off by the resolver's timers.
	TCPFallbacks, TCPAnswers, TCPFailures, TCPTimeouts uint64
}

// Resolver is the whole resolver state. It holds no pointers and every table
// is a fixed array, so a service can keep one in static storage.
//
// Retransmission schedule. A resolution has one absolute deadline,
// QueryTimeout*Attempts after it starts, and asks one server at a time. The
// servers are tried in rounds, in list order: round k sends the (k+1)th
// transmission to every server still usable, and waits
//
//	min(QueryTimeout, max(500 ms, QueryTimeout*2^k / servers))
//
// for each, cut short by the deadline. A server stops being usable after
// SERVFAIL, REFUSED, another error code, a malformed answer or a send
// failure; a timeout keeps it for the next round. A server that answers
// FORMERR without OPT to a query with OPT is asked once more at once without
// EDNS(0); that retry does not use up a logical attempt. No server receives
// more than maxTransmissions datagrams during the resolution. The hard limit
// includes EDNS fallback and queries for CNAME targets. A CNAME whose target
// needs its own query starts the logical rounds again under the same deadline.
//
// TCP fallback. A UDP answer with TC=1 that matches the outstanding query
// sends the same query, with the same ID and the same OPT record or lack of
// one, to the same server over TCP. The fallback belongs to the attempt that
// drew TC=1 and uses up no logical attempt of its own. It waits at most
// QueryTimeout, cut short by the deadline, and no server receives more than
// maxStreams of them during the resolution. A valid answer over TCP is used
// like one over UDP. Anything else, TC=1 and FORMERR included, makes the
// server unusable, except a timeout, which keeps it for the next round.
type Resolver struct {
	random      uint64
	policy      Policy
	servers     [MaxServers]Server
	serverCount int
	clock       uint64
	cacheUsed   int
	sendCursor  int
	cursor      int

	positiveHits, negativeHits, failureHits, misses uint64
	queries, timeouts, malformed, mismatched        uint64
	truncated, serverFailures                       uint64
	streamCursor                                    int
	fallbacks, streamAnswers, streamFailures        uint64
	streamTimeouts                                  uint64

	clients      [MaxClients]client
	transactions [MaxTransactions]transaction
	cache        [MaxCacheEntries]entry

	// Scratch space for message parsing, kept here instead of on the stack.
	owner  wireName
	target wireName
	host   hostname.Name
}

// Init resets the resolver with no servers and an empty cache. An invalid
// policy is replaced by DefaultPolicy. Fields are reset one by one: a literal
// of the whole struct would be a temporary larger than TinyGo keeps off the
// heap.
func (r *Resolver) Init(seed uint64, policy Policy) {
	r.random = seed
	if policy.Validate() >= 0 {
		policy = DefaultPolicy()
	}
	r.policy = policy
	r.serverCount = 0
	r.clock = 0
	r.sendCursor, r.cursor = 0, 0
	r.positiveHits, r.negativeHits, r.failureHits, r.misses = 0, 0, 0, 0
	r.queries, r.timeouts, r.malformed, r.mismatched = 0, 0, 0, 0
	r.truncated, r.serverFailures = 0, 0
	r.streamCursor = 0
	r.fallbacks, r.streamAnswers, r.streamFailures, r.streamTimeouts = 0, 0, 0, 0
	for index := range r.clients[:] {
		r.clients[index] = client{}
	}
	for index := range r.transactions[:] {
		r.transactions[index].phase = txFree
		r.transactions[index].waiters = 0
	}
	r.Flush()
}

// next is splitmix64. It only has to make query IDs unpredictable to an
// off-path sender, given a seed from the service.
func (r *Resolver) next() uint64 {
	r.random += 0x9e3779b97f4a7c15
	value := r.random
	value = (value ^ value>>30) * 0xbf58476d1ce4e5b9
	value = (value ^ value>>27) * 0x94d049bb133111eb
	return value ^ value>>31
}

// Reseed mixes more entropy into the ID generator.
func (r *Resolver) Reseed(entropy uint64) {
	r.random ^= entropy
	r.random = r.next()
}

// SetPolicy applies a valid policy and ignores an invalid one. A smaller
// cache limit evicts at once, expired entries first, then the least recently
// used. Other values apply from the next use: running transactions keep
// their deadline and attempts, and cached entries keep their expiry.
func (r *Resolver) SetPolicy(policy Policy, now uint64) {
	if policy.Validate() >= 0 {
		return
	}
	r.policy = policy
	for r.cacheUsed > int(r.policy.CacheEntries) {
		r.evict(now)
	}
}

// Policy returns the policy in use.
func (r *Resolver) Policy() Policy { return r.policy }

// SetServers sets the upstream list: the valid DHCP servers in order, at most
// four, then the configured servers not already listed. Invalid, unused and
// repeated addresses are skipped. Running transactions keep the list they
// started with, and the cache is kept.
func (r *Resolver) SetServers(dhcp [][4]byte, configured [][4]byte) {
	r.serverCount = 0
	for index := range dhcp {
		if r.serverCount == MaxServers/2 {
			break
		}
		r.addServer(dhcp[index], ServerDHCP)
	}
	for index := range configured {
		r.addServer(configured[index], ServerConfig)
	}
}

func (r *Resolver) addServer(address [4]byte, source ServerSource) {
	// The explicit lower bound lets the compiler drop the index check, whose
	// panic path tools/stackframes cannot follow.
	if r.serverCount < 0 || r.serverCount >= MaxServers || !usableServer(address) {
		return
	}
	for index := 0; index < r.serverCount; index++ {
		if r.servers[index].Address == address {
			return
		}
	}
	r.servers[r.serverCount].Address, r.servers[r.serverCount].Source = address, source
	r.serverCount++
}

// Servers returns the upstream list in the order servers are asked.
func (r *Resolver) Servers() ([MaxServers]Server, int) {
	return r.servers, r.serverCount
}

func loopbackAnswer() Answer {
	answer := Answer{Result: ResultOK, Source: SourceLocalhost, Count: 1}
	answer.Addresses[0] = [4]byte{127, 0, 0, 1}
	return answer
}

// Lookup answers at once when it can and reports true: localhost and names
// under it are 127.0.0.1 without a query or cache entry, .local is
// ResultUnsupportedNameScope, a live cache entry answers from the cache, and
// an empty server list is ResultNoServers. Otherwise it attaches client as a
// waiter to the transaction for the name, joining one already running or
// starting one, and reports false; the answer arrives through Completion.
// A client still waiting or holding an untaken completion, an index out of
// range, or no free transaction or waiter slot is ResultBusy.
func (r *Resolver) Lookup(client int, name *hostname.Name, now uint64) (Answer, bool) {
	if name == nil || name.Size == 0 || int(name.Size) > hostname.MaxText {
		return Answer{Result: ResultInvalidName}, true
	}
	if name.IsLocalhost() {
		return loopbackAnswer(), true
	}
	if name.IsLocalScope() {
		return Answer{Result: ResultUnsupportedNameScope}, true
	}
	if answer, ok := r.cached(name, now); ok {
		return answer, true
	}
	increment(&r.misses)
	if r.serverCount == 0 {
		return Answer{Result: ResultNoServers}, true
	}
	if client < 0 || client >= MaxClients || r.clients[client].phase != clientIdle {
		return Answer{Result: ResultBusy}, true
	}
	number := r.findTransaction(name)
	if number < 0 {
		if number = r.start(name, now); number < 0 {
			return Answer{Result: ResultBusy}, true
		}
	} else if r.transactions[number].waiters >= MaxWaiters {
		return Answer{Result: ResultBusy}, true
	}
	r.transactions[number].waiters++
	r.clients[client].phase = clientWaiting
	r.clients[client].transaction = number
	return Answer{}, false
}

func (r *Resolver) findTransaction(name *hostname.Name) int {
	for index := range r.transactions[:] {
		if r.transactions[index].phase != txFree && r.transactions[index].name.Equal(name) {
			return index
		}
	}
	return -1
}

func (r *Resolver) start(name *hostname.Name, now uint64) int {
	number := -1
	for index := range r.transactions[:] {
		if r.transactions[index].phase == txFree {
			number = index
			break
		}
	}
	if number < 0 {
		return -1
	}
	tx := &r.transactions[number]
	tx.name = *name
	tx.chain[0].setHost(name)
	tx.links = 0
	tx.chainTTL = ^uint32(0)
	tx.attempts = r.policy.Attempts
	tx.serverCount = r.serverCount
	for index := 0; index < r.serverCount; index++ {
		tx.servers[index] = r.servers[index].Address
		tx.classic[index] = false
		tx.transmissions[index] = 0
		tx.streams[index] = 0
	}
	tx.waiters = 0
	tx.deadline = addTime(now, r.policy.QueryTimeout*uint64(tx.attempts))
	_ = r.restart(tx, now)
	return number
}

// restart begins the logical rounds for the name being asked now. The hard
// transmission counters survive CNAME target changes.
func (r *Resolver) restart(tx *transaction, now uint64) bool {
	for index := 0; index < tx.serverCount; index++ {
		tx.sent[index] = 0
		tx.outcomes[index] = outcomeNone
	}
	for server := 0; server < tx.serverCount; server++ {
		if r.transmit(tx, server, true, now) {
			return true
		}
	}
	return false
}

// transmit schedules the next datagram to a server. A counted transmission
// uses up a logical attempt; the retry without EDNS(0) does not. Every
// scheduled datagram must fit the hard per-server transmission budget.
func (r *Resolver) transmit(tx *transaction, server int, counted bool, now uint64) bool {
	if tx.transmissions[server] >= maxTransmissions {
		return false
	}
	round := int(tx.sent[server])
	if counted {
		tx.sent[server]++
	} else if round > 0 {
		round--
	}
	wait := r.policy.QueryTimeout << uint(round) / uint64(tx.serverCount)
	if wait < minWait {
		wait = minWait
	}
	if wait > r.policy.QueryTimeout {
		wait = r.policy.QueryTimeout
	}
	tx.wait = addTime(now, wait)
	if tx.wait > tx.deadline {
		tx.wait = tx.deadline
	}
	tx.server = server
	tx.edns = !tx.classic[server]
	id := uint16(r.next())
	for draw := 0; id == tx.id && draw < 4; draw++ {
		id = uint16(r.next())
	}
	if id == tx.id {
		id = ^id
	}
	tx.id = id
	tx.serial++
	tx.phase = txSend
	return true
}

// fallBack schedules the query that drew TC=1 again over TCP, to the same
// server with the same ID and form. It does not use up a logical attempt.
func (r *Resolver) fallBack(tx *transaction, now uint64) {
	if tx.streams[tx.server] >= maxStreams {
		r.moveOn(tx, outcomeFailed, now)
		return
	}
	tx.wait = addTime(now, r.policy.QueryTimeout)
	if tx.wait > tx.deadline {
		tx.wait = tx.deadline
	}
	tx.serial++
	tx.phase = txStream
}

// moveOn records how the current server's transmission ended and schedules
// the next one, or ends the resolution when no server is left or the deadline
// has passed.
func (r *Resolver) moveOn(tx *transaction, result outcome, now uint64) {
	tx.outcomes[tx.server] = result
	if now < tx.deadline {
		for step := 1; step <= tx.serverCount; step++ {
			server := (tx.server + step) % tx.serverCount
			if tx.outcomes[server] != outcomeTimeout && tx.outcomes[server] != outcomeNone {
				continue
			}
			if tx.sent[server] < tx.attempts && r.transmit(tx, server, true, now) {
				return
			}
		}
	}
	r.fail(tx, r.failureResult(tx), now)
}

// failureResult sums up the servers asked: Timeout when every one timed out,
// over UDP or over TCP, and TemporaryFailure for anything else.
func (r *Resolver) failureResult(tx *transaction) Result {
	for index := 0; index < tx.serverCount; index++ {
		if tx.sent[index] != 0 && tx.outcomes[index] != outcomeTimeout {
			return ResultTemporaryFailure
		}
	}
	return ResultTimeout
}

// NextQuery writes the next datagram to send into dst and reports which
// transaction and server it is for. A dst shorter than DatagramSize leaves
// the datagram queued. Transactions are scanned from a rotating start so one
// cannot starve the others.
func (r *Resolver) NextQuery(dst []byte) (Outgoing, bool) {
	if len(dst) < DatagramSize {
		return Outgoing{}, false
	}
	for step := 0; step < MaxTransactions; step++ {
		number := (r.sendCursor + step) % MaxTransactions
		tx := &r.transactions[number]
		if tx.phase != txSend {
			continue
		}
		r.sendCursor = (number + 1) % MaxTransactions
		tx.phase = txWaiting
		tx.transmissions[tx.server]++
		increment(&r.queries)
		length := encodeQuery(dst, tx.id, &tx.chain[tx.links], tx.edns)
		return Outgoing{Transaction: number, Server: tx.servers[tx.server], Length: length, Serial: tx.serial}, true
	}
	return Outgoing{}, false
}

// NextStream writes the next query to repeat over TCP into dst, without the
// two-byte length prefix, and reports which transaction and server it is
// for. It is the query that drew TC=1, byte for byte. A dst shorter than
// MaxQuery leaves it queued. A transport with no room for another TCP
// exchange simply does not call NextStream: the transaction keeps waiting,
// at most until its wait ends.
func (r *Resolver) NextStream(dst []byte) (Outgoing, bool) {
	if len(dst) < MaxQuery {
		return Outgoing{}, false
	}
	for step := 0; step < MaxTransactions; step++ {
		number := (r.streamCursor + step) % MaxTransactions
		tx := &r.transactions[number]
		if tx.phase != txStream {
			continue
		}
		r.streamCursor = (number + 1) % MaxTransactions
		tx.phase = txStreamWaiting
		tx.streams[tx.server]++
		increment(&r.fallbacks)
		length := encodeQuery(dst, tx.id, &tx.chain[tx.links], tx.edns)
		return Outgoing{Transaction: number, Server: tx.servers[tx.server], Length: length, Serial: tx.serial}, true
	}
	return Outgoing{}, false
}

// StreamWaiting reports whether transaction still waits for the TCP
// exchange serial. Once it does not, the transport closes the connection.
func (r *Resolver) StreamWaiting(transaction int, serial uint32) bool {
	return transaction >= 0 && transaction < MaxTransactions && r.transactions[transaction].phase == txStreamWaiting &&
		r.transactions[transaction].serial == serial
}

// StreamFailed reports that the TCP exchange serial ended without a whole
// answer. timedOut marks a TCP timeout, which keeps the server for the next
// round; anything else makes it unusable.
func (r *Resolver) StreamFailed(transaction int, serial uint32, timedOut bool, now uint64) {
	if !r.StreamWaiting(transaction, serial) {
		return
	}
	tx := &r.transactions[transaction]
	if timedOut {
		increment(&r.timeouts)
		increment(&r.streamTimeouts)
		r.moveOn(tx, outcomeTimeout, now)
		return
	}
	increment(&r.streamFailures)
	r.moveOn(tx, outcomeFailed, now)
}

// ReceiveStream takes the one DNS message the TCP exchange serial carried,
// length prefix removed. It is checked like a datagram, but there is no
// waiting for a better one: a message that does not answer the query, or
// carries TC=1, ends the exchange and makes the server unusable.
func (r *Resolver) ReceiveStream(transaction int, serial uint32, payload []byte, now uint64) {
	if !r.StreamWaiting(transaction, serial) {
		return
	}
	tx := &r.transactions[transaction]
	var message response
	result := parseResponse(payload, MaxStreamMessage, tx.id, &tx.chain[tx.links], &r.owner, &message)
	switch {
	case result == parseMismatched:
		increment(&r.mismatched)
	case result == parseMalformed:
		increment(&r.malformed)
	case message.truncated:
		increment(&r.truncated)
	default:
		increment(&r.streamAnswers)
		r.use(tx, payload, &message, true, now)
		return
	}
	increment(&r.streamFailures)
	r.moveOn(tx, outcomeFailed, now)
}

// Waiting reports whether transaction waits for an answer to the last query
// NextQuery gave out for it. Once it does not, the transport can drop
// whatever it kept for that query.
func (r *Resolver) Waiting(transaction int) bool {
	return transaction >= 0 && transaction < MaxTransactions && r.transactions[transaction].phase == txWaiting
}

// SendFailed reports that the transport could not send the transaction's
// last datagram. The server counts as unusable, as after a bad answer.
func (r *Resolver) SendFailed(transaction int, now uint64) {
	if transaction < 0 || transaction >= MaxTransactions || r.transactions[transaction].phase != txWaiting {
		return
	}
	increment(&r.serverFailures)
	r.moveOn(&r.transactions[transaction], outcomeFailed, now)
}

// Receive takes a datagram the transport matched to transaction by address
// and port. The server must be the one asked and the message must answer the
// outstanding query; otherwise it counts as Mismatched and the query keeps
// waiting. A malformed answer counts as Malformed and makes the server
// unusable. A matching answer with TC=1 is not used: the same query goes to
// the same server over TCP. Nothing from a rejected or truncated message
// reaches the cache.
func (r *Resolver) Receive(transaction int, server [4]byte, payload []byte, now uint64) {
	if transaction < 0 || transaction >= MaxTransactions {
		increment(&r.mismatched)
		return
	}
	tx := &r.transactions[transaction]
	if tx.phase != txWaiting || server != tx.servers[tx.server] {
		increment(&r.mismatched)
		return
	}
	var message response
	switch parseResponse(payload, DatagramSize, tx.id, &tx.chain[tx.links], &r.owner, &message) {
	case parseMismatched:
		increment(&r.mismatched)
		return
	case parseMalformed:
		increment(&r.malformed)
		r.moveOn(tx, outcomeFailed, now)
		return
	}
	if message.truncated {
		increment(&r.truncated)
		r.fallBack(tx, now)
		return
	}
	r.use(tx, payload, &message, false, now)
}

// use acts on a validated answer by its rcode. The retry without EDNS(0)
// applies to datagrams only: over TCP the query keeps the form that drew
// TC=1, and FORMERR fails the server.
func (r *Resolver) use(tx *transaction, payload []byte, message *response, stream bool, now uint64) {
	switch message.rcode {
	case rcodeNoError, rcodeNXDomain:
		r.interpret(tx, payload, message, now)
	case rcodeFormErr:
		if !stream && tx.edns && !message.hasOPT {
			tx.classic[tx.server] = true
			if r.transmit(tx, tx.server, false, now) {
				return
			}
			r.moveOn(tx, outcomeFailed, now)
			return
		}
		increment(&r.serverFailures)
		r.moveOn(tx, outcomeFailed, now)
	default:
		increment(&r.serverFailures)
		r.moveOn(tx, outcomeFailed, now)
	}
}

// interpret uses a validated NOERROR or NXDOMAIN answer. NXDOMAIN after a
// CNAME chain applies to the end of the chain, so the asked name does not
// resolve either (RFC 6604). A NOERROR answer that ends a chain without A
// records and without an SOA is followed by a query for the chain's end.
func (r *Resolver) interpret(tx *transaction, message []byte, header *response, now uint64) {
	links := tx.links
	if !r.followChain(tx, message, header) {
		r.fail(tx, ResultTemporaryFailure, now)
		return
	}
	if header.rcode == rcodeNXDomain {
		r.negative(tx, message, header, ResultNotFound, now)
		return
	}
	var answer Answer
	ttl := r.collect(tx, message, header, &answer)
	if answer.Count > 0 {
		r.positive(tx, &answer, ttl, now)
		return
	}
	if _, ok := r.findSOA(message, header, &tx.chain[tx.links]); ok || tx.links == links {
		r.negative(tx, message, header, ResultNoData, now)
		return
	}
	if !r.restart(tx, now) {
		r.fail(tx, ResultTemporaryFailure, now)
	}
}

// followChain extends the transaction's chain with the CNAME records of the
// answer section. It fails on a loop or a chain longer than MaxCNAMEChain.
//
//go:noinline
func (r *Resolver) followChain(tx *transaction, message []byte, header *response) bool {
	var item record
	for {
		found := false
		position := header.answerAt
		for index := 0; index < header.answers; index++ {
			next, _ := readRecord(message, position, &r.owner, &item)
			position = next
			if item.kind != typeCNAME || item.class != classIN || !r.owner.equal(&tx.chain[tx.links]) {
				continue
			}
			decodeName(message, item.data, &r.target)
			for seen := 0; seen <= tx.links; seen++ {
				if tx.chain[seen].equal(&r.target) {
					return false
				}
			}
			if tx.links == MaxCNAMEChain {
				return false
			}
			tx.links++
			tx.chain[tx.links].copyFrom(&r.target)
			if ttl := recordTTL(item.ttl); ttl < tx.chainTTL {
				tx.chainTTL = ttl
			}
			found = true
			break
		}
		if !found {
			return true
		}
	}
}

// collect copies the A records for the end of the chain into answer, in
// message order, and returns the smallest TTL among those used.
//
//go:noinline
func (r *Resolver) collect(tx *transaction, message []byte, header *response, answer *Answer) uint32 {
	var item record
	ttl := ^uint32(0)
	position := header.answerAt
	for index := 0; index < header.answers && answer.Count < MaxAddresses; index++ {
		next, _ := readRecord(message, position, &r.owner, &item)
		position = next
		if item.kind != typeA || item.class != classIN || !r.owner.equal(&tx.chain[tx.links]) {
			continue
		}
		copy(answer.Addresses[answer.Count][:], message[item.data:item.data+4])
		answer.Count++
		if value := recordTTL(item.ttl); value < ttl {
			ttl = value
		}
	}
	return ttl
}

// findSOA returns the negative-caching TTL of RFC 2308, the smaller of the
// SOA record's TTL and its MINIMUM field, from the first SOA in the authority
// section whose owner is name or an ancestor of it.
//
//go:noinline
func (r *Resolver) findSOA(message []byte, header *response, name *wireName) (uint32, bool) {
	var item record
	position := header.authorityAt
	for index := 0; index < header.authorities; index++ {
		next, _ := readRecord(message, position, &r.owner, &item)
		position = next
		if item.kind != typeSOA || item.class != classIN || !name.within(&r.owner) {
			continue
		}
		ttl := recordTTL(item.ttl)
		minimum := recordTTL(binary.BigEndian.Uint32(message[item.data+item.length-4:]))
		if minimum < ttl {
			ttl = minimum
		}
		return ttl, true
	}
	return 0, false
}

func capTTL(ttl, limit uint32) uint32 {
	if ttl > limit {
		return limit
	}
	return ttl
}

// positive caches an address answer under the asked name and, when a CNAME
// chain led elsewhere, under the canonical name with that name's own TTL.
func (r *Resolver) positive(tx *transaction, answer *Answer, ttl uint32, now uint64) {
	answer.Result = ResultOK
	answer.Source = SourceUpstream
	if tx.links > 0 && tx.chain[tx.links].host(&r.host) && !r.host.Equal(&tx.name) {
		r.store(&r.host, entryPositive, ResultOK, &answer.Addresses, answer.Count, capTTL(ttl, r.policy.MaxTTL), now)
	}
	if tx.chainTTL < ttl {
		ttl = tx.chainTTL
	}
	answer.TTL = capTTL(ttl, r.policy.MaxTTL)
	r.store(&tx.name, entryPositive, ResultOK, &answer.Addresses, answer.Count, answer.TTL, now)
	r.finish(tx, answer)
}

// negative caches NXDOMAIN or NODATA only when the authority section holds a
// usable SOA; without one the answer is passed on uncached.
func (r *Resolver) negative(tx *transaction, message []byte, header *response, result Result, now uint64) {
	answer := Answer{Result: result, Source: SourceUpstream}
	if ttl, ok := r.findSOA(message, header, &tx.chain[tx.links]); ok {
		if tx.chainTTL < ttl {
			ttl = tx.chainTTL
		}
		answer.TTL = capTTL(ttl, r.policy.MaxNegative)
		kind := entryNXDomain
		if result == ResultNoData {
			kind = entryNoData
		}
		r.store(&tx.name, kind, result, nil, 0, answer.TTL, now)
	}
	r.finish(tx, &answer)
}

// fail ends a resolution that produced no answer and remembers the failure
// for FailureTTL.
func (r *Resolver) fail(tx *transaction, result Result, now uint64) {
	answer := Answer{Result: result, Source: SourceUpstream, TTL: r.policy.FailureTTL}
	r.store(&tx.name, entryFailure, result, nil, 0, answer.TTL, now)
	r.finish(tx, &answer)
}

// finish gives every waiter the same answer and frees the transaction.
func (r *Resolver) finish(tx *transaction, answer *Answer) {
	number := r.transactionIndex(tx)
	for index := range r.clients[:] {
		item := &r.clients[index]
		if item.phase == clientWaiting && item.transaction == number {
			item.phase = clientDone
			item.answer = *answer
		}
	}
	tx.phase = txFree
	tx.waiters = 0
}

func (r *Resolver) transactionIndex(tx *transaction) int {
	for index := range r.transactions[:] {
		if &r.transactions[index] == tx {
			return index
		}
	}
	return -1
}

// Cancel forgets a client's lookup without a completion, for a client that
// has gone away. A transaction left without waiters is dropped.
func (r *Resolver) Cancel(client int) {
	if client < 0 || client >= MaxClients {
		return
	}
	item := &r.clients[client]
	if item.phase == clientWaiting {
		tx := &r.transactions[item.transaction]
		tx.waiters--
		if tx.waiters <= 0 {
			tx.phase = txFree
			tx.waiters = 0
		}
	}
	item.phase = clientIdle
}

// Reset fails every waiting client with ResultReconfigured and drops every
// transaction, for a network change that makes outstanding queries
// meaningless. The cache is kept.
func (r *Resolver) Reset(now uint64) {
	for index := range r.clients[:] {
		item := &r.clients[index]
		if item.phase == clientWaiting {
			item.phase = clientDone
			item.answer = Answer{Result: ResultReconfigured}
		}
	}
	for index := range r.transactions[:] {
		r.transactions[index].phase = txFree
		r.transactions[index].waiters = 0
	}
}

// Advance runs every timer due at now: a transmission whose wait ended moves
// on to the next server, or ends the resolution at its deadline.
func (r *Resolver) Advance(now uint64) {
	for index := range r.transactions[:] {
		tx := &r.transactions[index]
		if tx.phase != txFree && tx.wait <= now {
			increment(&r.timeouts)
			if tx.phase == txStream || tx.phase == txStreamWaiting {
				increment(&r.streamTimeouts)
			}
			r.moveOn(tx, outcomeTimeout, now)
		}
	}
}

// Deadline returns the earliest pending timer, or false when only a datagram
// or a request can change anything.
func (r *Resolver) Deadline() (uint64, bool) {
	earliest, found := uint64(0), false
	for index := range r.transactions[:] {
		tx := &r.transactions[index]
		if tx.phase != txFree && (!found || tx.wait < earliest) {
			earliest, found = tx.wait, true
		}
	}
	return earliest, found
}

// Completion takes one finished lookup. Clients are scanned from a rotating
// start so one busy client cannot starve the others.
func (r *Resolver) Completion() (int, Answer, bool) {
	for step := 0; step < MaxClients; step++ {
		number := (r.cursor + step) % MaxClients
		item := &r.clients[number]
		if item.phase != clientDone {
			continue
		}
		item.phase = clientIdle
		r.cursor = (number + 1) % MaxClients
		return number, item.answer, true
	}
	return 0, Answer{}, false
}

// Stats reports cache occupancy, expired entries included until they are
// evicted or looked up, and the counters.
func (r *Resolver) Stats() Stats {
	stats := Stats{
		CacheUsed:      uint16(r.cacheUsed),
		CacheLimit:     r.policy.CacheEntries,
		PositiveHits:   r.positiveHits,
		NegativeHits:   r.negativeHits,
		FailureHits:    r.failureHits,
		Misses:         r.misses,
		Queries:        r.queries,
		Timeouts:       r.timeouts,
		Malformed:      r.malformed,
		Mismatched:     r.mismatched,
		Truncated:      r.truncated,
		ServerFailures: r.serverFailures,
		TCPFallbacks:   r.fallbacks,
		TCPAnswers:     r.streamAnswers,
		TCPFailures:    r.streamFailures,
		TCPTimeouts:    r.streamTimeouts,
	}
	for index := range r.cache[:] {
		switch r.cache[index].kind {
		case entryPositive:
			stats.Positive++
		case entryNXDomain, entryNoData:
			stats.Negative++
		case entryFailure:
			stats.Failures++
		}
	}
	return stats
}
