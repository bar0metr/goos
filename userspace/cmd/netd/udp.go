//go:build goos_userspace

package main

// UDP sessions (GUDP1, ADR 0195). PID 1 hands this service one channel and
// two pools per session: the TX pool the client writes and this service only
// reads, and the RX pool this service writes and the client only reads. The
// session's index is the endpoints' owner in the stack, so a session reaches
// only its own endpoints. A malformed message or a slot handed back that the
// client does not hold ends the session, and every endpoint with it.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
)

// udpWaiter is one of a session's endpoints and the correlation of the
// RecvFrom waiting on it, zero when none waits.
type udpWaiter struct {
	id   network.EndpointID
	recv uint64
}

type udpSession struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
	tx, rx   []byte
	// rxSlots tracks the RX slots the client holds.
	rxSlots   udpapi.Slots
	endpoints [network.MaxUDPSessionEndpoints]udpWaiter
}

var (
	udpSessions [network.MaxUDPSessions]udpSession
	udpIn       udpapi.Request
	udpOut      udpapi.Reply
)

// addUDPSession takes a session's channel and pools from PID 1.
func addUDPSession() netapi.ControlResult {
	channel, tx, rx := message.Handles[0], message.Handles[1], message.Handles[2]
	refuse := func(result netapi.ControlResult) netapi.ControlResult {
		for _, id := range [...]handle.ID{channel, tx, rx} {
			sys.HandleClose(id)
		}
		return result
	}
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) ||
		!handleIs(tx, handle.TypeMemory, capability.Read) || !handleIs(rx, handle.TypeMemory, capability.Read|capability.Write) {
		return refuse(netapi.ControlInvalidArgument)
	}
	index := -1
	for candidate := range udpSessions {
		if udpSessions[candidate].channel == 0 {
			index = candidate
			break
		}
	}
	if index < 0 {
		return refuse(netapi.ControlBusy)
	}
	const txPool, rxPool = udpapi.TXSlots * udpapi.SlotSize, udpapi.RXSlots * udpapi.SlotSize
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, false)
	if txStatus != kcall.StatusOK || txSize < txPool {
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, true)
	if rxStatus != kcall.StatusOK || rxSize < rxPool {
		sys.VMUnmap(txBase, txSize)
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return refuse(netapi.ControlInvalidArgument)
	}
	stack.UDPCloseOwner(index)
	current := &udpSessions[index]
	*current = udpSession{channel: channel, txHandle: tx, rxHandle: rx, txBase: txBase, txSize: txSize,
		rxBase: rxBase, rxSize: rxSize, tx: sys.Window(txBase, txPool), rx: sys.Window(rxBase, rxPool)}
	current.rxSlots.Reset(udpapi.RXSlots)
	return netapi.ControlOK
}

// closeUDPSession ends a session: its endpoints close, waiting receives are
// dropped with it, and its handles and mappings go.
func closeUDPSession(index int, reason string) {
	current := &udpSessions[index]
	if current.channel == 0 {
		return
	}
	if reason != "" {
		syslog.Warn("UDP session closed", syslog.Uint("session", uint64(index)), syslog.Str("reason", reason))
	}
	stack.UDPCloseOwner(index)
	for _, id := range [...]handle.ID{current.channel, current.txHandle, current.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(current.txBase, current.txSize)
	sys.VMUnmap(current.rxBase, current.rxSize)
	*current = udpSession{}
}

func serviceUDP(index int) {
	current := &udpSessions[index]
	for current.channel != 0 {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			closeUDPSession(index, "")
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			closeUDPSession(index, "message invalid")
			return
		}
		correlation, valid := udpapi.DecodeRequest(&message, &udpIn)
		if !valid {
			closeAttached()
			closeUDPSession(index, "request invalid")
			return
		}
		if !handleUDP(index, correlation) {
			return
		}
	}
}

