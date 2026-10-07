//go:build goos_userspace

// Command resolved is the resolver service (ADR 0194): a stub resolver with a
// cache that turns host names into IPv4 addresses for commands. It answers
// localhost and names under it itself, asks the configuration service's hosts
// table next, and only then its cache and the recursive DNS servers named by
// the DHCP lease and by system.dns.server1..4. It reaches those servers over
// the same UDP sessions any program uses (ADR 0195), and learns the lease's
// servers over a network status session; it holds no device.
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

var (
	consoleHandle, controlHandle, configHandle handle.ID

	resolver dns.Resolver
	sessions [maxSessions]session
	// configured is the server list from system.dns.server1..4.
	configured      [4][4]byte
	configuredCount int

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
		releaseFinished()
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
				serviceUDP()
			case -2:
				serviceControl()
			case -3:
				serviceStatus()
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
	if link.channel != 0 {
		add(link.channel, -1)
		add(link.status, -3)
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
		case netapi.ControlSetNetwork:
			answerControl(control.Opcode, setNetwork(control.Generation), control.Generation, correlation)
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

// --- servers ----------------------------------------------------------------

// applyServers gives the resolver the DHCP list, while a network session
// reports one, then the configured servers.
func applyServers() {
	var dhcp [netapi.MaxServers][4]byte
	count := 0
	if link.channel != 0 {
		count = link.serverCount
		dhcp = link.servers
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
	answer.Generation = link.serverGeneration
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
