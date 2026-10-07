//go:build goos_userspace

// Command resolved is the resolver service (ADR 0194): a stub resolver with a
// cache that turns host names into IPv4 addresses for commands. It answers
// localhost and names under it itself, asks the configuration service's hosts
// table next, and only then its cache and the recursive DNS servers named by
// the DHCP lease and by system.dns.server1..4. It reaches those servers only
// through the resolver transport the network service gives it; it holds no
// device and no general network session.
package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	config "goos/userspace/config"
	"goos/userspace/dns"
	"goos/userspace/hostname"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/resolvapi"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

const (
	idleWait     = 30 * clock.TicksPerSecond
	nanosPerTick = 1_000_000_000 / clock.TicksPerSecond
	// configDeadline bounds one configuration exchange, configBudget a
	// whole reload, and hostsDeadline one hosts lookup.
	configDeadline = 2 * clock.TicksPerSecond
	configBudget   = 5 * clock.TicksPerSecond
	hostsDeadline  = clock.TicksPerSecond / 2
	maxSessions    = dns.MaxClients
	exitFailed     = 1
)

// session is one command's channel, what PID 1 said it may do, and the
// correlation of its lookup in flight, zero when there is none.
type session struct {
	channel handle.ID
	rights  netapi.RightsClass
	lookup  uint64
}

// exchange is one query the network service has accepted or is accepting:
// the resolver transaction it belongs to, under the tag the transport knows
// it by.
type exchange struct {
	tag         uint64
	transaction int
}

type binding struct {
	channel    handle.ID
	generation uint64
	queryPool  handle.ID
	replyPool  handle.ID
	queryBase  uint64
	querySize  uint64
	replyBase  uint64
	replySize  uint64
	queries    []byte
	replies    []byte
	// lent marks query slots the network service holds.
	lent [netapi.DatagramSlots]bool
	// open holds the exchange behind each query slot until it ends.
	open [netapi.DatagramSlots]exchange
	// servers is the DHCP server list last reported, under its generation.
	servers          [netapi.MaxServers][4]byte
	serverCount      int
	serverGeneration uint64
}

var (
	consoleHandle, controlHandle, configHandle handle.ID

	resolver  dns.Resolver
	transport binding
	sessions  [maxSessions]session
	// configured is the server list from system.dns.server1..4.
	configured      [4][4]byte
	configuredCount int
	nextTag         uint64

	interests     [kcall.WaitAnyMaxObjects]sys.Interest
	interestOwner [kcall.WaitAnyMaxObjects]int

	wire          [ipc.WireMaxSize]byte
	configWire    [ipc.WireMaxSize]byte
	message       ipc.Message
	reply         ipc.Message
	configMessage ipc.Message
	configClient  config.Client
	request       resolvapi.Request
	answer        resolvapi.Reply
	control       netapi.ControlRequest
	controlBack   netapi.ControlReply
	datagram      netapi.Datagram
	query         [dns.DatagramSize]byte
	settings      [resolvapi.Keys]config.Value
)

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("RESOLVED_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("RESOLVED_STARTUP_INVALID\n")
	}
	consoleHandle, _ = data.Handle("console")
	syslog.Init(consoleHandle, "resolved")
	if controlHandle, ok = data.Handle("control"); !ok {
		fatal("RESOLVED_CONTROL_MISSING\n")
	}
	configHandle, _ = data.Handle("config")
	seed, status := sys.RandomUint64()
	if status != kcall.StatusOK {
		fatal("RESOLVED_RANDOM_FAILED\n")
	}
	resolver.Init(seed, dns.DefaultPolicy())
	if field := reload(); field != 0 {
		syslog.Warn("resolver settings invalid; using defaults", syslog.Str("key", resolvapi.Key(field)))
	}
	mark("RESOLVED_STARTED_OK\n")
	serve()
}

