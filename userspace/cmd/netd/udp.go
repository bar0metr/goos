//go:build goos_userspace

package main

// UDP sessions (GUDP1, ADR 0195). PID 1 hands this service one channel and
// two pools per session: the TX pool the client writes and this service only
// reads, and the RX pool this service writes and the client only reads. The
// session's index is the endpoints' owner in the stack, so a session reaches
// only its own endpoints. A malformed message or a slot handed back that the
// client does not hold ends the session, and every endpoint with it. A
// GUDP 3 session (ADR 0216) moves its datagrams through rings at the end of
// each pool, served by udpserve.Session3.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
)

// udpSession is one session's handles and mappings; udpserve holds the
// protocol state.
type udpSession struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
	state    udpserve.Session
	// three marks a GUDP 3 session, which ring serves.
	three bool
	ring  udpserve.Session3
	// Replies a full client queue held back, oldest first, as for TCP, and a
	// GUDP 3 doorbell it held.
	held         [udpHeldReplies]heldUDPReply
	heldFirst    int
	heldCount    int
	doorbellHeld bool
}

const udpHeldReplies = 128

type heldUDPReply struct {
	reply       udpapi.Reply
	correlation uint64
}

// udpReplier sends replies on one session's channel, or holds them while
// its queue is full.
type udpReplier struct{ index int }

func (r udpReplier) Reply(out *udpapi.Reply, correlation uint64) bool {
	current := &udpSessions[r.index]
	if current.heldCount == 0 {
		switch sendUDPReply(current, out, correlation) {
		case kcall.StatusOK:
			return true
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		}
	}
	if current.heldCount == len(current.held) {
		return false
	}
	current.held[(current.heldFirst+current.heldCount)%len(current.held)] = heldUDPReply{reply: *out, correlation: correlation}
	current.heldCount++
	return true
}

// Doorbell sends a GUDP 3 session's OpDoorbell. A full queue holds it, as
// it holds a reply; the client sleeps until it arrives.
func (r udpReplier) Doorbell() bool {
	current := &udpSessions[r.index]
	if current.doorbellHeld {
		return true
	}
	switch sendUDPDoorbell(current) {
	case kcall.StatusOK:
	case kcall.StatusPeerDied, kcall.StatusCorruptData:
		return false
	default:
		current.doorbellHeld = true
	}
	return true
}

