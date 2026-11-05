package linuxsock

// NativeLink is the service's end of one native session, GTCP 3 or GUDP 2,
// without the channel and the mapping, which the platform owns: the codec,
// the TX slots netd holds, and the RX slot releases a full queue held
// back. cmd/linuxsockd drives it with the channel's send and receive.

import (
	"goos/kernel/ipc"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

// SendStatus is what a send on the native channel did.
type SendStatus uint8

const (
	SendOK SendStatus = iota
	// SendFull: netd's queue is full; a writable event follows.
	SendFull
	// SendGone: the session is gone.
	SendGone
)

// NativeChannel sends one encoded message to netd.
type NativeChannel interface {
	Send(message *ipc.Message) SendStatus
}

// NativeLink holds one session. The zero value is unusable; call Reset.
type NativeLink struct {
	Proto      Proto
	Generation uint64
	layout     tcpapi.Layout
	tx, rx     []byte
	// txLent tracks the TX slots netd holds; releases are RX slots whose
	// Release found the queue full and is sent again.
	txLent   tcpapi.Slots
	releases uint8
	// writeBlocked: a send found netd's queue full. The event loop waits
	// for the channel to turn writable then, since requests that park in
	// netd draw no reply that would wake it.
	writeBlocked bool
	// lastSlot is the RX slot of the completion Decode returned; holding
	// says netd lent it and it has not gone back yet.
	lastSlot   uint8
	holding    bool
	out        ipc.Message
	nativeIn   ipc.Message
	completion NativeCompletion
	tcpRequest tcpapi.Request
	tcpReply   tcpapi.Reply
	udpRequest udpapi.Request
	udpReply   udpapi.Reply
}

// Reset installs a session of proto: tx is the TX pool the service writes,
// rx the RX pool netd lends from.
func (l *NativeLink) Reset(proto Proto, generation uint64, layout tcpapi.Layout, tx, rx []byte) {
	*l = NativeLink{Proto: proto, Generation: generation, layout: layout, tx: tx, rx: rx}
	slots := layout.TXSlots
	if proto == UDP {
		slots = udpapi.TXSlots
	}
	l.txLent.Reset(slots)
}

func (l *NativeLink) send(ch NativeChannel, message *ipc.Message) SendStatus {
	status := ch.Send(message)
	if status == SendFull {
		l.writeBlocked = true
	}
	return status
}

// Submit encodes and sends one request; it is Backend.Submit for this link.
//
// A GTCP 3 request carries the RX slots this side returns, so a read posted
// right after the last one answered does not cost netd a Release message
// first. The core copies a read's bytes before it submits anything while it
// handles the read, so a submission during that handling returns the slot
// too, and the completion's Data goes with it.
func (l *NativeLink) Submit(ch NativeChannel, tag uint64, request *NativeRequest) SubmitStatus {
	carry := l.Proto == TCP && l.layout.StreamOffsets()
	if carry && l.holding {
		l.holding, l.completion.Data = false, nil
		l.releases |= 1 << l.lastSlot
	}
	if !carry && l.Flush(ch) == SendGone {
		return SubmitDown
	}
	slot, lent := uint8(0), false
	data := request.Data
	if request.Op == NativeWrite || request.Op == NativeSendTo {
		size := uint64(udpapi.SlotSize)
		if l.Proto == TCP {
			// A stream write sends what fits; the reply's Length says so.
			size = uint64(l.layout.SlotSize)
			data = data[:min(uint64(len(data)), size)]
		} else if uint64(len(data)) > size {
			return SubmitUnsupported
		}
		free, ok := l.txLent.Free()
		if !ok {
			return SubmitBusy
		}
		slot, lent = free, true
		copy(l.tx[uint64(slot)*size:uint64(slot+1)*size], data)
	}
	var encoded bool
	if l.Proto == TCP {
		if status := l.encodeTCP(request, slot, len(data)); status != SubmitAccepted {
			return status
		}
		if carry {
			l.tcpRequest.Release = l.releases
		}
		encoded = l.layout.EncodeRequest(&l.tcpRequest, tag, &l.out)
	} else {
		if status := l.encodeUDP(request, slot); status != SubmitAccepted {
			return status
		}
		encoded = udpapi.EncodeRequest(&l.udpRequest, tag, &l.out)
	}
	if !encoded {
		return SubmitUnsupported
	}
	switch l.send(ch, &l.out) {
	case SendOK:
	case SendGone:
		return SubmitDown
	default:
		return SubmitBusy
	}
	if carry {
		l.releases = 0
	}
	if lent {
		l.txLent.Lend(slot)
	}
	return SubmitAccepted
}

// Decode turns a reply from netd into a completion for the core. ok false
// means netd broke the protocol. The completion's Data stays valid until
// Done, or until a GTCP 3 Submit returns its slot.
func (l *NativeLink) Decode(in *ipc.Message) (correlation uint64, completion *NativeCompletion, ok bool) {
	l.nativeIn = *in
	if l.Proto == TCP {
		correlation, ok = l.completeTCP()
		l.lastSlot = l.tcpReply.Slot
	} else {
		correlation, ok = l.completeUDP()
		l.lastSlot = l.udpReply.Slot
	}
	l.holding = ok && l.completion.Data != nil
	return correlation, &l.completion, ok
}

// Done returns the RX slot of the last decoded completion, once the core
// copied its bytes, now or when the queue has room.
func (l *NativeLink) Done(ch NativeChannel) SendStatus {
	if !l.holding {
		return SendOK
	}
	l.holding, l.completion.Data = false, nil
	l.releases |= 1 << l.lastSlot
	return l.Flush(ch)
}

// Flush sends the releases a full queue held back.
func (l *NativeLink) Flush(ch NativeChannel) SendStatus {
	for slot := uint8(0); slot < 8 && l.releases != 0; slot++ {
		if l.releases&(1<<slot) == 0 {
			continue
		}
		var encoded bool
		if l.Proto == TCP {
			request := tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
			encoded = l.layout.EncodeRequest(&request, 0, &l.out)
		} else {
			request := udpapi.Request{Opcode: udpapi.OpRelease, Slot: slot}
			encoded = udpapi.EncodeRequest(&request, 0, &l.out)
		}
		if !encoded {
			return SendGone
		}
		if status := l.send(ch, &l.out); status != SendOK {
			return status
		}
		l.releases &^= 1 << slot
	}
	return SendOK
}

// WantsWritable reports whether the link has work that only room in netd's
// queue lets go on: the event loop waits for the channel to be writable.
func (l *NativeLink) WantsWritable() bool { return l.writeBlocked || l.releases != 0 }

// Writable retries what a full queue held back when the channel turned
// writable: the releases first, then the core's submissions. It reports
// SendGone when the session is gone.
func (l *NativeLink) Writable(ch NativeChannel, core *Service) SendStatus {
	if !l.WantsWritable() {
		return SendOK
	}
	l.writeBlocked = false
	if status := l.Flush(ch); status != SendOK {
		return status
	}
	core.BackendWritable(l.Proto)
	return SendOK
}

// rxLend checks that netd lends a slot it does not already have lent to
// this side.
func (l *NativeLink) rxLend(slot uint8) bool {
	return slot < 8 && l.releases&(1<<slot) == 0
}

func (l *NativeLink) encodeTCP(r *NativeRequest, slot uint8, length int) SubmitStatus {
	l.tcpRequest = tcpapi.Request{Endpoint: r.Endpoint}
	if r.Flags&NativeNoWait != 0 {
		l.tcpRequest.Flags |= tcpapi.FlagNoWait
	}
	if r.Flags&NativePeek != 0 {
		l.tcpRequest.Flags |= tcpapi.FlagPeek
	}
	switch r.Op {
	case NativeOpen:
		l.tcpRequest.Opcode = tcpapi.OpOpen
	case NativeBind:
		l.tcpRequest.Opcode, l.tcpRequest.Address, l.tcpRequest.Port = tcpapi.OpBind, r.Address, r.Port
		if r.Flags&NativeReuseAddr != 0 {
			l.tcpRequest.Flags = tcpapi.FlagReuseAddr
		}
	case NativeListen:
		l.tcpRequest.Opcode, l.tcpRequest.Backlog = tcpapi.OpListen, uint8(min(r.Length, 255))
	case NativeAccept:
		l.tcpRequest.Opcode = tcpapi.OpAccept
	case NativeConnect:
		l.tcpRequest.Opcode, l.tcpRequest.Address, l.tcpRequest.Port = tcpapi.OpConnect, r.Address, r.Port
	case NativeRead:
		l.tcpRequest.Opcode, l.tcpRequest.Length = tcpapi.OpRead, uint16(min(r.Length, l.layout.SlotSize))
	case NativeWrite:
		l.tcpRequest.Opcode, l.tcpRequest.Slot, l.tcpRequest.Length = tcpapi.OpWrite, slot, uint16(length)
		l.tcpRequest.Offset = r.Offset
	case NativeShutdownWrite:
		l.tcpRequest.Opcode = tcpapi.OpShutdownWrite
	case NativeClose:
		l.tcpRequest.Opcode = tcpapi.OpClose
	case NativeReady:
		l.tcpRequest.Opcode = tcpapi.OpReady
	case NativeWaitReady:
		l.tcpRequest.Opcode, l.tcpRequest.Mask = tcpapi.OpWaitReady, tcpMask(r.Mask)
	case NativeError:
		l.tcpRequest.Opcode = tcpapi.OpError
	case NativeOption:
		l.tcpRequest.Opcode = tcpapi.OpOption
		switch r.Option {
		case OptionMSS:
			l.tcpRequest.Option = tcpapi.OptionSendMSS
		case OptionSendBuffer:
			l.tcpRequest.Option = tcpapi.OptionSendBuffer
		case OptionReceiveBuffer:
			l.tcpRequest.Option = tcpapi.OptionReceiveBuffer
		case OptionTTL:
			l.tcpRequest.Option = tcpapi.OptionTTL
		default:
			return SubmitUnsupported
		}
	default:
		return SubmitUnsupported
	}
	return SubmitAccepted
}

func (l *NativeLink) encodeUDP(r *NativeRequest, slot uint8) SubmitStatus {
	l.udpRequest = udpapi.Request{Endpoint: r.Endpoint}
	switch r.Op {
	case NativeOpen:
		l.udpRequest.Opcode = udpapi.OpOpen
	case NativeBind:
		l.udpRequest.Opcode, l.udpRequest.Address, l.udpRequest.Port = udpapi.OpBind, r.Address, r.Port
		if r.Flags&NativeReuseAddr != 0 {
			l.udpRequest.Flags = udpapi.FlagReuseAddr
		}
	case NativeConnect:
		l.udpRequest.Opcode, l.udpRequest.Address, l.udpRequest.Port = udpapi.OpConnect, r.Address, r.Port
	case NativeRead:
		l.udpRequest.Opcode = udpapi.OpRecvFrom
		if r.Flags&NativeNoWait != 0 {
			l.udpRequest.Flags |= udpapi.FlagNoWait
		}
		if r.Flags&NativePeek != 0 {
			l.udpRequest.Flags |= udpapi.FlagPeek
		}
	case NativeWrite:
		l.udpRequest.Opcode, l.udpRequest.Slot, l.udpRequest.Length = udpapi.OpSend, slot, uint16(len(r.Data))
	case NativeSendTo:
		l.udpRequest.Opcode, l.udpRequest.Slot, l.udpRequest.Length = udpapi.OpSendTo, slot, uint16(len(r.Data))
		l.udpRequest.Address, l.udpRequest.Port = r.Address, r.Port
	case NativeClose:
		l.udpRequest.Opcode = udpapi.OpClose
	case NativeReady:
		l.udpRequest.Opcode = udpapi.OpReady
	case NativeWaitReady:
		l.udpRequest.Opcode, l.udpRequest.Mask = udpapi.OpWaitReady, udpMask(r.Mask)
	case NativeOption:
		if r.Option != OptionTTL {
			return SubmitUnsupported
		}
		l.udpRequest.Opcode, l.udpRequest.Option = udpapi.OpOption, udpapi.OptionTTL
	default:
		return SubmitUnsupported
	}
	return SubmitAccepted
}

// The native masks and the core's name the same conditions.
var tcpBits = [...]struct {
	native uint16
	core   NativeMask
}{
	{tcpapi.ReadyAcceptable, MaskAcceptable},
	{tcpapi.ReadyConnected, MaskConnected},
	{tcpapi.ReadyConnectFailed, MaskConnectFailed},
	{tcpapi.ReadyReadable, MaskReadable},
	{tcpapi.ReadyReadEOF, MaskReceiveEOF},
	{tcpapi.ReadyWritable, MaskWritable},
	{tcpapi.ReadyWriteClosed, MaskWriteClosed},
	{tcpapi.ReadyError, MaskPendingError},
	{tcpapi.ReadyHangUp, MaskHungUp},
	{tcpapi.ReadyConnecting, MaskConnecting},
	{tcpapi.ReadyListening, MaskListening},
}

func tcpMask(mask NativeMask) uint16 {
	var native uint16
	for _, bit := range tcpBits {
		if mask&bit.core != 0 {
			native |= bit.native
		}
	}
	return native
}

func coreTCPMask(native uint16) NativeMask {
	var mask NativeMask
	for _, bit := range tcpBits {
		if native&bit.native != 0 {
			mask |= bit.core
		}
	}
	return mask
}

func udpMask(mask NativeMask) uint16 {
	var native uint16
	if mask&MaskReadable != 0 {
		native |= udpapi.ReadyReadable
	}
	if mask&MaskWritable != 0 {
		native |= udpapi.ReadyWritable
	}
	return native
}

func coreUDPMask(native uint16) NativeMask {
	var mask NativeMask
	if native&udpapi.ReadyReadable != 0 {
		mask |= MaskReadable
	}
	if native&udpapi.ReadyWritable != 0 {
		mask |= MaskWritable
	}
	return mask
}

// serviceLink reads every reply waiting on a session and hands each to the
// core. A dead session takes its sockets down.
func tcpResult(result tcpapi.Result) NativeResult {
	switch result {
	case tcpapi.ResultOK:
		return NativeOK
	case tcpapi.ResultInvalidArgument:
		return NativeInvalidArgument
	case tcpapi.ResultBusy:
		return NativeBusy
	case tcpapi.ResultDown:
		return NativeDown
	case tcpapi.ResultNoAddress:
		return NativeNoAddress
	case tcpapi.ResultNoRoute:
		return NativeNoRoute
	case tcpapi.ResultNeighborUnreachable:
		return NativeNeighborUnreachable
	case tcpapi.ResultReconfigured:
		return NativeReconfigured
	case tcpapi.ResultAddressInUse:
		return NativeAddressInUse
	case tcpapi.ResultConnectionRefused:
		return NativeRefused
	case tcpapi.ResultTimeout:
		return NativeTimeout
	case tcpapi.ResultReset:
		return NativeReset
	case tcpapi.ResultDriverFault:
		return NativeDriverFault
	case tcpapi.ResultEOF:
		return NativeEOF
	case tcpapi.ResultWouldBlock:
		return NativeWouldBlock
	case tcpapi.ResultInProgress:
		return NativeInProgress
	}
	return NativeClosed
}

func udpResult(result udpapi.Result) NativeResult {
	switch result {
	case udpapi.ResultOK:
		return NativeOK
	case udpapi.ResultInvalidArgument:
		return NativeInvalidArgument
	case udpapi.ResultBusy:
		return NativeBusy
	case udpapi.ResultDown:
		return NativeDown
	case udpapi.ResultNoAddress:
		return NativeNoAddress
	case udpapi.ResultNoRoute:
		return NativeNoRoute
	case udpapi.ResultNeighborUnreachable:
		return NativeNeighborUnreachable
	case udpapi.ResultReconfigured:
		return NativeReconfigured
	case udpapi.ResultAddressInUse:
		return NativeAddressInUse
	case udpapi.ResultDriverFault:
		return NativeDriverFault
	case udpapi.ResultWouldBlock:
		return NativeWouldBlock
	case udpapi.ResultMessageSize:
		return NativeMessageSize
	}
	return NativeClosed
}

// completeTCP turns a GTCP 3 reply into l.completion.
func (l *NativeLink) completeTCP() (uint64, bool) {
	l.tcpReply = tcpapi.Reply{}
	correlation, ok := l.layout.DecodeReply(&l.nativeIn, &l.tcpReply)
	if !ok {
		return 0, false
	}
	l.completion = NativeCompletion{Result: tcpResult(l.tcpReply.Result), Endpoint: l.tcpReply.Endpoint}
	r := &l.tcpReply
	switch r.Opcode {
	case tcpapi.OpOpen:
		l.completion.Op = NativeOpen
	case tcpapi.OpBind:
		l.completion.Op, l.completion.Local, l.completion.LocalPort = NativeBind, r.Address, r.Port
	case tcpapi.OpListen:
		l.completion.Op = NativeListen
	case tcpapi.OpAccept:
		l.completion.Op, l.completion.Address, l.completion.Port = NativeAccept, r.Address, r.Port
		l.completion.Local, l.completion.LocalPort = r.Local, r.LocalPort
	case tcpapi.OpConnect:
		l.completion.Op, l.completion.Local, l.completion.LocalPort = NativeConnect, r.Local, r.LocalPort
	case tcpapi.OpRead:
		l.completion.Op = NativeRead
		if r.Result == tcpapi.ResultOK {
			if !l.rxLend(r.Slot) {
				return 0, false
			}
			offset := l.layout.Offset(r.Slot)
			l.completion.Data = l.rx[offset : offset+uint64(r.Length)]
		}
	case tcpapi.OpWrite:
		l.completion.Op, l.completion.Length = NativeWrite, uint32(r.Length)
		if !l.txLent.Return(r.Slot) {
			return 0, false
		}
	case tcpapi.OpShutdownWrite:
		l.completion.Op = NativeShutdownWrite
	case tcpapi.OpClose:
		l.completion.Op = NativeClose
	case tcpapi.OpReady, tcpapi.OpWaitReady:
		l.completion.Op = NativeReady
		if r.Opcode == tcpapi.OpWaitReady {
			l.completion.Op = NativeWaitReady
		}
		l.completion.Mask, l.completion.Readable = coreTCPMask(r.Mask), r.Value
	case tcpapi.OpError:
		l.completion.Op, l.completion.Value = NativeError, uint32(tcpResult(r.Error))
	case tcpapi.OpOption:
		l.completion.Op, l.completion.Value = NativeOption, r.Value
	}
	return correlation, true
}

// completeUDP turns a GUDP 2 reply into l.completion.
func (l *NativeLink) completeUDP() (uint64, bool) {
	l.udpReply = udpapi.Reply{}
	correlation, ok := udpapi.DecodeReply(&l.nativeIn, &l.udpReply)
	if !ok {
		return 0, false
	}
	l.completion = NativeCompletion{Result: udpResult(l.udpReply.Result), Endpoint: l.udpReply.Endpoint}
	r := &l.udpReply
	switch r.Opcode {
	case udpapi.OpOpen:
		l.completion.Op = NativeOpen
	case udpapi.OpBind:
		l.completion.Op, l.completion.Local, l.completion.LocalPort = NativeBind, r.Address, r.Port
	case udpapi.OpConnect:
		l.completion.Op, l.completion.Local, l.completion.LocalPort = NativeConnect, r.Local, r.LocalPort
	case udpapi.OpSend:
		l.completion.Op = NativeWrite
		if !l.txLent.Return(r.Slot) {
			return 0, false
		}
	case udpapi.OpSendTo:
		l.completion.Op = NativeSendTo
		if !l.txLent.Return(r.Slot) {
			return 0, false
		}
	case udpapi.OpRecvFrom:
		l.completion.Op = NativeRead
		if r.Result == udpapi.ResultOK {
			if !l.rxLend(r.Slot) {
				return 0, false
			}
			l.completion.Address, l.completion.Port, l.completion.Full = r.Address, r.Port, uint32(r.Length)
			l.completion.Data = l.rx[uint64(r.Slot)*udpapi.SlotSize : uint64(r.Slot)*udpapi.SlotSize+uint64(r.Length)]
		}
	case udpapi.OpClose:
		l.completion.Op = NativeClose
	case udpapi.OpReady, udpapi.OpWaitReady:
		l.completion.Op = NativeReady
		if r.Opcode == udpapi.OpWaitReady {
			l.completion.Op = NativeWaitReady
		}
		l.completion.Mask, l.completion.Readable = coreUDPMask(r.Mask), r.Value
	case udpapi.OpOption:
		l.completion.Op, l.completion.Value = NativeOption, r.Value
	}
	return correlation, true
}

// Snapshot reports the TX slots netd holds, the releases a full queue held
// back and whether the link waits for room, for diagnostics.
func (l *NativeLink) Snapshot() (txLent uint8, releases uint8, blocked bool) {
	return l.txLent.Bits(), l.releases, l.writeBlocked
}
