//go:build goos_userspace

package main

// The native backend: the GTCP 3 and GUDP 2 sessions PID 1 hands over with
// ControlSetTCP and ControlSetUDP, behind linuxsock.Backend. This file is
// the only one that knows the native wire. netd answers a NoWait request at
// once; it holds a WaitReady until the endpoint's mask changes and a
// blocking Read until bytes arrive. GUDP has no pending error and no buffer
// options: those requests answer SubmitUnsupported.

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
var tcpLayout = tcpapi.Layout3

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
	tx         []byte
	rx         []byte
	// txLent tracks the TX slots netd holds; releases are RX slots whose
	// Release found the queue full and is sent again.
	txLent   tcpapi.Slots
	releases uint8
}

var (
	links      [linuxsock.UDP + 1]nativeLink
	nativeWire [ipc.WireMaxSize]byte
	nativeOut  ipc.Message
	nativeIn   ipc.Message
	completion linuxsock.NativeCompletion
	tcpRequest tcpapi.Request
	tcpReply   tcpapi.Reply
	udpRequest udpapi.Request
	udpReply   udpapi.Reply
)

// backend is the linuxsock.Backend over links.
type backend struct{}

func (backend) Submit(proto linuxsock.Proto, tag uint64, request *linuxsock.NativeRequest) linuxsock.SubmitStatus {
	link := &links[proto]
	if link.channel == 0 {
		return linuxsock.SubmitDown
	}
	link.flushReleases()
	slot, lent := uint8(0), false
	data := request.Data
	if request.Op == linuxsock.NativeWrite || request.Op == linuxsock.NativeSendTo {
		size := uint64(udpapi.SlotSize)
		if proto == linuxsock.TCP {
			// A stream write sends what fits; the reply's Length says so.
			size = uint64(tcpLayout.SlotSize)
			data = data[:min(uint64(len(data)), size)]
		} else if uint64(len(data)) > size {
			return linuxsock.SubmitUnsupported
		}
		free, ok := link.txLent.Free()
		if !ok {
			return linuxsock.SubmitBusy
		}
		slot, lent = free, true
		copy(link.tx[uint64(slot)*size:uint64(slot+1)*size], data)
	}
	var encoded bool
	if proto == linuxsock.TCP {
		status := encodeTCP(request, slot, len(data))
		if status != linuxsock.SubmitAccepted {
			return status
		}
		encoded = tcpLayout.EncodeRequest(&tcpRequest, tag, &nativeOut)
	} else {
		status := encodeUDP(request, slot)
		if status != linuxsock.SubmitAccepted {
			return status
		}
		encoded = udpapi.EncodeRequest(&udpRequest, tag, &nativeOut)
	}
	if !encoded {
		return linuxsock.SubmitUnsupported
	}
	switch link.send(&nativeOut) {
	case kcall.StatusOK:
	case kcall.StatusPeerDied:
		return linuxsock.SubmitDown
	default:
		return linuxsock.SubmitBusy
	}
	if lent {
		link.txLent.Lend(slot)
	}
	return linuxsock.SubmitAccepted
}