func sendUDPDoorbell(current *udpSession) kcall.Status {
	udpapi.EncodeServiceDoorbell(&reply)
	size, ok := ipc.EncodeFrom(&reply, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	return sys.IPCSend(current.channel, wire[:size])
}

func sendUDPReply(current *udpSession, out *udpapi.Reply, correlation uint64) kcall.Status {
	if !udpapi.EncodeReply(out, correlation, &reply) {
		return kcall.StatusCorruptData
	}
	size, ok := ipc.EncodeFrom(&reply, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	return sys.IPCSend(current.channel, wire[:size])
}

func flushUDPReplies(current *udpSession) bool {
	if current.doorbellHeld {
		switch sendUDPDoorbell(current) {
		case kcall.StatusOK:
			current.doorbellHeld = false
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		default:
			return true
		}
	}
	for current.heldCount != 0 {
		held := &current.held[current.heldFirst]
		switch sendUDPReply(current, &held.reply, held.correlation) {
		case kcall.StatusOK:
		case kcall.StatusPeerDied, kcall.StatusCorruptData:
			return false
		default:
			return true
		}
		current.heldFirst = (current.heldFirst + 1) % len(current.held)
		current.heldCount--
	}
	return true
}

var (
	udpSessions [network.MaxUDPSessions]udpSession
	udpIn       udpapi.Request
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
	three := control.UDPVersion == udpapi.Version3
	txPool, rxPool := uint64(udpapi.TXSlots*udpapi.SlotSize), uint64(udpapi.RXSlots*udpapi.SlotSize)
	if three {
		txPool, rxPool = udpapi.TXPoolPages3*4096, udpapi.RXPoolPages3*4096
	}
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
	current := &udpSessions[index]
	current.channel, current.txHandle, current.rxHandle = channel, tx, rx
	current.txBase, current.txSize, current.rxBase, current.rxSize = txBase, txSize, rxBase, rxSize
	current.three, current.doorbellHeld, current.heldCount, current.heldFirst = three, false, 0, 0
	// The control codec admits quotas 1 to netapi.MaxSessionQuota only.
	if three {
		if !current.ring.Start(&stack, index, int(control.Quota), sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)) {
			closeUDPSession(index, "quota invalid")
			return netapi.ControlInvalidArgument
		}
		// netd armed before it first slept.
		current.ring.Idle()
		return netapi.ControlOK
	}
	if !current.state.StartQuota(&stack, index, int(control.Quota), sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)) {
		closeUDPSession(index, "quota invalid")
		return netapi.ControlInvalidArgument
	}
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
	if current.three {
		current.ring.End(&stack)
	} else {
		current.state.End(&stack)
	}
	current.three, current.doorbellHeld, current.heldCount = false, false, 0
	for _, id := range [...]handle.ID{current.channel, current.txHandle, current.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(current.txBase, current.txSize)
	sys.VMUnmap(current.rxBase, current.rxSize)
	current.channel, current.txHandle, current.rxHandle = 0, 0, 0
	current.txBase, current.txSize, current.rxBase, current.rxSize = 0, 0, 0, 0
}

func serviceUDP(index int) {
	current := &udpSessions[index]
	if !flushUDPReplies(current) {
		closeUDPSession(index, "")
		return
	}
	for budget := sessionBudget; current.channel != 0 && budget > 0; budget-- {
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
		if current.three && udpapi.IsDoorbell(&message) {
			udpDoorbells[0]++
			if fault, _ := current.ring.Deliver(&stack, servedAt, udpReplier{index}); fault != udpserve.FaultNone {
				closeUDPSession(index, string(fault))
				return
			}
			transmitPending()
			continue
		}
		correlation, valid := udpapi.DecodeRequest(&message, &udpIn)
		if !valid {
			closeAttached()
			closeUDPSession(index, "request invalid")
			return
		}
		var fault udpserve.Fault
		if current.three {
			fault = current.ring.Handle(&stack, &udpIn, correlation, servedAt, udpReplier{index})
		} else {
			fault = current.state.Handle(&stack, &udpIn, correlation, servedAt, udpReplier{index})
		}
		if fault != udpserve.FaultNone {
			closeUDPSession(index, string(fault))
			return
		}
		transmitPending()
	}
}

// deliverUDP runs each session's Deliver once. It reports whether a
// session spent its budget with work left, so the loop must not sleep.
func deliverUDP() bool {
	more := false
	for index := range udpSessions {
		if udpSessions[index].channel == 0 {
			continue
		}
		var fault udpserve.Fault
		var left bool
		if udpSessions[index].three {
			fault, left = udpSessions[index].ring.Deliver(&stack, servedAt, udpReplier{index})
		} else {
			fault, left = udpSessions[index].state.Deliver(&stack, udpReplier{index})
		}
		if fault != udpserve.FaultNone {
			closeUDPSession(index, string(fault))
			continue
		}
		more = more || left
	}
	return more
}

// stalledUDP names a session, endpoint and parked request that Deliver
// would answer now, for the validation build's check before netd sleeps.
func stalledUDP() (session int, endpoint uint32, op string, found bool) {
	for index := range udpSessions {
		if udpSessions[index].channel == 0 {
			continue
		}
		runnable := udpSessions[index].state.Runnable
		if udpSessions[index].three {
			runnable = udpSessions[index].ring.Runnable
		}
		if endpoint, op, found := runnable(&stack); found {
			return index, endpoint, op, true
		}
	}
	return 0, 0, "", false
}

// udpDoorbells counts the GUDP 3 doorbells netd took.
var udpDoorbells [1]uint64

// armUDP arms every GUDP 3 session's doorbell before netd sleeps. It
// reports false when a client published ring entries meanwhile; then netd
// must run Deliver before it sleeps.
func armUDP() bool {
	idle := true
	for index := range udpSessions {
		current := &udpSessions[index]
		if current.channel != 0 && current.three && !current.ring.Idle() {
			idle = false
		}
	}
	return idle
}