func serve() {
	start := arena.Current()
	for {
		arena.Reset(start)
		now := clockNanos()
		resolver.Advance(now)
		sendQueries(now)
		deliverAnswers()
		count := buildInterests()
		timeout := uint64(idleWait)
		if deadline, pending := resolver.Deadline(); pending {
			timeout = 1
			if deadline > now {
				timeout = max(min((deadline-now+nanosPerTick-1)/nanosPerTick, uint64(idleWait)), 1)
			}
		}
		ready, status := sys.WaitAny(&interests, count, timeout)
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK {
			fatal("RESOLVED_WAIT_FAILED\n")
		}
		for index := 0; index < count; index++ {
			if ready&(1<<index) == 0 {
				continue
			}
			switch owner := interestOwner[index]; owner {
			case -1:
				serviceTransport()
			case -2:
				serviceControl()
			default:
				serviceSession(owner)
			}
		}
	}
}

func buildInterests() int {
	count := 0
	add := func(channel handle.ID, owner int) {
		interests[count] = sys.Interest{Handle: channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interestOwner[count] = owner
		count++
	}
	if transport.channel != 0 {
		add(transport.channel, -1)
	}
	add(controlHandle, -2)
	for index := range sessions {
		if sessions[index].channel != 0 {
			add(sessions[index].channel, index)
		}
	}
	return count
}

func receiveOn(channel handle.ID) kcall.Status {
	status := sys.IPCReceive(channel, wire[:])
	if status != kcall.StatusOK {
		return status
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &message) {
		return kcall.StatusCorruptData
	}
	return kcall.StatusOK
}

func sendOn(channel handle.ID, value *ipc.Message) bool {
	size, ok := ipc.EncodeFrom(value, wire[:])
	return ok && sys.IPCSend(channel, wire[:size]) == kcall.StatusOK
}

func closeAttached() {
	for _, attached := range message.Handles[:message.HandleCount] {
		if attached != 0 {
			sys.HandleClose(attached)
		}
	}
}

// --- control --------------------------------------------------------------

func serviceControl() {
	for {
		status := receiveOn(controlHandle)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			fatal("RESOLVED_CONTROL_GONE\n")
		}
		if status != kcall.StatusOK {
			closeAttached()
			continue
		}
		if renewed, isNotice := config.DecodeSessionNotice(&message); isNotice {
			if configHandle != 0 {
				sys.HandleClose(configHandle)
			}
			configHandle = renewed
			syslog.Info("configuration session renewed")
			continue
		}
		correlation, valid := netapi.DecodeControlRequest(&message, &control)
		if !valid {
			opcode, attributed, known := netapi.ControlRequestOpcode(&message)
			closeAttached()
			if known {
				answerControl(opcode, netapi.ControlInvalidArgument, 0, attributed)
			}
			continue
		}
		switch control.Opcode {
		case netapi.ControlAddSession:
			answerControl(control.Opcode, addSession(message.Handles[0], control.Rights), 0, correlation)
		case netapi.ControlSetTransport:
			answerControl(control.Opcode, setTransport(control.Generation), control.Generation, correlation)
		default:
			closeAttached()
			answerControl(control.Opcode, netapi.ControlInvalidArgument, 0, correlation)
		}
	}
}

func answerControl(opcode netapi.ControlOpcode, result netapi.ControlResult, generation, correlation uint64) {
	controlBack = netapi.ControlReply{Opcode: opcode, Result: result, Generation: generation}
	if netapi.EncodeControlReply(&controlBack, correlation, &reply) {
		sendOn(controlHandle, &reply)
	}
}

// handleIs checks a received handle's type and exact rights.
func handleIs(id handle.ID, typeID handle.Type, rights capability.Rights) bool {
	var record [kcall.HandleInfoSize]byte
	slot := uint64(uint32(id)) - 1
	if uint32(id) == 0 || sys.HandleInfo(slot, &record) != kcall.StatusOK {
		return false
	}
	recordID := uint64(record[0]) | uint64(record[1])<<8 | uint64(record[2])<<16 | uint64(record[3])<<24 |
		uint64(record[4])<<32 | uint64(record[5])<<40 | uint64(record[6])<<48 | uint64(record[7])<<56
	got := capability.Rights(uint32(record[12]) | uint32(record[13])<<8 | uint32(record[14])<<16 | uint32(record[15])<<24)
	return recordID == uint64(id) && handle.Type(record[8]) == typeID && got == rights
}

