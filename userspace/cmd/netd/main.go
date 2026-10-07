//go:build goos_userspace

// Command netd is the network service (ADR 0191). It runs the host stack of
// userspace/network over the frames its driver hands it, answers the private
// sessions PID 1 opens for commands and the UDP sessions it opens for
// programs that use UDP (ADR 0195), and loads its settings from the
// configuration service. It holds no device authority: frames arrive in a
// shared pool the driver fills and leave through one the driver reads.
package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	config "goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/netdev"
	"goos/userspace/network"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

const (
	// idleWait bounds a wait with no timer due, so a lost wake costs
	// seconds rather than the service.
	idleWait = 30 * clock.TicksPerSecond
	// configDeadline bounds one configuration exchange and configBudget the
	// whole read of a candidate.
	configDeadline = 2 * clock.TicksPerSecond
	configBudget   = 5 * clock.TicksPerSecond
	nanosPerTick   = 1_000_000_000 / clock.TicksPerSecond
	exitFailed     = 1
	udpOwner       = 1000
	tcpOwner       = 2000
)

// session is one command's channel and what PID 1 said it may do.
type session struct {
	channel handle.ID
	rights  netapi.RightsClass
	// echo is the correlation of the session's echo in flight, zero when
	// there is none, and sequence its sequence number.
	echo     uint64
	sequence uint16
	// servers is the correlation of a held Servers request, zero when there
	// is none, and known the server generation the client has.
	servers uint64
	known   uint64
}

// binding is the current driver: its channel, generation and pools. A new
// binding replaces the old one whole; messages from an older generation
// cannot reach the new channel.
type binding struct {
	channel    handle.ID
	generation uint64
	rxHandle   handle.ID
	txHandle   handle.ID
	rxBase     uint64
	rxSize     uint64
	txBase     uint64
	txSize     uint64
	rx, tx     []byte
	rxSlots    netdev.Pool
	txSlots    netdev.Pool
	ready      bool
}

var (
	consoleHandle, controlHandle, configHandle handle.ID

	stack    network.Stack
	device   binding
	sessions [network.MaxSessions]session
	// interestOwner maps a WaitAny interest to its object: -1 the driver,
	// -2 the control channel, udpOwner plus an index a UDP session,
	// otherwise a session index.
	interests     [kcall.WaitAnyMaxObjects]sys.Interest
	interestOwner [kcall.WaitAnyMaxObjects]int

	wire        [ipc.WireMaxSize]byte
	configWire  [ipc.WireMaxSize]byte
	message     ipc.Message
	reply       ipc.Message
	frame       netdev.Message
	request     netapi.Request
	answer      netapi.Reply
	control     netapi.ControlRequest
	controlBack netapi.ControlReply

	txCorrelation  uint64
	configClient   config.Client
	configMessage  ipc.Message
	driverFaults   uint64
	reportedState  network.State
	readyAnnounced bool
	stateAnnounced bool
	// fallback is set while the stack runs on built-in defaults because the
	// stored settings could not be read or applied at startup.
	fallback bool
)

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("NETD_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("NETD_STARTUP_INVALID\n")
	}
	consoleHandle, _ = data.Handle("console")
	syslog.Init(consoleHandle, "netd")
	if controlHandle, ok = data.Handle("control"); !ok {
		fatal("NETD_CONTROL_MISSING\n")
	}
	configHandle, _ = data.Handle("config")
	now := clockNanos()
	seed, status := sys.RandomUint64()
	if status != kcall.StatusOK {
		fatal("NETD_RANDOM_FAILED\n")
	}
	stack.Init(seed ^ now)
	// Defaults stay in force when the stored settings are missing or invalid.
	// The stored values are left alone: net status reports the fallback, and
	// a reload applies the settings once they are fixed.
	candidate, tuning, field, read := readConfig()
	if read && field == network.FieldNone {
		field = stack.Apply(candidate, tuning, now)
	}
	if !read {
		fallback = true
		syslog.Warn("network settings unavailable; using built-in defaults")
	} else if field != network.FieldNone {
		fallback = true
		syslog.Warn("network settings invalid; using built-in defaults", syslog.Str("key", netapi.Field(field).Key()))
	}
	mark("NETD_STARTED_OK\n")
	serve()
}