func encodeTCP(r *linuxsock.NativeRequest, slot uint8, length int) linuxsock.SubmitStatus {
	tcpRequest = tcpapi.Request{Endpoint: r.Endpoint}
	if r.Flags&linuxsock.NativeNoWait != 0 {
		tcpRequest.Flags |= tcpapi.FlagNoWait
	}
	if r.Flags&linuxsock.NativePeek != 0 {
		tcpRequest.Flags |= tcpapi.FlagPeek
	}
	switch r.Op {
	case linuxsock.NativeOpen:
		tcpRequest.Opcode = tcpapi.OpOpen
	case linuxsock.NativeBind:
		tcpRequest.Opcode, tcpRequest.Address, tcpRequest.Port = tcpapi.OpBind, r.Address, r.Port
		if r.Flags&linuxsock.NativeReuseAddr != 0 {
			tcpRequest.Flags = tcpapi.FlagReuseAddr
		}
	case linuxsock.NativeListen:
		tcpRequest.Opcode, tcpRequest.Backlog = tcpapi.OpListen, uint8(min(r.Length, 255))
	case linuxsock.NativeAccept:
		tcpRequest.Opcode = tcpapi.OpAccept
	case linuxsock.NativeConnect:
		tcpRequest.Opcode, tcpRequest.Address, tcpRequest.Port = tcpapi.OpConnect, r.Address, r.Port
	case linuxsock.NativeRead:
		tcpRequest.Opcode, tcpRequest.Length = tcpapi.OpRead, uint16(min(r.Length, tcpLayout.SlotSize))
	case linuxsock.NativeWrite:
		tcpRequest.Opcode, tcpRequest.Slot, tcpRequest.Length = tcpapi.OpWrite, slot, uint16(length)
		tcpRequest.Offset = r.Offset
	case linuxsock.NativeShutdownWrite:
		tcpRequest.Opcode = tcpapi.OpShutdownWrite
	case linuxsock.NativeClose:
		tcpRequest.Opcode = tcpapi.OpClose
	case linuxsock.NativeReady:
		tcpRequest.Opcode = tcpapi.OpReady
	case linuxsock.NativeWaitReady:
		tcpRequest.Opcode, tcpRequest.Mask = tcpapi.OpWaitReady, tcpMask(r.Mask)
	case linuxsock.NativeError:
		tcpRequest.Opcode = tcpapi.OpError
	case linuxsock.NativeOption:
		tcpRequest.Opcode = tcpapi.OpOption
		switch r.Option {
		case linuxsock.OptionMSS:
			tcpRequest.Option = tcpapi.OptionSendMSS
		case linuxsock.OptionSendBuffer:
			tcpRequest.Option = tcpapi.OptionSendBuffer
		case linuxsock.OptionReceiveBuffer:
			tcpRequest.Option = tcpapi.OptionReceiveBuffer
		case linuxsock.OptionTTL:
			tcpRequest.Option = tcpapi.OptionTTL
		default:
			return linuxsock.SubmitUnsupported
		}
	default:
		return linuxsock.SubmitUnsupported
	}
	return linuxsock.SubmitAccepted
}

func encodeUDP(r *linuxsock.NativeRequest, slot uint8) linuxsock.SubmitStatus {
	udpRequest = udpapi.Request{Endpoint: r.Endpoint}
	switch r.Op {
	case linuxsock.NativeOpen:
		udpRequest.Opcode = udpapi.OpOpen
	case linuxsock.NativeBind:
		udpRequest.Opcode, udpRequest.Address, udpRequest.Port = udpapi.OpBind, r.Address, r.Port
		if r.Flags&linuxsock.NativeReuseAddr != 0 {
			udpRequest.Flags = udpapi.FlagReuseAddr
		}
	case linuxsock.NativeConnect:
		udpRequest.Opcode, udpRequest.Address, udpRequest.Port = udpapi.OpConnect, r.Address, r.Port
	case linuxsock.NativeRead:
		udpRequest.Opcode = udpapi.OpRecvFrom
		if r.Flags&linuxsock.NativeNoWait != 0 {
			udpRequest.Flags |= udpapi.FlagNoWait
		}
		if r.Flags&linuxsock.NativePeek != 0 {
			udpRequest.Flags |= udpapi.FlagPeek
		}
	case linuxsock.NativeWrite:
		udpRequest.Opcode, udpRequest.Slot, udpRequest.Length = udpapi.OpSend, slot, uint16(len(r.Data))
	case linuxsock.NativeSendTo:
		udpRequest.Opcode, udpRequest.Slot, udpRequest.Length = udpapi.OpSendTo, slot, uint16(len(r.Data))
		udpRequest.Address, udpRequest.Port = r.Address, r.Port
	case linuxsock.NativeClose:
		udpRequest.Opcode = udpapi.OpClose
	case linuxsock.NativeReady:
		udpRequest.Opcode = udpapi.OpReady
	case linuxsock.NativeWaitReady:
		udpRequest.Opcode, udpRequest.Mask = udpapi.OpWaitReady, udpMask(r.Mask)
	case linuxsock.NativeOption:
		if r.Option != linuxsock.OptionTTL {
			return linuxsock.SubmitUnsupported
		}
		udpRequest.Opcode, udpRequest.Option = udpapi.OpOption, udpapi.OptionTTL
	default:
		return linuxsock.SubmitUnsupported
	}
	return linuxsock.SubmitAccepted
}