// addSession takes a command's session. The resolver knows two classes:
// resolve and status, or that plus flush and reload.
func addSession(channel handle.ID, class netapi.RightsClass) netapi.ControlResult {
	if class != netapi.RightsResolveStatus && class != netapi.RightsAdminStatus ||
		!handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) {
		sys.HandleClose(channel)
		return netapi.ControlInvalidArgument
	}
	for index := range sessions {
		if sessions[index].channel == 0 {
			sessions[index] = session{channel: channel, rights: class}
			return netapi.ControlOK
		}
	}
	sys.HandleClose(channel)
	return netapi.ControlBusy
}

// --- transport --------------------------------------------------------------

// setTransport installs a new binding to the network service. Exchanges on
// the old one can no longer finish, so every lookup waiting on them fails
// with Reconfigured; the cache stays.
func setTransport(generation uint64) netapi.ControlResult {
	channel, queries, replies := message.Handles[0], message.Handles[1], message.Handles[2]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, queries, replies} {
			sys.HandleClose(id)
		}
		return result
	}
	if generation <= transport.generation {
		return refuse(netapi.ControlInvalidArgument)
	}
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) ||
		!handleIs(queries, handle.TypeMemory, capability.Read|capability.Write) || !handleIs(replies, handle.TypeMemory, capability.Read) {
		return refuse(netapi.ControlInvalidArgument)
	}
	poolSize := uint64(netapi.DatagramSlots * netapi.DatagramSlotSize)
	queryBase, querySize, queryStatus := sys.MemoryMapAnywhere(queries, true)
	if queryStatus != kcall.StatusOK || querySize < poolSize {
		if queryStatus == kcall.StatusOK {
			sys.VMUnmap(queryBase, querySize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	replyBase, replySize, replyStatus := sys.MemoryMapAnywhere(replies, false)
	if replyStatus != kcall.StatusOK || replySize < poolSize {
		sys.VMUnmap(queryBase, querySize)
		if replyStatus == kcall.StatusOK {
			sys.VMUnmap(replyBase, replySize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	closeTransport()
	transport = binding{channel: channel, generation: generation, queryPool: queries, replyPool: replies,
		queryBase: queryBase, querySize: querySize, replyBase: replyBase, replySize: replySize,
		queries: sys.Window(queryBase, poolSize), replies: sys.Window(replyBase, poolSize)}
	resolver.Reset(clockNanos())
	applyServers()
	syslog.Info("transport bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// closeTransport drops the binding. The DHCP servers belonged to its lease
// as far as this service can tell, so they go too.
func closeTransport() {
	if transport.channel == 0 {
		return
	}
	for _, id := range [...]handle.ID{transport.channel, transport.queryPool, transport.replyPool} {
		sys.HandleClose(id)
	}
	for _, mapping := range [...][2]uint64{{transport.queryBase, transport.querySize}, {transport.replyBase, transport.replySize}} {
		sys.VMUnmap(mapping[0], mapping[1])
	}
	generation := transport.generation
	transport = binding{generation: generation}
	resolver.Reset(clockNanos())
	applyServers()
}

func serviceTransport() {
	for transport.channel != 0 {
		status := receiveOn(transport.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("network service gone; waiting for a new transport")
			closeTransport()
			return
		}
		if status != kcall.StatusOK || !netapi.DecodeDatagram(&message, &datagram) {
			closeAttached()
			syslog.Error("transport message invalid")
			closeTransport()
			return
		}
		now := clockNanos()
		switch datagram.Opcode {
		case netapi.DatagramAccepted:
			if !transport.lent[datagram.Slot] || transport.open[datagram.Slot].tag != datagram.Tag {
				syslog.Error("transport returned a slot it did not hold")
				closeTransport()
				return
			}
			transport.lent[datagram.Slot] = false
			if datagram.Result != netapi.ResultOK {
				resolver.SendFailed(transport.open[datagram.Slot].transaction, now)
				transport.open[datagram.Slot] = exchange{}
			}
		case netapi.DatagramResult:
			receiveResult(now)
		case netapi.DatagramServers:
			if datagram.Generation <= transport.serverGeneration {
				continue
			}
			transport.serverGeneration, transport.serverCount = datagram.Generation, int(datagram.Count)
			transport.servers = datagram.Servers
			applyServers()
		default:
			closeTransport()
			return
		}
	}
}

// receiveResult ends an accepted exchange. A reply goes to its transaction,
// which checks it against the query it sent; the reply slot goes straight
// back. A timeout is the resolver's own business: it keeps its own timers.
func receiveResult(now uint64) {
	transaction := -1
	for index := range transport.open {
		if transport.open[index].tag == datagram.Tag {
			transaction = transport.open[index].transaction
			transport.open[index] = exchange{}
			break
		}
	}
	switch {
	case datagram.Result == netapi.ResultOK:
		offset, _ := netapi.DatagramSlotOffset(datagram.Slot)
		if transaction >= 0 {
			resolver.Receive(transaction, datagram.Address, transport.replies[offset:offset+uint64(datagram.Length)], now)
		}
		release := netapi.Datagram{Opcode: netapi.DatagramRelease, Slot: datagram.Slot}
		if !netapi.EncodeDatagram(&release, &reply) || !sendOn(transport.channel, &reply) {
			closeTransport()
		}
	case datagram.Result != netapi.ResultTimeout && transaction >= 0:
		resolver.SendFailed(transaction, now)
	}
}

// sendQueries moves every query the resolver has ready into a free slot. A
// query that cannot go, for want of a transport or a slot, fails at once so
// the resolver tries its next server or gives up.
func sendQueries(now uint64) {
	for {
		outgoing, ready := resolver.NextQuery(query[:])
		if !ready {
			return
		}
		slot := -1
		if transport.channel != 0 {
			for index := range transport.lent {
				if !transport.lent[index] && transport.open[index].tag == 0 {
					slot = index
					break
				}
			}
		}
		if slot < 0 {
			resolver.SendFailed(outgoing.Transaction, now)
			continue
		}
		offset, _ := netapi.DatagramSlotOffset(uint8(slot))
		copy(transport.queries[offset:], query[:outgoing.Length])
		nextTag++
		if nextTag == 0 {
			nextTag++
		}
		timeout := min(resolver.Policy().QueryTimeout/1_000_000, netapi.MaxDatagramTimeoutMS)
		datagram = netapi.Datagram{Opcode: netapi.DatagramSend, Slot: uint8(slot), Address: outgoing.Server,
			Length: uint16(outgoing.Length), Port: netapi.DatagramPort, TimeoutMS: uint16(timeout), Tag: nextTag}
		if !netapi.EncodeDatagram(&datagram, &reply) || !sendOn(transport.channel, &reply) {
			resolver.SendFailed(outgoing.Transaction, now)
			closeTransport()
			continue
		}
		transport.lent[slot] = true
		transport.open[slot] = exchange{tag: nextTag, transaction: outgoing.Transaction}
	}
}

// applyServers gives the resolver the DHCP list, when a transport carries
// one, then the configured servers.
func applyServers() {
	var dhcp [netapi.MaxServers][4]byte
	count := 0
	if transport.channel != 0 {
		count = transport.serverCount
		dhcp = transport.servers
	}
	resolver.SetServers(dhcp[:count], configured[:configuredCount])
}

// --- sessions ---------------------------------------------------------------

func serviceSession(index int) {
	current := &sessions[index]
	for current.channel != 0 {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			resolver.Cancel(index)
			sys.HandleClose(current.channel)
			*current = session{}
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			continue
		}
		correlation, valid := resolvapi.DecodeRequest(&message, &request)
		if !valid {
			closeAttached()
			continue
		}
		handleRequest(index, correlation)
	}
}

func respond(current *session, correlation uint64) {
	if resolvapi.EncodeReply(&answer, correlation, &reply) {
		sendOn(current.channel, &reply)
	}
}

func handleRequest(index int, correlation uint64) {
	current := &sessions[index]
	now := clockNanos()
	answer = resolvapi.Reply{Opcode: request.Opcode}
	admin := current.rights == netapi.RightsAdminStatus
	switch request.Opcode {
	case resolvapi.OpResolve:
		if current.lookup != 0 {
			answer.Result = resolvapi.ResultBusy
			break
		}
		if request.Name.Size == 0 {
			answer.Result = resolvapi.ResultInvalidName
			break
		}
		if !request.Name.IsLocalhost() && fromHosts(&request.Name) {
			break
		}
		result, done := resolver.Lookup(index, &request.Name, now)
		if !done {
			current.lookup = correlation
			return
		}
		fill(&result)
	case resolvapi.OpStatus:
		status()
	case resolvapi.OpFlush:
		if !admin {
			answer.Result = resolvapi.ResultPermissionDenied
			break
		}
		resolver.Flush()
		syslog.Info("cache flushed")
	case resolvapi.OpReload:
		if !admin {
			answer.Result = resolvapi.ResultPermissionDenied
			break
		}
		if field := reload(); field != 0 {
			answer.Result, answer.Field = resolvapi.ResultConfigInvalid, field
			syslog.Warn("resolver settings refused", syslog.Str("key", resolvapi.Key(field)))
		} else {
			syslog.Info("resolver settings applied")
		}
	}
	respond(current, correlation)
}

// fromHosts answers from the hosts table. Without a configuration service
// the table is out of reach, and the lookup goes on to DNS.
func fromHosts(name *hostname.Name) bool {
	if configHandle == 0 {
		return false
	}
	hosts := config.HostsRequest{Operation: config.HostsGet, Name: name.Bytes()}
	encoded, ok := config.EncodeHostsRequest(hosts, 1)
	if !ok {
		return false
	}
	var link configLink
	if configClient.Exchange(link, &encoded, link.Now()+hostsDeadline, &configMessage) != config.OutcomeOK {
		return false
	}
	found, valid := config.DecodeHostsReply(&configMessage)
	if !valid || found.Operation != config.HostsGet || found.Status != config.StatusOK {
		return false
	}
	answer.Count, answer.Source = 1, resolvapi.SourceHosts
	answer.Addresses[0] = found.Address
	return true
}

func fill(result *dns.Answer) {
	answer.Result, answer.Source, answer.TTL = resolvapi.Result(result.Result), resolvapi.Source(result.Source), result.TTL
	if result.Result != dns.ResultOK {
		answer.Source, answer.TTL = 0, 0
		return
	}
	answer.Count = result.Count
	copy(answer.Addresses[:], result.Addresses[:result.Count])
}

// deliverAnswers sends every lookup the resolver finished to its session.
func deliverAnswers() {
	for {
		client, result, found := resolver.Completion()
		if !found {
			return
		}
		if client < 0 || client >= len(sessions) {
			continue
		}
		current := &sessions[client]
		if current.channel == 0 || current.lookup == 0 {
			continue
		}
		answer = resolvapi.Reply{Opcode: resolvapi.OpResolve}
		fill(&result)
		correlation := current.lookup
		current.lookup = 0
		respond(current, correlation)
	}
}

func status() {
	servers, count := resolver.Servers()
	answer.Count = uint8(min(count, resolvapi.MaxServers))
	for index := 0; index < int(answer.Count); index++ {
		answer.Servers[index] = resolvapi.Server{Address: servers[index].Address,
			Source: resolvapi.ServerSource(servers[index].Source)}
	}
	answer.Generation = transport.serverGeneration
	stats := resolver.Stats()
	answer.CacheUsed, answer.CacheLimit = stats.CacheUsed, stats.CacheLimit
	answer.Positive, answer.Negative, answer.Failures = stats.Positive, stats.Negative, stats.Failures
	answer.PositiveHits, answer.NegativeHits, answer.FailureHits = stats.PositiveHits, stats.NegativeHits, stats.FailureHits
	answer.Misses, answer.Queries, answer.Timeouts = stats.Misses, stats.Queries, stats.Timeouts
	answer.Malformed, answer.Mismatched = stats.Malformed, stats.Mismatched
	answer.Truncated, answer.ServerFailures = stats.Truncated, stats.ServerFailures
}

// --- configuration ----------------------------------------------------------

// reload reads system.dns.* as one candidate and applies it whole, or
// returns the first bad key's number and changes nothing. Without a
// configuration service it changes nothing and reports success, as the
// defaults are then the settings.
func reload() uint8 {
	if configHandle == 0 {
		return 0
	}
	var link configLink
	deadline := link.Now() + configBudget
	for index := range settings {
		field := uint8(index + 1)
		keyDeadline := min(deadline, link.Now()+configDeadline)
		value, status, outcome := configClient.ExchangeGet(link, sys.Bytes(resolvapi.Key(field)), keyDeadline, &configMessage)
		if outcome != config.OutcomeOK || status != config.StatusOK {
			syslog.Warn("resolver settings unavailable")
			return 0
		}
		want := config.KindUint
		if index < 4 {
			want = config.KindText
		}
		if value.Kind != want {
			return field
		}
		settings[index] = value
	}
	var texts [4][]byte
	for index := range texts {
		texts[index] = settings[index].Bytes()
	}
	servers, bad := dns.ParseServers(&texts)
	if bad >= 0 {
		return uint8(bad + 1)
	}
	var values [dns.PolicyFields]uint64
	for index := range values {
		values[index] = settings[4+index].Uint
	}
	policy, bad := dns.ParsePolicy(&values)
	if bad >= 0 {
		return uint8(5 + bad)
	}
	configuredCount = 0
	for _, server := range servers {
		if server != ([4]byte{}) {
			configured[configuredCount] = server
			configuredCount++
		}
	}
	resolver.SetPolicy(policy, clockNanos())
	applyServers()
	return 0
}

// configLink is the configuration session as a config.Transport.
type configLink struct{}

func (configLink) Send(value *ipc.Message) bool {
	size, ok := ipc.EncodeFrom(value, configWire[:])
	return ok && sys.IPCSend(configHandle, configWire[:size]) == kcall.StatusOK
}

func (configLink) Receive(ticks uint64, out *ipc.Message) kcall.Status {
	if status := sys.IPCWaitReceive(configHandle, configWire[:], ticks); status != kcall.StatusOK {
		return status
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	if !ipc.DecodeInto(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8], out) {
		return kcall.StatusCorruptData
	}
	return kcall.StatusOK
}

func (configLink) Now() uint64 {
	ticks, status := sys.ClockRead()
	if status != kcall.StatusOK {
		fatal("RESOLVED_CLOCK_FAILED\n")
	}
	return ticks
}

// --- reporting ---------------------------------------------------------------

func clockNanos() uint64 {
	value, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		fatal("RESOLVED_CLOCK_FAILED\n")
	}
	return value
}

func mark(text string) {
	if validation.Enabled {
		sys.ConsoleWriteString(consoleHandle, text)
	}
}

func fatal(text string) {
	if !syslog.Error(text[:len(text)-1]) {
		sys.ConsoleWriteString(consoleHandle, text)
	}
	sys.Exit(exitFailed)
}

func main() {}
