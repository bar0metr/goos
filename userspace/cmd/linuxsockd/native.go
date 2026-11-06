//go:build goos_userspace

package main

// The native backend: the GTCP 4 and GUDP 2 sessions PID 1 hands over with
// ControlSetTCP and ControlSetUDP, behind linuxsock.Backend. The rings, the
// codec and the slot bookkeeping are linuxsock.RingLink for TCP and
// linuxsock.NativeLink for UDP; this file owns the handles, the mappings
// and the channels. A send that finds netd's queue full makes the event
// loop wait for the channel to turn writable, the one event that retries
// it when the requests ahead of it park in netd.

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/linuxsock"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

// tcpLayout is the GTCP version and pools of the TCP session.
var tcpLayout = tcpapi.Layout4

// nativeLink is one native session.
type nativeLink struct {
	proto      linuxsock.Proto
	generation uint64
	channel    handle.ID
	txHandle   handle.ID
	rxHandle   handle.ID
	txBase     uint64
	txSize     uint64
	rxBase     uint64
	rxSize     uint64
	// state is the UDP session; tcpRing holds the TCP one.
	state linuxsock.NativeLink
}

var (
	links [linuxsock.UDP + 1]nativeLink
	// tcpRing stays apart from links: the link literals there would copy
	// its table.
	tcpRing    linuxsock.RingLink
	nativeWire [ipc.WireMaxSize]byte
	nativeIn   ipc.Message
)

// channelOf sends on a link's channel.
type channelOf linuxsock.Proto

func (c channelOf) Send(message *ipc.Message) linuxsock.SendStatus {
	link := &links[c]
	if link.channel == 0 {
		return linuxsock.SendGone
	}
	size, ok := ipc.EncodeFrom(message, nativeWire[:])
	if !ok {
		return linuxsock.SendGone
	}
	switch sys.IPCSend(link.channel, nativeWire[:size]) {
	case kcall.StatusOK:
		return linuxsock.SendOK
	case kcall.StatusPeerDied:
		return linuxsock.SendGone
	}
	return linuxsock.SendFull
}

// backend is the linuxsock.Backend over links.
type backend struct{}

func (backend) Submit(proto linuxsock.Proto, tag uint64, request *linuxsock.NativeRequest) linuxsock.SubmitStatus {
	if links[proto].channel == 0 {
		return linuxsock.SubmitDown
	}
	if proto == linuxsock.TCP {
		return tcpRing.Submit(channelOf(proto), tag, request)
	}
	return links[proto].state.Submit(channelOf(proto), tag, request)
}

// wantsWritable reports whether the event loop must wait for the link's
// channel to turn writable.
func (link *nativeLink) wantsWritable() bool {
	if link.proto == linuxsock.TCP {
		return link.channel != 0 && tcpRing.WantsWritable()
	}
	return link.channel != 0 && link.state.WantsWritable()
}

// settleRing takes what the TCP session published, then arms its doorbell
// and checks the rings once more, until nothing moves: linuxsockd may sleep
// only then. It reports whether Release entries still wait for ring room,
// which no doorbell announces.
func settleRing() bool {
	link := &links[linuxsock.TCP]
	if link.channel == 0 {
		return false
	}
	for !tcpRing.Idle(channelOf(linuxsock.TCP), &core) {
	}
	if tcpRing.Broken() {
		syslog.Warn("native session broke the ring protocol", syslog.Uint("protocol", uint64(linuxsock.TCP)))
		dropLink(linuxsock.TCP)
		return false
	}
	return tcpRing.Backlogged()
}

// serviceLink retries what a full queue held back, then reads every reply
// waiting on a session and hands each to the core. A dead session takes
// its sockets down.
func serviceLink(proto linuxsock.Proto) {
	link := &links[proto]
	if proto == linuxsock.TCP {
		serviceRing()
		return
	}
	if link.channel != 0 && link.state.Writable(channelOf(proto), &core) == linuxsock.SendGone {
		syslog.Warn("native session ended", syslog.Uint("protocol", uint64(proto)))
		dropLink(proto)
		return
	}
	for link.channel != 0 {
		status := sys.IPCReceive(link.channel, nativeWire[:])
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("native session ended", syslog.Uint("protocol", uint64(proto)))
			dropLink(proto)
			return
		}
		payloadSize := int(nativeWire[24]) | int(nativeWire[25])<<8
		if status != kcall.StatusOK || !ipc.DecodeInto(nativeWire[:ipc.WireHeaderSize+payloadSize+int(nativeWire[26])*8], &nativeIn) {
			syslog.Warn("native session sent a malformed message", syslog.Uint("protocol", uint64(proto)))
			dropLink(proto)
			return
		}
		correlation, completion, ok := link.state.Decode(&nativeIn)
		if !ok {
			syslog.Warn("native session sent a malformed reply", syslog.Uint("protocol", uint64(proto)))
			dropLink(proto)
			return
		}
		core.Complete(proto, link.generation, correlation, completion)
		if link.channel != 0 && link.state.Done(channelOf(proto)) == linuxsock.SendGone {
			dropLink(proto)
			return
		}
	}
}