// The native masks and the core's name the same conditions.
var tcpBits = [...]struct {
	native uint16
	core   linuxsock.NativeMask
}{
	{tcpapi.ReadyAcceptable, linuxsock.MaskAcceptable},
	{tcpapi.ReadyConnected, linuxsock.MaskConnected},
	{tcpapi.ReadyConnectFailed, linuxsock.MaskConnectFailed},
	{tcpapi.ReadyReadable, linuxsock.MaskReadable},
	{tcpapi.ReadyReadEOF, linuxsock.MaskReceiveEOF},
	{tcpapi.ReadyWritable, linuxsock.MaskWritable},
	{tcpapi.ReadyWriteClosed, linuxsock.MaskWriteClosed},
	{tcpapi.ReadyError, linuxsock.MaskPendingError},
	{tcpapi.ReadyHangUp, linuxsock.MaskHungUp},
	{tcpapi.ReadyConnecting, linuxsock.MaskConnecting},
	{tcpapi.ReadyListening, linuxsock.MaskListening},
}

func tcpMask(mask linuxsock.NativeMask) uint16 {
	var native uint16
	for _, bit := range tcpBits {
		if mask&bit.core != 0 {
			native |= bit.native
		}
	}
	return native
}

func coreTCPMask(native uint16) linuxsock.NativeMask {
	var mask linuxsock.NativeMask
	for _, bit := range tcpBits {
		if native&bit.native != 0 {
			mask |= bit.core
		}
	}
	return mask
}

func udpMask(mask linuxsock.NativeMask) uint16 {
	var native uint16
	if mask&linuxsock.MaskReadable != 0 {
		native |= udpapi.ReadyReadable
	}
	if mask&linuxsock.MaskWritable != 0 {
		native |= udpapi.ReadyWritable
	}
	return native
}

func coreUDPMask(native uint16) linuxsock.NativeMask {
	var mask linuxsock.NativeMask
	if native&udpapi.ReadyReadable != 0 {
		mask |= linuxsock.MaskReadable
	}
	if native&udpapi.ReadyWritable != 0 {
		mask |= linuxsock.MaskWritable
	}
	return mask
}

// serviceLink reads every reply waiting on a session and hands each to the
// core. A dead session takes its sockets down.
func serviceLink(proto linuxsock.Proto) {
	link := &links[proto]
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
		var ok bool
		var correlation uint64
		if proto == linuxsock.TCP {
			correlation, ok = completeTCP(link)
		} else {
			correlation, ok = completeUDP(link)
		}
		if !ok {
			syslog.Warn("native session sent a malformed reply", syslog.Uint("protocol", uint64(proto)))
			dropLink(proto)
			return
		}
		core.Complete(proto, link.generation, correlation, &completion)
		if completion.Data != nil {
			link.release(tcpReply.Slot, udpReply.Slot, proto)
		}
	}
}

func tcpResult(result tcpapi.Result) linuxsock.NativeResult {
	switch result {
	case tcpapi.ResultOK:
		return linuxsock.NativeOK
	case tcpapi.ResultInvalidArgument:
		return linuxsock.NativeInvalidArgument
	case tcpapi.ResultBusy:
		return linuxsock.NativeBusy
	case tcpapi.ResultDown:
		return linuxsock.NativeDown
	case tcpapi.ResultNoAddress:
		return linuxsock.NativeNoAddress
	case tcpapi.ResultNoRoute:
		return linuxsock.NativeNoRoute
	case tcpapi.ResultNeighborUnreachable:
		return linuxsock.NativeNeighborUnreachable
	case tcpapi.ResultReconfigured:
		return linuxsock.NativeReconfigured
	case tcpapi.ResultAddressInUse:
		return linuxsock.NativeAddressInUse
	case tcpapi.ResultConnectionRefused:
		return linuxsock.NativeRefused
	case tcpapi.ResultTimeout:
		return linuxsock.NativeTimeout
	case tcpapi.ResultReset:
		return linuxsock.NativeReset
	case tcpapi.ResultDriverFault:
		return linuxsock.NativeDriverFault
	case tcpapi.ResultEOF:
		return linuxsock.NativeEOF
	case tcpapi.ResultWouldBlock:
		return linuxsock.NativeWouldBlock
	case tcpapi.ResultInProgress:
		return linuxsock.NativeInProgress
	}
	return linuxsock.NativeClosed
}

