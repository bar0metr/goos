//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

type tcpSession struct {
	channel  handle.ID
	txHandle handle.ID
	rxHandle handle.ID
	txBase   uint64
	txSize   uint64
	rxBase   uint64
	rxSize   uint64
	state    tcpserve.Session
}

type tcpReplier struct{ index int }

func (r tcpReplier) Reply(out *tcpapi.Reply, correlation uint64) bool {
	return tcpapi.EncodeReply(out, correlation, &reply) && sendOn(tcpSessions[r.index].channel, &reply)
}

var (
	tcpSessions [network.MaxTCPSessions]tcpSession
	tcpIn       tcpapi.Request
)

func addTCPSession() netapi.ControlResult {
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
	for candidate := range tcpSessions {
		if tcpSessions[candidate].channel == 0 {
			index = candidate
			break
		}
	}
	if index < 0 {
		return refuse(netapi.ControlBusy)
	}
	const txPool, rxPool = tcpapi.TXSlots * tcpapi.SlotSize, tcpapi.RXSlots * tcpapi.SlotSize
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
	current := &tcpSessions[index]
	current.channel, current.txHandle, current.rxHandle = channel, tx, rx
	current.txBase, current.txSize, current.rxBase, current.rxSize = txBase, txSize, rxBase, rxSize
	current.state.Start(&stack, index, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool), clockNanos())
	return netapi.ControlOK
}

func closeTCPSession(index int, reason string) {
	current := &tcpSessions[index]
	if current.channel == 0 {
		return
	}
	if reason != "" {
		syslog.Warn("TCP session closed", syslog.Uint("session", uint64(index)), syslog.Str("reason", reason))
	}
	current.state.End(&stack, clockNanos())
	for _, id := range [...]handle.ID{current.channel, current.txHandle, current.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(current.txBase, current.txSize)
	sys.VMUnmap(current.rxBase, current.rxSize)
	*current = tcpSession{}
}

func serviceTCP(index int) {
	current := &tcpSessions[index]
	for current.channel != 0 {
		status := receiveOn(current.channel)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			closeTCPSession(index, "")
			return
		}
		if status != kcall.StatusOK {
			closeAttached()
			closeTCPSession(index, "message invalid")
			return
		}
		correlation, valid := tcpapi.DecodeRequest(&message, &tcpIn)
		if !valid {
			closeAttached()
			closeTCPSession(index, "request invalid")
			return
		}
		if fault := current.state.Handle(&stack, &tcpIn, correlation, clockNanos(), tcpReplier{index}); fault != tcpserve.FaultNone {
			closeTCPSession(index, string(fault))
			return
		}
		transmitPending()
	}
}

func deliverTCP(now uint64) {
	for index := range tcpSessions {
		if tcpSessions[index].channel == 0 {
			continue
		}
		if fault := tcpSessions[index].state.Deliver(&stack, now, tcpReplier{index}); fault != tcpserve.FaultNone {
			closeTCPSession(index, string(fault))
		}
	}
}