// serviceRing reads every message waiting on the TCP session: replies and
// doorbells. The link takes the rings after each.
func serviceRing() {
	link := &links[linuxsock.TCP]
	if link.channel != 0 && tcpRing.WantsWritable() {
		tcpRing.Writable(channelOf(linuxsock.TCP), &core)
	}
	for link.channel != 0 {
		status := sys.IPCReceive(link.channel, nativeWire[:])
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("native session ended", syslog.Uint("protocol", uint64(linuxsock.TCP)))
			dropLink(linuxsock.TCP)
			return
		}
		payloadSize := int(nativeWire[24]) | int(nativeWire[25])<<8
		if status != kcall.StatusOK || payloadSize > ipc.MaxPayload ||
			!ipc.DecodeInto(nativeWire[:ipc.WireHeaderSize+payloadSize+int(nativeWire[26])*8], &nativeIn) || nativeIn.HandleCount != 0 ||
			!tcpRing.Receive(channelOf(linuxsock.TCP), &nativeIn, &core) {
			syslog.Warn("native session sent a malformed message", syslog.Uint("protocol", uint64(linuxsock.TCP)))
			dropLink(linuxsock.TCP)
			return
		}
	}
}

// setLink installs a native session from a control request's handles: the
// channel, the TX pool the service writes and the RX pool it reads.
func setLink(proto linuxsock.Proto, generation uint64, channel, tx, rx handle.ID) bool {
	const rw = capability.Read | capability.Write | capability.Wait
	txPool, rxPool := tcpLayout.TXPool(), tcpLayout.RXPool()
	if proto == linuxsock.UDP {
		txPool, rxPool = udpapi.TXSlots*udpapi.SlotSize, udpapi.RXSlots*udpapi.SlotSize
	}
	if !handleIs(channel, handle.TypeChannel, rw) || !handleIs(tx, handle.TypeMemory, capability.Read|capability.Write) ||
		!handleIs(rx, handle.TypeMemory, capability.Read) {
		return false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(tx, true)
	if txStatus != kcall.StatusOK || txSize < txPool {
		if txStatus == kcall.StatusOK {
			sys.VMUnmap(txBase, txSize)
		}
		return false
	}
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rx, false)
	if rxStatus != kcall.StatusOK || rxSize < rxPool {
		sys.VMUnmap(txBase, txSize)
		if rxStatus == kcall.StatusOK {
			sys.VMUnmap(rxBase, rxSize)
		}
		return false
	}
	dropLink(proto)
	link := &links[proto]
	*link = nativeLink{proto: proto, generation: generation, channel: channel, txHandle: tx, rxHandle: rx,
		txBase: txBase, txSize: txSize, rxBase: rxBase, rxSize: rxSize}
	if proto == linuxsock.TCP {
		if !tcpRing.Reset(generation, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool)) {
			*link = nativeLink{}
			sys.VMUnmap(txBase, txSize)
			sys.VMUnmap(rxBase, rxSize)
			return false
		}
		return true
	}
	link.state.Reset(proto, generation, tcpLayout, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool))
	return true
}

// dropLink ends a native session: the core fails every socket on it first,
// then the handles and mappings go.
func dropLink(proto linuxsock.Proto) {
	link := &links[proto]
	if link.channel == 0 {
		return
	}
	generation := link.generation
	for _, id := range [...]handle.ID{link.channel, link.txHandle, link.rxHandle} {
		sys.HandleClose(id)
	}
	sys.VMUnmap(link.txBase, link.txSize)
	sys.VMUnmap(link.rxBase, link.rxSize)
	*link = nativeLink{}
	core.BackendDown(proto, generation)
}