func udpResult(result udpapi.Result) linuxsock.NativeResult {
	switch result {
	case udpapi.ResultOK:
		return linuxsock.NativeOK
	case udpapi.ResultInvalidArgument:
		return linuxsock.NativeInvalidArgument
	case udpapi.ResultBusy:
		return linuxsock.NativeBusy
	case udpapi.ResultDown:
		return linuxsock.NativeDown
	case udpapi.ResultNoAddress:
		return linuxsock.NativeNoAddress
	case udpapi.ResultNoRoute:
		return linuxsock.NativeNoRoute
	case udpapi.ResultNeighborUnreachable:
		return linuxsock.NativeNeighborUnreachable
	case udpapi.ResultReconfigured:
		return linuxsock.NativeReconfigured
	case udpapi.ResultAddressInUse:
		return linuxsock.NativeAddressInUse
	case udpapi.ResultDriverFault:
		return linuxsock.NativeDriverFault
	case udpapi.ResultWouldBlock:
		return linuxsock.NativeWouldBlock
	case udpapi.ResultMessageSize:
		return linuxsock.NativeMessageSize
	}
	return linuxsock.NativeClosed
}

// completeTCP turns a GTCP 3 reply into completion.
func completeTCP(link *nativeLink) (uint64, bool) {
	tcpReply = tcpapi.Reply{}
	correlation, ok := tcpLayout.DecodeReply(&nativeIn, &tcpReply)
	if !ok {
		return 0, false
	}
	completion = linuxsock.NativeCompletion{Result: tcpResult(tcpReply.Result), Endpoint: tcpReply.Endpoint}
	r := &tcpReply
	switch r.Opcode {
	case tcpapi.OpOpen:
		completion.Op = linuxsock.NativeOpen
	case tcpapi.OpBind:
		completion.Op, completion.Local, completion.LocalPort = linuxsock.NativeBind, r.Address, r.Port
	case tcpapi.OpListen:
		completion.Op = linuxsock.NativeListen
	case tcpapi.OpAccept:
		completion.Op, completion.Address, completion.Port = linuxsock.NativeAccept, r.Address, r.Port
		completion.Local, completion.LocalPort = r.Local, r.LocalPort
	case tcpapi.OpConnect:
		completion.Op, completion.Local, completion.LocalPort = linuxsock.NativeConnect, r.Local, r.LocalPort
	case tcpapi.OpRead:
		completion.Op = linuxsock.NativeRead
		if r.Result == tcpapi.ResultOK {
			if !link.rxLend(r.Slot) {
				return 0, false
			}
			offset := tcpLayout.Offset(r.Slot)
			completion.Data = link.rx[offset : offset+uint64(r.Length)]
		}
	case tcpapi.OpWrite:
		completion.Op, completion.Length = linuxsock.NativeWrite, uint32(r.Length)
		if !link.txLent.Return(r.Slot) {
			return 0, false
		}
	case tcpapi.OpShutdownWrite:
		completion.Op = linuxsock.NativeShutdownWrite
	case tcpapi.OpClose:
		completion.Op = linuxsock.NativeClose
	case tcpapi.OpReady, tcpapi.OpWaitReady:
		completion.Op = linuxsock.NativeReady
		if r.Opcode == tcpapi.OpWaitReady {
			completion.Op = linuxsock.NativeWaitReady
		}
		completion.Mask, completion.Readable = coreTCPMask(r.Mask), r.Value
	case tcpapi.OpError:
		completion.Op, completion.Value = linuxsock.NativeError, uint32(tcpResult(r.Error))
	case tcpapi.OpOption:
		completion.Op, completion.Value = linuxsock.NativeOption, r.Value
	}
	return correlation, true
}

