//go:build goos_userspace

package main

// The resolver's way to its DNS servers (ADR 0195). PID 1 gives this service
// a UDP session and a network status session with ControlSetNetwork, the
// same sessions any program gets, whenever this service or the network
// service starts.
//
// Each query gets an endpoint of its own: Open, then Connect to the server's
// port 53, which binds a random ephemeral port, then Send and RecvFrom. The
// connected endpoint keeps only datagrams from that address and port, and
// the resolver checks the transaction ID and question of what arrives. The
// endpoint closes as soon as its transaction stops waiting for that query,
// so a late answer has no endpoint left to reach. The resolver's own timers
// decide timeouts and retransmissions; each retransmission opens a new
// endpoint with a new port.
//
// The network session carries the DHCP lease's DNS servers: one Servers
// request waits in the network service until the list changes.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/dns"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
)

const dnsPort = 53

type upstreamPhase uint8

const (
	upstreamIdle upstreamPhase = iota
	// upstreamOpening waits for the endpoint of the query in query.
	upstreamOpening
	// upstreamActive has an endpoint that sent the query or is sending it.
	upstreamActive
)

// upstream is what one resolver transaction holds for its query in flight.
type upstream struct {
	phase    upstreamPhase
	open     uint64
	endpoint uint32
	server   [4]byte
	length   int
	query    [dns.DatagramSize]byte
}

type networkLink struct {
	generation uint64
	channel    handle.ID
	txHandle   handle.ID
	rxHandle   handle.ID
	txBase     uint64
	txSize     uint64
	rxBase     uint64
	rxSize     uint64
	tx, rx     []byte
	txSlots    udpapi.Slots
	// status is the network session; serversAsk is the correlation of the
	// Servers request waiting in it.
	status     handle.ID
	serversAsk uint64
	// servers is the DHCP server list last reported, under its generation.
	servers          [netapi.MaxServers][4]byte
	serverCount      int
	serverGeneration uint64
}

var (
	link        networkLink
	upstreams   [dns.MaxTransactions]upstream
	correlation uint64
	udpRequest  udpapi.Request
	udpReply    udpapi.Reply
	netRequest  netapi.Request
	netReply    netapi.Reply
)

func nextCorrelation() uint64 {
	correlation++
	if correlation == 0 {
		correlation++
	}
	return correlation
}