func serve() {
	start := arena.Current()
	for {
		arena.Reset(start)
		now := clockNanos()
		stack.Advance(now)
		deliverCompletions()
		deliverUDP()
		deliverTCP(now)
		deliverServers()
		transmitPending()
		announceState(now)
		count := buildInterests()
		timeout := uint64(idleWait)
		if deadline, pending := stack.Deadline(); pending {
			timeout = 1
			if deadline > now {
				// Round up: waking a tick early would only wait again.
				timeout = min((deadline-now+nanosPerTick-1)/nanosPerTick, uint64(idleWait))
				if timeout == 0 {
					timeout = 1
				}
			}
		}
		ready, status := sys.WaitAny(&interests, count, timeout)
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK {
			fatal("NETD_WAIT_FAILED\n")
		}
		for index := 0; index < count; index++ {
			if ready&(1<<index) == 0 {
				continue
			}
			switch owner := interestOwner[index]; owner {
			case -1:
				serviceDriver()
			case -2:
				serviceControl()
			default:
				if owner >= tcpOwner {
					serviceTCP(owner - tcpOwner)
				} else if owner >= udpOwner {
					serviceUDP(owner - udpOwner)
				} else {
					serviceSession(owner)
				}
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
	if device.channel != 0 {
		add(device.channel, -1)
	}
	add(controlHandle, -2)
	for index := range sessions {
		if sessions[index].channel != 0 {
			add(sessions[index].channel, index)
		}
	}
	for index := range udpSessions {
		if udpSessions[index].channel != 0 {
			add(udpSessions[index].channel, udpOwner+index)
		}
	}
	for index := range tcpSessions {
		if tcpSessions[index].channel != 0 {
			add(tcpSessions[index].channel, tcpOwner+index)
		}
	}
	return count
}

// receiveOn takes one message from channel into message. It reports NotFound
// when the queue is empty and PeerDied once the peer is gone.
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

// --- driver ---------------------------------------------------------------

func serviceDriver() {
	for device.channel != 0 {
		status := receiveOn(device.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			unbind("driver gone")
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			faultDriver(netdev.FaultProtocol, "driver message invalid")
			return
		}
		if _, valid := netdev.Decode(&message, &frame); !valid {
			closeAttached()
			faultDriver(netdev.FaultProtocol, "driver message invalid")
			return
		}
		if frame.Generation != device.generation {
			faultDriver(netdev.FaultGeneration, "driver generation mismatch")
			return
		}
		now := clockNanos()
		switch frame.Opcode {
		case netdev.OpReady:
			device.ready = true
			if frame.Link == netdev.LinkUp {
				stack.LinkUp(network.MAC(frame.MAC), frame.MTU, now)
			} else {
				stack.LinkDown(network.ResultDown, now)
			}
			syslog.Info("driver ready", syslog.Uint("generation", device.generation), syslog.Uint("link", uint64(frame.Link)))
		case netdev.OpRxReady:
			if !device.ready || !device.rxSlots.Take(frame.Slot) {
				faultDriver(netdev.FaultProtocol, "receive slot not the driver's")
				return
			}
			offset, _ := netdev.SlotOffset(frame.Slot)
			stack.Receive(device.rx[offset:offset+uint64(frame.Length)], now)
			release := netdev.Message{Opcode: netdev.OpRxRelease, Generation: device.generation, Slot: frame.Slot}
			if !netdev.Encode(&release, 0, &reply) || !sendOn(device.channel, &reply) || !device.rxSlots.Give(frame.Slot) {
				unbind("driver release failed")
				return
			}
		case netdev.OpTxDone:
			if !device.txSlots.Take(frame.Slot) || !device.txSlots.Release(frame.Slot) {
				faultDriver(netdev.FaultProtocol, "transmit completion for no request")
				return
			}
			if frame.Result != netdev.ResultOK {
				stack.CountDrop()
			}
		case netdev.OpFault:
			syslog.Warn("driver reported a fault", syslog.Uint("reason", uint64(frame.Reason)))
			unbind("driver fault")
			return
		default:
			faultDriver(netdev.FaultProtocol, "driver operation invalid")
			return
		}
	}
}

// transmitPending moves queued frames into free transmit slots. A frame waits
// in the stack's bounded queue while every slot is with the driver.
func transmitPending() {
	if device.channel == 0 || !device.ready {
		return
	}
	for stack.PendingFrames() != 0 {
		slot, free := device.txSlots.Acquire()
		if !free {
			return
		}
		offset, _ := netdev.SlotOffset(slot)
		length, taken := stack.NextFrame(device.tx[offset : offset+netdev.SlotSize])
		if !taken {
			device.txSlots.Release(slot)
			return
		}
		txCorrelation++
		if txCorrelation == 0 {
			txCorrelation++
		}
		frame = netdev.Message{Opcode: netdev.OpTx, Generation: device.generation, Slot: slot, Length: uint16(length)}
		if !netdev.Encode(&frame, txCorrelation, &reply) || !sendOn(device.channel, &reply) {
			// The slot stays ours; the frame is lost like any dropped one.
			device.txSlots.Release(slot)
			stack.CountDrop()
			return
		}
		device.txSlots.Give(slot)
	}
}

// faultDriver tells the driver why its binding ends and drops it. The driver
// exits and PID 1 starts another with a new binding.
func faultDriver(reason netdev.FaultReason, text string) {
	if device.channel != 0 {
		frame = netdev.Message{Opcode: netdev.OpFault, Generation: device.generation, Reason: reason}
		if netdev.Encode(&frame, 0, &reply) {
			sendOn(device.channel, &reply)
		}
	}
	unbind(text)
}

// unbind releases the current binding: its channel, its pool mappings and
// handles. Echoes in flight fail now rather than at their deadlines.
func unbind(reason string) {
	if device.channel == 0 && device.rxHandle == 0 {
		return
	}
	syslog.Warn("driver binding ended", syslog.Str("reason", reason), syslog.Uint("generation", device.generation))
	driverFaults++
	// A pool stays allocated while any handle or mapping names it, so a
	// release that fails is reported: it is memory PID 1 paid for.
	for _, id := range [...]handle.ID{device.channel, device.rxHandle, device.txHandle} {
		if id != 0 && sys.HandleClose(id) != kcall.StatusOK {
			syslog.Error("binding handle not released", syslog.Uint("handle", uint64(id)))
			mark("NETD_RELEASE_FAILED\n")
		}
	}
	for _, mapping := range [...][2]uint64{{device.rxBase, device.rxSize}, {device.txBase, device.txSize}} {
		if mapping[0] != 0 && sys.VMUnmap(mapping[0], mapping[1]) != kcall.StatusOK {
			syslog.Error("pool mapping not released", syslog.Uint("address", mapping[0]))
			mark("NETD_RELEASE_FAILED\n")
		}
	}
	generation := device.generation
	device = binding{generation: generation}
	stack.LinkDown(network.ResultDriverFault, clockNanos())
	mark("NETD_DRIVER_UNBOUND\n")
}

// --- control --------------------------------------------------------------

func serviceControl() {
	for {
		status := receiveOn(controlHandle)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			fatal("NETD_CONTROL_GONE\n")
		}
		if status != kcall.StatusOK {
			closeAttached()
			continue
		}
		// A configuration session renewed after the configuration service
		// restarted arrives as a notice, not a control request.
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
		case netapi.ControlSetDevice:
			answerControl(control.Opcode, setDevice(control.Generation), control.Generation, correlation)
		case netapi.ControlAddUDPSession:
			answerControl(control.Opcode, addUDPSession(), 0, correlation)
		case netapi.ControlAddTCPSession:
			answerControl(control.Opcode, addTCPSession(), 0, correlation)
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

// handleIs checks a received handle's type and that it carries exactly the
// rights the protocol says: a pool the service may write when it should only
// read would let it change frames the driver is reading.
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

// addSession takes a command's session. A resolve-status session is the
// resolver service's: it may read the status and the DNS servers.
func addSession(channel handle.ID, class netapi.RightsClass) netapi.ControlResult {
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) {
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

// setDevice installs a new binding. Its generation must be newer than any
// seen, so a delayed message from a replaced driver cannot be taken for the
// new one's.
func setDevice(generation uint64) netapi.ControlResult {
	channel, rx, tx := message.Handles[0], message.Handles[1], message.Handles[2]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, rx, tx} {
			sys.HandleClose(id)
		}
		return result
	}
	if generation <= device.generation {
		return refuse(netapi.ControlInvalidArgument)
	}
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) ||
		!handleIs(rx, handle.TypeMemory, capability.Read) || !handleIs(tx, handle.TypeMemory, capability.Read|capability.Write) {
		return refuse(netapi.ControlInvalidArgument)
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	if rxStatus != kcall.StatusOK || rxSize < netdev.PoolSize {
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	if txStatus != kcall.StatusOK || txSize < netdev.PoolSize {
		sys.VMUnmap(rxBase, rxSize)
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	unbind("replaced")
	device = binding{channel: channel, generation: generation, rxHandle: rx, txHandle: tx,
		rxBase: rxBase, rxSize: rxSize, txBase: txBase, txSize: txSize,
		rx: sys.Window(rxBase, netdev.PoolSize), tx: sys.Window(txBase, netdev.PoolSize)}
	device.rxSlots.Reset(false)
	device.txSlots.Reset(true)
	syslog.Info("driver bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// --- sessions -------------------------------------------------------------

func serviceSession(index int) {
	current := &sessions[index]
	for current.channel != 0 {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			// The command is gone; its echo, if any, is forgotten without an
			// answer, and no other session notices.
			stack.CancelSession(index)
			sys.HandleClose(current.channel)
			*current = session{}
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			continue
		}
		correlation, valid := netapi.DecodeRequest(&message, &request)
		if !valid {
			opcode, attributed, known := netapi.RequestOpcode(&message)
			closeAttached()
			if known {
				respond(current, opcode, netapi.ResultInvalidArgument, attributed)
			}
			continue
		}
		handleRequest(index, correlation)
	}
}

func respond(current *session, opcode netapi.Opcode, result netapi.Result, correlation uint64) {
	answer = netapi.Reply{Opcode: opcode, Result: result}
	if opcode == netapi.OpEcho {
		answer.Sequence = request.Sequence
	}
	send(current, correlation)
}

func send(current *session, correlation uint64) {
	if netapi.EncodeReply(&answer, correlation, &reply) {
		sendOn(current.channel, &reply)
	}
}

func handleRequest(index int, correlation uint64) {
	current := &sessions[index]
	now := clockNanos()
	switch request.Opcode {
	case netapi.OpStatus:
		snapshot := stack.Snapshot(now)
		answer = netapi.Reply{Opcode: netapi.OpStatus, Result: netapi.ResultOK, State: netapi.State(snapshot.State),
			Mode: netapi.Mode(snapshot.Mode), Local: snapshot.Address, Gateway: snapshot.Gateway, MAC: snapshot.MAC,
			Prefix: snapshot.Prefix, Lease: snapshot.LeaseRemaining,
			RX: snapshot.RX, TX: snapshot.TX, Drop: snapshot.Drop, Malformed: snapshot.Malformed,
			MTU: snapshot.MTU, Fallback: fallback,
			RateLimited: snapshot.RateLimited, ConflictRun: snapshot.ConflictRun,
			Probes: snapshot.Conflicts.Probes, ProbeConflicts: snapshot.Conflicts.ProbeConflicts,
			Announcements: snapshot.Conflicts.Announcements, ActiveConflicts: snapshot.Conflicts.ActiveConflicts,
			Defenses: snapshot.Conflicts.Defenses, Relinquished: snapshot.Conflicts.Relinquished,
			RateLimits: snapshot.Conflicts.RateLimits, Loopback: snapshot.Loopback,
			UDPRX: snapshot.UDP.RX, UDPTX: snapshot.UDP.TX, UDPDropNoEndpoint: snapshot.UDP.DropNoEndpoint,
			UDPDropQueueFull: snapshot.UDP.DropQueueFull, UDPDropChecksum: snapshot.UDP.DropChecksum,
			UDPDropTooLong: snapshot.UDP.DropTooLong, UDPBindFail: snapshot.UDP.BindFail,
			UDPEndpoints: uint16(snapshot.UDP.Endpoints)}
		limits := stack.Tuning().UDP
		answer.UDPEphemeralMin, answer.UDPEphemeralMax, answer.UDPMaxPayload = limits.EphemeralMin, limits.EphemeralMax, limits.MaxPayload
		answer.UDPReceiveQueue, answer.UDPMaxEndpoints, answer.UDPSessionEndpoints = limits.ReceiveQueue, limits.MaxEndpoints, limits.SessionEndpoints
		if snapshot.Link {
			answer.Link = netapi.LinkUp
		}
		send(current, correlation)
	case netapi.OpServers:
		if current.servers != 0 {
			respond(current, request.Opcode, netapi.ResultBusy, correlation)
			return
		}
		current.servers, current.known = correlation, request.Generation
		deliverServers()
	case netapi.OpTCPStatus:
		snapshot := stack.TCPStats()
		limits := stack.Tuning().TCP
		answer = netapi.Reply{Opcode: netapi.OpTCPStatus, Result: netapi.ResultOK,
			TCPRX: snapshot.RX, TCPTX: snapshot.TX, TCPRetransmits: snapshot.Retransmits,
			TCPRXReset: snapshot.RXReset, TCPTXReset: snapshot.TXReset,
			TCPDropChecksum: snapshot.DropChecksum, TCPDropNoTCB: snapshot.DropNoTCB,
			TCPDropWindow: snapshot.DropWindow, TCPDropMalformed: snapshot.DropMalformed,
			TCPEndpoints: uint16(snapshot.Endpoints), TCPListeners: uint16(snapshot.Listeners), TCPConnections: uint16(snapshot.Connections),
			TCPEphemeralMin: limits.EphemeralMin, TCPEphemeralMax: limits.EphemeralMax,
			TCPMaxEndpoints: limits.MaxEndpoints, TCPSessionEndpoints: limits.SessionEndpoints, TCPListenBacklog: limits.ListenBacklog,
			TCPSendBuffer: limits.SendBuffer, TCPReceiveBuffer: limits.ReceiveBuffer,
			TCPConnectTimeoutMS: uint32(limits.ConnectTimeout / 1_000_000), TCPRTOMinMS: uint32(limits.RTOMin / 1_000_000),
			TCPRTOMaxMS: uint32(limits.RTOMax / 1_000_000), TCPTimeWaitMS: uint32(limits.TimeWait / 1_000_000)}
		for index, count := range snapshot.States {
			answer.TCPStates[index] = uint16(count)
		}
		send(current, correlation)
	case netapi.OpEcho:
		if current.rights != netapi.RightsPingStatus {
			respond(current, request.Opcode, netapi.ResultPermissionDenied, correlation)
			return
		}
		if current.echo != 0 {
			respond(current, request.Opcode, netapi.ResultBusy, correlation)
			return
		}
		result := stack.Echo(index, network.IPv4(request.Destination), request.Sequence, uint64(request.TimeoutMS)*1_000_000, now)
		if result != network.ResultOK {
			respond(current, request.Opcode, netapi.Result(result), correlation)
			return
		}
		current.echo, current.sequence = correlation, request.Sequence
		transmitPending()
	case netapi.OpReload:
		if current.rights != netapi.RightsAdminStatus {
			respond(current, request.Opcode, netapi.ResultPermissionDenied, correlation)
			return
		}
		answer = netapi.Reply{Opcode: netapi.OpReload, Result: netapi.ResultOK}
		candidate, tuning, field, read := readConfig()
		if read && field == network.FieldNone {
			field = stack.Apply(candidate, tuning, now)
		}
		if !read {
			answer.Result, answer.Field = netapi.ResultConfigInvalid, netapi.FieldNone
		} else if field != network.FieldNone {
			answer.Result, answer.Field = netapi.ResultConfigInvalid, netapi.Field(field)
			syslog.Warn("network settings refused", syslog.Str("key", netapi.Field(field).Key()))
		} else {
			fallback = false
			syslog.Info("network settings applied")
		}
		send(current, correlation)
		transmitPending()
	}
}

// deliverServers answers every held Servers request whose client has a list
// of another generation than the stack's.
func deliverServers() {
	servers, count, generation := stack.DNSServers()
	for index := range sessions {
		current := &sessions[index]
		if current.channel == 0 || current.servers == 0 || current.known == generation {
			continue
		}
		answer = netapi.Reply{Opcode: netapi.OpServers, Result: netapi.ResultOK, Generation: generation}
		for server := 0; server < count && server < netapi.MaxServers; server++ {
			answer.Servers[server] = servers[server]
			answer.ServerCount++
		}
		correlation := current.servers
		current.servers = 0
		send(current, correlation)
	}
}

// deliverCompletions answers every echo the stack finished.
func deliverCompletions() {
	for {
		completion, found := stack.Completion()
		if !found {
			return
		}
		if completion.Session < 0 || completion.Session >= len(sessions) {
			continue
		}
		current := &sessions[completion.Session]
		if current.channel == 0 || current.echo == 0 {
			continue
		}
		answer = netapi.Reply{Opcode: netapi.OpEcho, Result: netapi.Result(completion.Result), Sequence: current.sequence}
		switch completion.Result {
		case network.ResultOK:
			answer.Responder, answer.RTT = completion.Responder, completion.RTT
		case network.ResultRemoteUnreachable, network.ResultTimeExceeded:
			answer.Responder = completion.Responder
		}
		correlation := current.echo
		current.echo = 0
		send(current, correlation)
	}
}

// --- configuration ----------------------------------------------------------

// settingCount is every network setting: the four address keys, then the
// tuning keys. Setting index i is reported as field i+1, whose Key names it.
const settingCount = int(network.FieldGateway) + network.TuningFields

// settings holds one candidate while it is read. It is static because each
// value carries a text buffer.
var (
	settings [settingCount]config.Value
	tuningIn [network.TuningFields]uint64
)

// readConfig reads every network setting and parses them as one candidate.
// It reports false when the configuration service could not be asked. The
// whole read shares one deadline in ticks, configBudget from the start, and
// each key waits at most configDeadline of it. A key that would start at or
// after the shared deadline is not asked for, and a late reply to a key that
// timed out is dropped by the next exchange instead of being taken for its
// answer.
func readConfig() (network.Config, network.Tuning, network.Field, bool) {
	if configHandle == 0 {
		return network.Config{}, network.Tuning{}, network.FieldNone, false
	}
	var link configLink
	deadline := link.Now() + configBudget
	for index := range settings {
		field := netapi.Field(index + 1)
		keyDeadline := min(deadline, link.Now()+configDeadline)
		value, status, outcome := configClient.ExchangeGet(link, sys.Bytes(field.Key()), keyDeadline, &configMessage)
		if outcome != config.OutcomeOK || status != config.StatusOK {
			return network.Config{}, network.Tuning{}, network.FieldNone, false
		}
		settings[index] = value
		want := config.KindUint
		if field == netapi.FieldMode || field == netapi.FieldAddress || field == netapi.FieldGateway {
			want = config.KindText
		}
		if settings[index].Kind != want {
			return network.Config{}, network.Tuning{}, network.Field(field), true
		}
	}
	candidate, field := network.ParseConfig(settings[network.FieldMode-1].Bytes(), settings[network.FieldAddress-1].Bytes(),
		settings[network.FieldPrefix-1].Uint, settings[network.FieldGateway-1].Bytes())
	if field != network.FieldNone {
		return network.Config{}, network.Tuning{}, field, true
	}
	for index := range tuningIn {
		tuningIn[index] = settings[int(network.FieldTTL)-1+index].Uint
	}
	tuning, field := network.ParseTuning(&tuningIn)
	return candidate, tuning, field, true
}

// configLink is the configuration session as a config.Transport, with the
// clock in ticks.
type configLink struct{}

func (configLink) Send(message *ipc.Message) bool {
	size, ok := ipc.EncodeFrom(message, configWire[:])
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
		fatal("NETD_CLOCK_FAILED\n")
	}
	return ticks
}

// --- reporting ---------------------------------------------------------------

// announceState logs each change of interface state, and the validation
// build marks the first time the interface is ready.
func announceState(now uint64) {
	snapshot := stack.Snapshot(now)
	if stateAnnounced && snapshot.State == reportedState {
		return
	}
	stateAnnounced, reportedState = true, snapshot.State
	syslog.Info("interface state", syslog.Str("state", netapi.State(snapshot.State).Text()))
	if snapshot.State == network.StateReady && !readyAnnounced {
		readyAnnounced = true
		var text [48]byte
		line := append(text[:0], "NETD_READY "...)
		line = network.AppendIPv4(line, snapshot.Address)
		line = append(line, '\n')
		if validation.Enabled {
			sys.ConsoleWrite(consoleHandle, line)
		}
	}
}

func clockNanos() uint64 {
	value, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		fatal("NETD_CLOCK_FAILED\n")
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