// completeUDP turns a GUDP 2 reply into completion.
func completeUDP(link *nativeLink) (uint64, bool) {
	udpReply = udpapi.Reply{}
	correlation, ok := udpapi.DecodeReply(&nativeIn, &udpReply)
	if !ok {
		return 0, false
	}
	completion = linuxsock.NativeCompletion{Result: udpResult(udpReply.Result), Endpoint: udpReply.Endpoint}
	r := &udpReply
	switch r.Opcode {
	case udpapi.OpOpen:
		completion.Op = linuxsock.NativeOpen
	case udpapi.OpBind:
		completion.Op, completion.Local, completion.LocalPort = linuxsock.NativeBind, r.Address, r.Port
	case udpapi.OpConnect:
		completion.Op, completion.Local, completion.LocalPort = linuxsock.NativeConnect, r.Local, r.LocalPort
	case udpapi.OpSend:
		completion.Op = linuxsock.NativeWrite
		if !link.txLent.Return(r.Slot) {
			return 0, false
		}
	case udpapi.OpSendTo:
		completion.Op = linuxsock.NativeSendTo
		if !link.txLent.Return(r.Slot) {
			return 0, false
		}
	case udpapi.OpRecvFrom:
		completion.Op = linuxsock.NativeRead
		if r.Result == udpapi.ResultOK {
			if !link.rxLend(r.Slot) {
				return 0, false
			}
			completion.Address, completion.Port, completion.Full = r.Address, r.Port, uint32(r.Length)
			completion.Data = link.rx[uint64(r.Slot)*udpapi.SlotSize : uint64(r.Slot)*udpapi.SlotSize+uint64(r.Length)]
		}
	case udpapi.OpClose:
		completion.Op = linuxsock.NativeClose
	case udpapi.OpReady, udpapi.OpWaitReady:
		completion.Op = linuxsock.NativeReady
		if r.Opcode == udpapi.OpWaitReady {
			completion.Op = linuxsock.NativeWaitReady
		}
		completion.Mask, completion.Readable = coreUDPMask(r.Mask), r.Value
	case udpapi.OpOption:
		completion.Op, completion.Value = linuxsock.NativeOption, r.Value
	}
	return correlation, true
}

// rxLend checks that netd lends a slot it does not already have lent to
// this side.
func (link *nativeLink) rxLend(slot uint8) bool {
	if slot >= 8 || link.releases&(1<<slot) != 0 {
		return false
	}
	return true
}

// release returns a native RX slot after the core copied its bytes.
func (link *nativeLink) release(tcpSlot, udpSlot uint8, proto linuxsock.Proto) {
	slot := tcpSlot
	if proto == linuxsock.UDP {
		slot = udpSlot
	}
	link.releases |= 1 << slot
	link.flushReleases()
}

func (link *nativeLink) flushReleases() {
	for slot := uint8(0); slot < 8 && link.releases != 0; slot++ {
		if link.releases&(1<<slot) == 0 {
			continue
		}
		var encoded bool
		if link.proto == linuxsock.TCP {
			request := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
			encoded = tcpLayout.EncodeRequest(&request, 0, &nativeOut)
		} else {
			request := udpapi.Request{Opcode: udpapi.OpRelease, Slot: slot}
			encoded = udpapi.EncodeRequest(&request, 0, &nativeOut)
		}
		if !encoded || link.send(&nativeOut) != kcall.StatusOK {
			return
		}
		link.releases &^= 1 << slot
	}
}

func (link *nativeLink) send(message *ipc.Message) kcall.Status {
	size, ok := ipc.EncodeFrom(message, nativeWire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	return sys.IPCSend(link.channel, nativeWire[:size])
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
		txBase: txBase, txSize: txSize, rxBase: rxBase, rxSize: rxSize,
		tx: sys.Window(txBase, txPool), rx: sys.Window(rxBase, rxPool)}
	slots := tcpLayout.TXSlots
	if proto == linuxsock.UDP {
		slots = udpapi.TXSlots
	}
	link.txLent.Reset(slots)
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