// setNetwork installs new sessions. Queries on the old ones can no longer
// finish, so every lookup waiting on them fails with Reconfigured; the cache
// stays.
func setNetwork(generation uint64) netapi.ControlResult {
	channel, tx, rx, status := message.Handles[0], message.Handles[1], message.Handles[2], message.Handles[3]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, tx, rx, status} {
			sys.HandleClose(id)
		}
		return result
	}
	const rw = capability.Read | capability.Write | capability.Wait
	if generation <= link.generation || !handleIs(channel, handle.TypeChannel, rw) || !handleIs(status, handle.TypeChannel, rw) ||
		!handleIs(tx, handle.TypeMemory, capability.Read|capability.Write) || !handleIs(rx, handle.TypeMemory, capability.Read) {
		return refuse(netapi.ControlInvalidArgument)
	}
	const txPool, rxPool = udpapi.TXSlots * udpapi.SlotSize, udpapi.RXSlots * udpapi.SlotSize
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	if txStatus != kcall.StatusOK || txSize < txPool {
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	if rxStatus != kcall.StatusOK || rxSize < rxPool {
		sys.VMUnmap(txBase, txSize)
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	closeNetwork()
	link = networkLink{generation: generation, channel: channel, txHandle: tx, rxHandle: rx, status: status,
		txBase: txBase, txSize: txSize, rxBase: rxBase, rxSize: rxSize,
		tx: sys.Window(txBase, txPool), rx: sys.Window(rxBase, rxPool)}
	link.txSlots.Reset(udpapi.TXSlots)
	resolver.Reset(clockNanos())
	applyServers()
	askServers()
	syslog.Info("network sessions bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// closeNetwork drops both sessions. The DHCP servers belonged to the lease
// the old network session reported, so they go too.
func closeNetwork() {
	if link.channel == 0 {
		return
	}
	for _, id := range [...]handle.ID{link.channel, link.txHandle, link.rxHandle, link.status} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(link.txBase, link.txSize)
	sys.VMUnmap(link.rxBase, link.rxSize)
	generation := link.generation
	link = networkLink{generation: generation}
	for index := range upstreams {
		upstreams[index].phase = upstreamIdle
	}
	resolver.Reset(clockNanos())
	applyServers()
}

// askServers leaves a Servers request waiting in the network service.
func askServers() {
	link.serversAsk = nextCorrelation()
	netRequest = netapi.Request{Opcode: netapi.OpServers, Generation: link.serverGeneration}
	if !netapi.EncodeRequest(&netRequest, link.serversAsk, &reply) || !sendOn(link.status, &reply) {
		syslog.Error("network session unreachable")
		closeNetwork()
	}
}

func serviceStatus() {
	for link.status != 0 {
		status := receiveOn(link.status)
		if status == kcall.StatusNotFound {
			return
		}
		if status != kcall.StatusOK || !netapi.DecodeReply(&message, link.serversAsk, netapi.OpServers, &netReply) ||
			netReply.Result != netapi.ResultOK {
			closeAttached()
			if status != kcall.StatusPeerDied {
				syslog.Error("network session reply invalid")
			}
			closeNetwork()
			return
		}
		link.serverGeneration, link.serverCount, link.servers = netReply.Generation, int(netReply.ServerCount), netReply.Servers
		applyServers()
		askServers()
	}
}

func serviceUDP() {
	for link.channel != 0 {
		status := receiveOn(link.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("network service gone; waiting for new sessions")
			closeNetwork()
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			syslog.Error("UDP reply invalid")
			closeNetwork()
			return
		}
		answered, valid := udpapi.DecodeReply(&message, &udpReply)
		if !valid || !udpAnswer(answered, clockNanos()) {
			syslog.Error("UDP reply invalid")
			closeNetwork()
			return
		}
	}
}

// udpAnswer handles one reply and reports false for a protocol violation.
func udpAnswer(answered uint64, now uint64) bool {
	if udpReply.Opcode == udpapi.OpOpen {
		opened(answered)
		return true
	}
	transaction := owning(udpReply.Endpoint)
	switch udpReply.Opcode {
	case udpapi.OpSend:
		if !link.txSlots.Return(udpReply.Slot) {
			return false
		}
		if udpReply.Result != udpapi.ResultOK && transaction >= 0 {
			fail(transaction, now)
		}
	case udpapi.OpConnect:
		if udpReply.Result != udpapi.ResultOK && transaction >= 0 {
			fail(transaction, now)
		}
	case udpapi.OpRecvFrom:
		if udpReply.Result != udpapi.ResultOK {
			return true
		}
		offset := udpapi.Offset(udpReply.Slot)
		if transaction >= 0 && udpReply.Port == dnsPort && udpReply.Address == upstreams[transaction].server {
			resolver.Receive(transaction, udpReply.Address, link.rx[offset:offset+uint64(udpReply.Length)], now)
		}
		udpRequest = udpapi.Request{Opcode: udpapi.OpRelease, Slot: udpReply.Slot}
		if !send(0) {
			return true
		}
		// A reply the resolver did not take, such as a mismatched ID, leaves
		// the query waiting for the real one.
		if transaction >= 0 && resolver.Waiting(transaction) {
			udpRequest = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: udpReply.Endpoint}
			send(nextCorrelation())
		}
	}
	return true
}

// opened takes a new endpoint. One whose transaction has moved on is closed
// at once.
func opened(answered uint64) {
	transaction := -1
	for index := range upstreams {
		if upstreams[index].phase == upstreamOpening && upstreams[index].open == answered {
			transaction = index
		}
	}
	if transaction < 0 {
		if udpReply.Result == udpapi.ResultOK {
			closeEndpoint(udpReply.Endpoint)
		}
		return
	}
	current := &upstreams[transaction]
	now := clockNanos()
	if udpReply.Result != udpapi.ResultOK {
		current.phase = upstreamIdle
		resolver.SendFailed(transaction, now)
		return
	}
	current.phase, current.endpoint = upstreamActive, udpReply.Endpoint
	slot, free := link.txSlots.Free()
	if !free {
		fail(transaction, now)
		return
	}
	copy(link.tx[udpapi.Offset(slot):], current.query[:current.length])
	link.txSlots.Lend(slot)
	udpRequest = udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: current.endpoint, Address: current.server, Port: dnsPort}
	if !send(nextCorrelation()) {
		return
	}
	udpRequest = udpapi.Request{Opcode: udpapi.OpSend, Endpoint: current.endpoint, Slot: slot, Length: uint16(current.length)}
	if !send(nextCorrelation()) {
		return
	}
	udpRequest = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: current.endpoint}
	send(nextCorrelation())
}

func owning(endpoint uint32) int {
	for index := range upstreams {
		if upstreams[index].phase == upstreamActive && upstreams[index].endpoint == endpoint {
			return index
		}
	}
	return -1
}

// fail ends a transaction's query as unsendable and closes its endpoint.
func fail(transaction int, now uint64) {
	release(transaction)
	resolver.SendFailed(transaction, now)
}

// release forgets a transaction's query, closing its endpoint if it has one.
// An endpoint still opening is closed when it arrives.
func release(transaction int) {
	current := &upstreams[transaction]
	if current.phase == upstreamActive {
		closeEndpoint(current.endpoint)
	}
	current.phase = upstreamIdle
}

func closeEndpoint(endpoint uint32) {
	udpRequest = udpapi.Request{Opcode: udpapi.OpClose, Endpoint: endpoint}
	send(nextCorrelation())
}

// send sends udpRequest and closes both sessions if it cannot.
func send(with uint64) bool {
	if !udpapi.EncodeRequest(&udpRequest, with, &reply) || !sendOn(link.channel, &reply) {
		syslog.Error("UDP session unreachable")
		closeNetwork()
		return false
	}
	return true
}

// sendQueries starts every query the resolver has ready. A query that
// cannot go, for want of a session, fails at once so the resolver tries its
// next server or gives up.
func sendQueries(now uint64) {
	for {
		outgoing, ready := resolver.NextQuery(query[:])
		if !ready {
			return
		}
		if link.channel == 0 {
			resolver.SendFailed(outgoing.Transaction, now)
			continue
		}
		release(outgoing.Transaction)
		current := &upstreams[outgoing.Transaction]
		current.server, current.length = outgoing.Server, outgoing.Length
		copy(current.query[:], query[:outgoing.Length])
		current.phase, current.open = upstreamOpening, nextCorrelation()
		udpRequest = udpapi.Request{Opcode: udpapi.OpOpen}
		send(current.open)
	}
}

// releaseFinished closes the endpoints of transactions that stopped waiting.
func releaseFinished() {
	if link.channel == 0 {
		return
	}
	for index := range upstreams {
		if upstreams[index].phase != upstreamIdle && !resolver.Waiting(index) {
			release(index)
		}
	}
}
