//go:build goos_userspace

package main

// The resolver transport (GNETDGRAM1, ADR 0194). PID 1 binds it to this
// service and to the resolver: one channel and two pools, the queries the
// resolver writes and this service only reads, and the replies this service
// writes and the resolver only reads. It carries DNS queries to port 53 and
// their replies, and the DNS servers of the current lease. A new binding
// replaces the old one whole, and exchanges still open on the old one are
// cancelled, so a late reply cannot reach the new resolver.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
)

type resolverTransport struct {
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
	// lent marks reply slots the resolver holds.
	lent [netapi.DatagramSlots]bool
	// tags names the resolver's exchange in each stack datagram slot; zero
	// is a free slot.
	tags [network.MaxDatagrams]uint64
	// serverGeneration counts server lists sent; stackServers is the stack's
	// generation of the list last sent.
	serverGeneration uint64
	stackServers     uint64
	serversSent      bool
}

var (
	datagramIn  netapi.Datagram
	datagramOut netapi.Datagram
)

// setTransport installs a new binding. Its generation must be newer than any
// seen, as for the device.
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
		!handleIs(queries, handle.TypeMemory, capability.Read) || !handleIs(replies, handle.TypeMemory, capability.Read|capability.Write) {
		return refuse(netapi.ControlInvalidArgument)
	}
	poolSize := uint64(netapi.DatagramSlots * netapi.DatagramSlotSize)
	queryBase, querySize, queryStatus := sys.MemoryMapAnywhere(queries, false)
	if queryStatus != kcall.StatusOK || querySize < poolSize {
		if queryStatus == kcall.StatusOK {
			sys.VMUnmap(queryBase, querySize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	replyBase, replySize, replyStatus := sys.MemoryMapAnywhere(replies, true)
	if replyStatus != kcall.StatusOK || replySize < poolSize {
		sys.VMUnmap(queryBase, querySize)
		if replyStatus == kcall.StatusOK {
			sys.VMUnmap(replyBase, replySize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	closeTransport("replaced")
	transport = resolverTransport{channel: channel, generation: generation, queryPool: queries, replyPool: replies,
		queryBase: queryBase, querySize: querySize, replyBase: replyBase, replySize: replySize,
		queries: sys.Window(queryBase, poolSize), replies: sys.Window(replyBase, poolSize),
		serverGeneration: transport.serverGeneration}
	syslog.Info("resolver transport bound", syslog.Uint("generation", generation))
	return netapi.ControlOK
}

// closeTransport releases the binding and cancels its open exchanges.
func closeTransport(reason string) {
	if transport.channel == 0 {
		return
	}
	syslog.Info("resolver transport closed", syslog.Str("reason", reason))
	for slot, tag := range transport.tags {
		if tag != 0 {
			stack.CancelDatagram(slot)
		}
	}
	for _, id := range [...]handle.ID{transport.channel, transport.queryPool, transport.replyPool} {
		if id != 0 {
			sys.HandleClose(id)
		}
	}
	for _, mapping := range [...][2]uint64{{transport.queryBase, transport.querySize}, {transport.replyBase, transport.replySize}} {
		if mapping[0] != 0 {
			sys.VMUnmap(mapping[0], mapping[1])
		}
	}
	generation, servers := transport.generation, transport.serverGeneration
	transport = resolverTransport{generation: generation, serverGeneration: servers}
}

func serviceTransport() {
	for transport.channel != 0 {
		status := receiveOn(transport.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			closeTransport("resolver gone")
			return
		}
		if status != kcall.StatusOK || !netapi.DecodeDatagram(&message, &datagramIn) {
			closeAttached()
			closeTransport("resolver message invalid")
			return
		}
		switch datagramIn.Opcode {
		case netapi.DatagramSend:
			sendQuery()
		case netapi.DatagramRelease:
			if !transport.lent[datagramIn.Slot] {
				closeTransport("release of a slot the resolver does not hold")
				return
			}
			transport.lent[datagramIn.Slot] = false
		default:
			closeTransport("resolver operation invalid")
			return
		}
	}
}

// sendQuery copies a query out of the resolver's slot, starts its exchange,
// and gives the slot straight back.
func sendQuery() {
	datagramOut = netapi.Datagram{Opcode: netapi.DatagramAccepted, Slot: datagramIn.Slot, Tag: datagramIn.Tag}
	slot := -1
	for index, tag := range transport.tags {
		if tag == 0 {
			slot = index
			break
		}
	}
	if slot < 0 {
		datagramOut.Result = netapi.ResultBusy
		sendDatagram()
		return
	}
	offset, _ := netapi.DatagramSlotOffset(datagramIn.Slot)
	payload := transport.queries[offset : offset+uint64(datagramIn.Length)]
	result := stack.SendDatagram(slot, network.IPv4(datagramIn.Address), datagramIn.Port, payload,
		uint64(datagramIn.TimeoutMS)*1_000_000, clockNanos())
	datagramOut.Result = netapi.Result(result)
	if result == network.ResultOK {
		transport.tags[slot] = datagramIn.Tag
	}
	sendDatagram()
}

func sendDatagram() {
	if !netapi.EncodeDatagram(&datagramOut, &reply) || !sendOn(transport.channel, &reply) {
		closeTransport("resolver unreachable")
	}
}

// deliverDatagrams hands every finished exchange to the resolver while it
// has a reply slot to put it in. A result for an exchange no binding owns is
// dropped.
func deliverDatagrams() {
	for transport.channel != 0 {
		free := -1
		for index, lent := range transport.lent {
			if !lent {
				free = index
				break
			}
		}
		if free < 0 {
			return
		}
		offset, _ := netapi.DatagramSlotOffset(uint8(free))
		done, found := stack.DatagramCompletion(transport.replies[offset : offset+netapi.DatagramSlotSize])
		if !found {
			return
		}
		tag := transport.tags[done.Slot]
		transport.tags[done.Slot] = 0
		if tag == 0 {
			continue
		}
		datagramOut = netapi.Datagram{Opcode: netapi.DatagramResult, Result: netapi.Result(done.Result), Tag: tag}
		if done.Result == network.ResultOK {
			datagramOut.Slot, datagramOut.Address, datagramOut.Port = uint8(free), done.Source, done.Port
			datagramOut.Length = uint16(done.Length)
			transport.lent[free] = true
		}
		sendDatagram()
	}
	// Without a resolver, finished exchanges are cancelled as they come.
	var scratch [network.DatagramPayload]byte
	for {
		if _, found := stack.DatagramCompletion(scratch[:]); !found {
			return
		}
	}
}

// announceServers tells the resolver the lease's DNS servers when the list
// changes, and once on every new binding.
func announceServers() {
	if transport.channel == 0 {
		return
	}
	servers, count, generation := stack.DNSServers()
	if transport.serversSent && generation == transport.stackServers {
		return
	}
	transport.serverGeneration++
	datagramOut = netapi.Datagram{Opcode: netapi.DatagramServers, Count: uint8(count), Generation: transport.serverGeneration}
	for index := 0; index < count && index < netapi.MaxServers; index++ {
		datagramOut.Servers[index] = servers[index]
	}
	transport.serversSent, transport.stackServers = true, generation
	sendDatagram()
}