// handleUDP serves one request and reports whether the session is still
// open.
func handleUDP(index int, correlation uint64) bool {
	current := &udpSessions[index]
	now := clockNanos()
	id := network.EndpointID(udpIn.Endpoint)
	udpOut = udpapi.Reply{Opcode: udpIn.Opcode, Endpoint: udpIn.Endpoint}
	switch udpIn.Opcode {
	case udpapi.OpOpen:
		opened, result := stack.UDPOpen(index)
		waiter := current.free()
		if result == network.ResultOK && waiter == nil {
			stack.UDPClose(index, opened)
			result = network.ResultBusy
		}
		udpOut.Result = udpResult(result)
		if result == network.ResultOK {
			*waiter = udpWaiter{id: opened}
			udpOut.Endpoint = uint32(opened)
		}
	case udpapi.OpBind:
		address, port, result := stack.UDPBind(index, id, network.IPv4(udpIn.Address), udpIn.Port)
		udpOut.Result = udpResult(result)
		if result == network.ResultOK {
			udpOut.Address, udpOut.Port = address, port
		}
	case udpapi.OpConnect:
		local, port, result := stack.UDPConnect(index, id, network.IPv4(udpIn.Address), udpIn.Port, now)
		udpOut.Result = udpResult(result)
		if result == network.ResultOK {
			udpOut.Address, udpOut.Port, udpOut.Local, udpOut.LocalPort = udpIn.Address, udpIn.Port, local, port
		}
	case udpapi.OpSend, udpapi.OpSendTo:
		offset := udpapi.Offset(udpIn.Slot)
		payload := current.tx[offset : offset+uint64(udpIn.Length)]
		var result network.Result
		if udpIn.Opcode == udpapi.OpSend {
			result = stack.UDPSend(index, id, payload, now)
		} else {
			result = stack.UDPSendTo(index, id, network.IPv4(udpIn.Address), udpIn.Port, payload, now)
		}
		udpOut.Result, udpOut.Slot = udpResult(result), udpIn.Slot
	case udpapi.OpRecvFrom:
		_, result := stack.UDPQueued(index, id)
		waiter := current.find(id)
		switch {
		case result != network.ResultOK || waiter == nil:
			udpOut.Result = udpapi.ResultInvalidArgument
		case waiter.recv != 0:
			udpOut.Result = udpapi.ResultBusy
		default:
			waiter.recv = correlation
			return true
		}
	case udpapi.OpRelease:
		if !current.rxSlots.Return(udpIn.Slot) {
			closeUDPSession(index, "release of a slot the client does not hold")
			return false
		}
		return true
	case udpapi.OpClose:
		waiter := current.find(id)
		result := stack.UDPClose(index, id)
		if waiter != nil {
			if waiter.recv != 0 {
				pending := udpapi.Reply{Opcode: udpapi.OpRecvFrom, Result: udpapi.ResultClosed, Endpoint: udpIn.Endpoint}
				if !replyUDP(index, &pending, waiter.recv) {
					return false
				}
			}
			*waiter = udpWaiter{}
		}
		udpOut.Result = udpResult(result)
	}
	if !replyUDP(index, &udpOut, correlation) {
		return false
	}
	transmitPending()
	return true
}

// find returns the entry for id, or a free entry for id 0.
func (current *udpSession) find(id network.EndpointID) *udpWaiter {
	for index := range current.endpoints {
		if current.endpoints[index].id == id {
			return &current.endpoints[index]
		}
	}
	return nil
}

func (current *udpSession) free() *udpWaiter {
	return current.find(0)
}

func replyUDP(index int, out *udpapi.Reply, correlation uint64) bool {
	if !udpapi.EncodeReply(out, correlation, &reply) || !sendOn(udpSessions[index].channel, &reply) {
		closeUDPSession(index, "client unreachable")
		return false
	}
	return true
}

// deliverUDP answers every waiting RecvFrom whose endpoint has a datagram,
// while the session has an RX slot to put it in.
func deliverUDP() {
	for index := range udpSessions {
		current := &udpSessions[index]
		for waiterIndex := range current.endpoints {
			if current.channel == 0 {
				break
			}
			waiter := &current.endpoints[waiterIndex]
			if waiter.recv == 0 {
				continue
			}
			slot, free := current.rxSlots.Free()
			if !free {
				break
			}
			offset := udpapi.Offset(slot)
			received, found := stack.UDPReceive(index, waiter.id, current.rx[offset:offset+udpapi.SlotSize])
			if !found {
				continue
			}
			current.rxSlots.Lend(slot)
			udpOut = udpapi.Reply{Opcode: udpapi.OpRecvFrom, Endpoint: uint32(waiter.id), Slot: slot,
				Address: received.Source, Port: received.SourcePort, Length: uint16(received.Length),
				Local: received.Destination}
			correlation := waiter.recv
			waiter.recv = 0
			replyUDP(index, &udpOut, correlation)
		}
	}
}

func udpResult(result network.Result) udpapi.Result {
	switch result {
	case network.ResultOK:
		return udpapi.ResultOK
	case network.ResultBusy:
		return udpapi.ResultBusy
	case network.ResultDown:
		return udpapi.ResultDown
	case network.ResultNoAddress:
		return udpapi.ResultNoAddress
	case network.ResultNoRoute:
		return udpapi.ResultNoRoute
	case network.ResultNeighborUnreachable:
		return udpapi.ResultNeighborUnreachable
	case network.ResultReconfigured:
		return udpapi.ResultReconfigured
	case network.ResultAddressInUse:
		return udpapi.ResultAddressInUse
	case network.ResultDriverFault:
		return udpapi.ResultDriverFault
	}
	return udpapi.ResultInvalidArgument
}
