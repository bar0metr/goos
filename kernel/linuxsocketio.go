//go:build uefi

package main

// Socket data transfer: read, readv, recvfrom and recvmsg take bytes from
// RX slots, write, writev, sendto and sendmsg put them in TX slots, one
// slot of at most 4096 bytes per exchange. One call moves at most
// MaxVectorBytes, as a file read does; a longer request returns short and
// the program continues.

import (
	"encoding/binary"

	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/userspace/linuxsockapi"
)

// linuxSocketRounds bounds the exchanges and sleeps of one call before it
// returns what it moved: sixteen chunks and the waits between them.
const linuxSocketRounds = 48

// socketTransfer is one send or receive: where its bytes are, how many the
// call may move, and what a receive learned of its datagram.
type socketTransfer struct {
	address uint64
	wire    []byte
	limit   uint64
	flags   uint64
	// destination is a send's address, when it names one.
	destination    linuxcompat.SocketAddress
	hasDestination bool
	source         linuxcompat.SocketAddress
	datagramLength uint64
	truncated      bool
}

// vectorLimit returns how many bytes of a vector the program can use from
// the start, at most bound: writable ones for a receive, readable ones for
// a send. A vector whose first byte is unusable is EFAULT.
func (handler *linuxCallHandler) vectorLimit(wire []byte, bound uint64, write bool) (uint64, linuxabi.Errno) {
	total := uint64(0)
	for index := 0; index < len(wire)/linuxabi.IOVecSize && total < bound; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		length = min(length, bound-total)
		if length == 0 {
			continue
		}
		usable := handler.accessible(base, length, write)
		total += usable
		if usable < length {
			if total == 0 || executablePageWanted(handler.task) {
				return 0, linuxabi.EFAULT
			}
			break
		}
	}
	return total, 0
}

// rangeLimit is vectorLimit for one buffer.
func (handler *linuxCallHandler) rangeLimit(address uint64, length uint64, write bool) (uint64, linuxabi.Errno) {
	if !userRange(address, length) {
		return 0, linuxabi.EFAULT
	}
	length = min(length, uint64(linuxcompat.MaxVectorBytes))
	if length == 0 {
		return 0, 0
	}
	usable := handler.accessible(address, length, write)
	// A page of the program's image the copy cannot take now is not the end
	// of the buffer: the call fails before any effect, and the dispatcher
	// brings the page in and runs it again. Shortening the buffer instead
	// would cut a datagram.
	if usable == 0 || usable < length && executablePageWanted(handler.task) {
		return 0, linuxabi.EFAULT
	}
	return usable, 0
}

// socketRead serves read and readv on a socket.
//
//go:noinline
func (handler *linuxCallHandler) socketRead(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	transfer := &thisCPU().linuxSocketTransfer
	*transfer = socketTransfer{}
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call := &thisCPU().linuxSocket.call
	if errno := handler.socketOf(call, description); errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	limit, wire, errno := handler.readLimit(request, vector, linuxcompat.MaxVectorBytes)
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	transfer.address, transfer.wire, transfer.limit = request.Arguments[1], wire, limit
	return handler.socketReceive(request, call, transfer, pending, resumed)
}

// socketWrite serves write and writev on a socket.
//
//go:noinline
func (handler *linuxCallHandler) socketWrite(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	transfer := &thisCPU().linuxSocketTransfer
	*transfer = socketTransfer{}
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call := &thisCPU().linuxSocket.call
	if errno := handler.socketOf(call, description); errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	var limit uint64
	var wire []byte
	var errno linuxabi.Errno
	if vector {
		if wire, errno = handler.copyIOVec(request); errno == 0 {
			limit, errno = handler.vectorLimit(wire, linuxcompat.MaxVectorBytes, false)
		}
	} else {
		limit, errno = handler.rangeLimit(request.Arguments[1], request.Arguments[2], false)
	}
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	transfer.address, transfer.wire, transfer.limit = request.Arguments[1], wire, limit
	return handler.socketSend(request, call, transfer, pending, resumed)
}

// socketSendCall serves sendto and sendmsg.
//
//go:noinline
func (handler *linuxCallHandler) socketSendCall(request linuxcompat.Request) linuxcompat.Result {
	transfer := &thisCPU().linuxSocketTransfer
	*transfer = socketTransfer{}
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call, errno := handler.socketDescriptor(request.Arguments[0])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	arguments := &request.Arguments
	name, nameLength := uint64(0), uint64(0)
	if request.Number == linuxabi.SysSendto {
		transfer.flags = arguments[3]
		name, nameLength = arguments[4], arguments[5]
		transfer.address = arguments[1]
		transfer.limit, errno = handler.rangeLimit(arguments[1], arguments[2], false)
	} else {
		transfer.flags = arguments[2]
		var header linuxcompat.MessageHeader
		if header, errno = handler.socketHeader(arguments[1], true); errno == 0 {
			errno = linuxcompat.SendControlErrno(header)
		}
		if errno == 0 {
			name, nameLength = header.Name, uint64(header.NameLength)
			transfer.wire, errno = handler.socketVector(header)
		}
		if errno == 0 {
			transfer.limit, errno = handler.vectorLimit(transfer.wire, linuxcompat.MaxVectorBytes, false)
		}
	}
	if errno == 0 {
		errno = linuxcompat.SendFlags(transfer.flags)
	}
	if errno == 0 && name != 0 && call.socket.Datagram {
		var address linuxcompat.SocketAddress
		if address, _, errno = handler.socketAddressIn(name, nameLength, linuxcompat.AddressSend); errno == 0 && address.Port == 0 {
			errno = linuxabi.EINVAL
		}
		transfer.destination, transfer.hasDestination = address, true
	}
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	result = handler.socketSend(request, call, transfer, pending, resumed)
	if result.Errno == linuxabi.EPIPE && transfer.flags&linuxabi.MessageNoSignal == 0 {
		handler.state.Signals.Raise(linuxabi.SignalPipe, linuxcompat.SignalInfo{Code: linuxabi.SignalCodeUser, PID: handler.state.PID, UID: linuxcompat.StatUID})
	}
	return result
}

// socketReceiveCall serves recvfrom and recvmsg.
//
//go:noinline
func (handler *linuxCallHandler) socketReceiveCall(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	scratch := &local.linuxSocket
	transfer := &local.linuxSocketTransfer
	*transfer = socketTransfer{}
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call, errno := handler.socketDescriptor(request.Arguments[0])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	arguments := &request.Arguments
	var header linuxcompat.MessageHeader
	if request.Number == linuxabi.SysRecvfrom {
		transfer.flags = arguments[3]
		transfer.address = arguments[1]
		transfer.limit, errno = handler.rangeLimit(arguments[1], arguments[2], true)
	} else {
		transfer.flags = arguments[2]
		if header, errno = handler.socketHeader(arguments[1], false); errno == 0 {
			transfer.wire, errno = handler.socketVector(header)
		}
		if errno == 0 {
			transfer.limit, errno = handler.vectorLimit(transfer.wire, linuxcompat.MaxVectorBytes, true)
		}
	}
	if errno == 0 {
		errno = linuxcompat.ReceiveFlags(transfer.flags)
	}
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	result = handler.socketReceive(request, call, transfer, pending, resumed)
	if result.Errno != 0 || result.Action == linuxabi.ActionBlock || result.Restart {
		return result
	}
	actual := 0
	if call.socket.Datagram {
		actual = linuxcompat.SockaddrInSize
	}
	if request.Number == linuxabi.SysRecvfrom {
		if errno := handler.socketAddressOut(arguments[4], arguments[5], transfer.source, actual, true); errno != 0 {
			return linuxErrno(errno)
		}
		return result
	}
	// recvmsg writes back the name length, an empty control part, and the
	// flags of what it received.
	if header.Name != 0 {
		size, _ := linuxcompat.SockaddrOutputLength(header.NameLength, actual)
		linuxcompat.EncodeSockaddrIn(&scratch.address, transfer.source)
		if size != 0 && !vm.CopyOut(handler.root, header.Name, scratch.address[:size]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	flags := uint32(0)
	if transfer.truncated {
		flags |= uint32(linuxabi.MessageTrunc)
	}
	nameLength := uint32(actual)
	if header.Name == 0 {
		nameLength = 0
	}
	binary.LittleEndian.PutUint32(scratch.word[:], nameLength)
	var control [8]byte
	binary.LittleEndian.PutUint32(scratch.header[:4], flags)
	if !vm.CopyOut(handler.root, arguments[1]+linuxcompat.MsghdrNameLength, scratch.word[:]) ||
		!vm.CopyOut(handler.root, arguments[1]+linuxcompat.MsghdrControlLength, control[:]) ||
		!vm.CopyOut(handler.root, arguments[1]+linuxcompat.MsghdrFlags, scratch.header[:4]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return result
}

// socketHeader copies and checks a struct msghdr.
func (handler *linuxCallHandler) socketHeader(address uint64, send bool) (linuxcompat.MessageHeader, linuxabi.Errno) {
	scratch := &thisCPU().linuxSocket
	if !vm.CopyIn(handler.root, address, scratch.header[:]) {
		return linuxcompat.MessageHeader{}, linuxabi.EFAULT
	}
	return linuxcompat.DecodeMsghdr(&scratch.header, send)
}

// socketVector copies and checks the vector a msghdr names.
func (handler *linuxCallHandler) socketVector(header linuxcompat.MessageHeader) ([]byte, linuxabi.Errno) {
	local := thisCPU()
	wire := local.linuxIOVecWire[:header.IOVecCount*linuxabi.IOVecSize]
	if header.IOVecCount != 0 && !vm.CopyIn(handler.root, header.IOVec, wire) {
		return nil, linuxabi.EFAULT
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return nil, errno
	}
	return wire, 0
}

// socketReceive takes up to transfer.limit bytes. A stream returns what is
// there once it has some, or waits for all with MSG_WAITALL; a datagram
// socket returns one datagram. EOF ends a stream receive with what it has.
//
//go:noinline
func (handler *linuxCallHandler) socketReceive(request linuxcompat.Request, call *socketCall, transfer *socketTransfer, pending linuxcompat.Pending, resumed bool) linuxcompat.Result {
	timeout := call.socket.RecvTimeout
	nonBlocking := socketNonBlocking(call, transfer.flags)
	peek := transfer.flags&linuxabi.MessagePeek != 0
	waitAll := transfer.flags&linuxabi.MessageWaitAll != 0 && !peek && !call.socket.Datagram
	discard := transfer.flags&linuxabi.MessageTrunc != 0 && !call.socket.Datagram && !peek
	partial := uint64(0)
	if resumed {
		partial = pending.Partial
	}
	if call.socket.Listening {
		// A listener carries no stream; Linux reads it as unconnected.
		handler.socketEnd()
		return linuxErrno(linuxabi.ENOTCONN)
	}
	if transfer.limit == 0 {
		handler.socketEnd()
		return linuxcompat.Result{}
	}
	for round := 0; round < linuxSocketRounds; round++ {
		var reply linuxsockapi.Reply
		var errno linuxabi.Errno
		var blocked bool
		if awaiting(pending, resumed) {
			reply, errno, blocked = handler.socketAwait(call)
		} else {
			if woken(pending, resumed) {
				if result, done := handler.socketSlept(pending, timeout, linuxabi.EAGAIN); done {
					return result
				}
				pending = handler.socketPending()
			}
			// After SHUT_RD the service still returns what it had buffered,
			// then end of file, as Linux does.
			if partial >= transfer.limit {
				handler.socketEnd()
				return linuxcompat.Result{Value: partial}
			}
			if !call.live {
				handler.socketEnd()
				return socketShort(partial, linuxabi.ENETRESET)
			}
			if !resumed {
				if errno := handler.socketStart(request, socketTimeoutEnd(timeout)); errno != 0 {
					return linuxErrno(errno)
				}
				resumed, pending = true, handler.socketPending()
			}
			if state, live := socketReadiness(call); live && state.Ready&(linuxsockapi.ReadyIn|linuxsockapi.ReadyReadHangup|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup) == 0 && state.Readable == 0 {
				if result, done := handler.socketStall(call, linuxsockapi.ReadyIn, nonBlocking, partial != 0 && !waitAll, partial, timeout); done {
					return result
				}
				pending = handler.socketPending()
				continue
			}
			message := socketRequestFor(linuxsockapi.OpRead, call.socket.Identity)
			message.Length = uint32(min(transfer.limit-partial, uint64(linuxsockapi.SlotSize)))
			if peek {
				message.Flags = linuxsockapi.FlagPeek
			} else if discard {
				message.Flags = linuxsockapi.FlagDiscard
			}
			reply, errno, blocked = handler.socketExchange(call, message, socketFill{})
		}
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			handler.socketEnd()
			return socketShort(partial, errno)
		}
		switch reply.Result {
		case linuxsockapi.ResultOK:
		case linuxsockapi.ResultWouldBlock:
			linuxcompat.NoteNet(linuxcompat.NetWouldBlock, 1)
			if result, done := handler.socketStall(call, linuxsockapi.ReadyIn, nonBlocking, partial != 0 && !waitAll, partial, timeout); done {
				return result
			}
			pending = handler.socketPending()
			continue
		default:
			handler.socketEnd()
			return socketShort(partial, linuxcompat.SocketErrno(reply.Result, linuxcompat.SocketCallRead))
		}
		moved := uint64(reply.Length)
		if moved != 0 && !discard {
			copied := handler.deliverRX(call, reply.Slot, transfer, partial, moved)
			releaseRXSlot(call.familyID, call.endpoint, reply.Slot, uint32(moved-copied), uint32(copied))
			if copied == 0 && partial == 0 {
				handler.socketEnd()
				return linuxErrno(linuxabi.EFAULT)
			}
			moved = copied
		}
		partial += moved
		linuxcompat.NoteNet(linuxcompat.NetBytesReceived, moved)
		if call.socket.Datagram {
			handler.socketEnd()
			linuxcompat.NoteNet(linuxcompat.NetDatagramsReceived, 1)
			transfer.source = linuxcompat.SocketAddress{Address: reply.Address, Port: reply.Port}
			transfer.datagramLength, transfer.truncated = reply.Value, reply.Flags&linuxsockapi.FlagTruncated != 0
			if transfer.flags&linuxabi.MessageTrunc != 0 {
				return linuxcompat.Result{Value: reply.Value}
			}
			return linuxcompat.Result{Value: partial}
		}
		if reply.Flags&linuxsockapi.FlagEOF != 0 || reply.Length == 0 || peek || partial >= transfer.limit {
			handler.socketEnd()
			return linuxcompat.Result{Value: partial}
		}
		handler.socketProgress(partial)
		pending = handler.socketPending()
		if !waitAll {
			if state, live := socketReadiness(call); !live || state.Readable == 0 {
				handler.socketEnd()
				return linuxcompat.Result{Value: partial}
			}
		}
	}
	handler.socketEnd()
	return linuxcompat.Result{Value: partial}
}

// socketStall handles a socket that cannot move bytes now. With bytes
// moved and returnShort, or on a nonblocking call, it ends the call; else
// it sleeps until want.
func (handler *linuxCallHandler) socketStall(call *socketCall, want linuxsockapi.Ready, nonBlocking bool, returnShort bool, partial uint64, timeout uint64) (linuxcompat.Result, bool) {
	if returnShort || partial != 0 && nonBlocking {
		handler.socketEnd()
		return linuxcompat.Result{Value: partial}, true
	}
	if nonBlocking {
		handler.socketEnd()
		linuxcompat.NoteNet(linuxcompat.NetWouldBlock, 1)
		return linuxErrno(linuxabi.EAGAIN), true
	}
	handler.socketProgress(partial)
	return handler.socketWait(call, want, handler.socketPending().Note, timeout, partial)
}

// socketShort returns the bytes moved so far, or errno when there are none.
func socketShort(partial uint64, errno linuxabi.Errno) linuxcompat.Result {
	if partial != 0 {
		return linuxcompat.Result{Value: partial}
	}
	return linuxErrno(errno)
}

// deliverRX copies length bytes of an RX slot to the receive's buffer from
// byte position on and returns how many reached it.
func (handler *linuxCallHandler) deliverRX(call *socketCall, slot uint8, transfer *socketTransfer, position uint64, length uint64) uint64 {
	page, found := socketPoolPage(call.endpoint, uint32(linuxsockapi.RXOffset(slot)/vm.PageSize))
	if !found {
		return 0
	}
	length = min(length, uint64(linuxsockapi.SlotSize), transfer.limit-position)
	copied := uint64(0)
	for copied < length {
		address, room := transfer.address+position+copied, length-copied
		if transfer.wire != nil {
			piece, remaining, ok := linuxcompat.VectorPiece(transfer.wire, position+copied)
			if !ok {
				break
			}
			address, room = piece, min(room, remaining)
		}
		room = min(room, vm.PageSize-address%vm.PageSize)
		if !vm.CopyOut(handler.root, address, page[copied:copied+room]) {
			break
		}
		copied += room
	}
	return copied
}

// socketSend sends up to transfer.limit bytes. A blocking stream send waits
// for room until it has sent everything this call may; a nonblocking one
// sends what fits and is EAGAIN only when nothing did. A datagram goes in
// one piece of at most one slot, or fails with EMSGSIZE.
//
//go:noinline
func (handler *linuxCallHandler) socketSend(request linuxcompat.Request, call *socketCall, transfer *socketTransfer, pending linuxcompat.Pending, resumed bool) linuxcompat.Result {
	timeout := call.socket.SendTimeout
	nonBlocking := socketNonBlocking(call, transfer.flags)
	if call.socket.Datagram && transfer.limit > linuxsockapi.SlotSize {
		handler.socketEnd()
		return linuxErrno(linuxabi.EMSGSIZE)
	}
	partial := uint64(0)
	if resumed {
		partial = pending.Partial
	}
	sent := false
	for round := 0; round < linuxSocketRounds; round++ {
		var reply linuxsockapi.Reply
		var errno linuxabi.Errno
		var blocked bool
		if awaiting(pending, resumed) {
			reply, errno, blocked = handler.socketAwait(call)
		} else {
			if woken(pending, resumed) {
				if result, done := handler.socketSlept(pending, timeout, linuxabi.EAGAIN); done {
					return result
				}
				pending = handler.socketPending()
			}
			if partial >= transfer.limit && (sent || !call.socket.Datagram && transfer.limit == 0) {
				handler.socketEnd()
				return linuxcompat.Result{Value: partial}
			}
			if !call.live {
				handler.socketEnd()
				return socketShort(partial, linuxabi.ENETRESET)
			}
			if call.socket.ShutWrite {
				handler.socketEnd()
				return socketShort(partial, linuxabi.EPIPE)
			}
			if !resumed {
				if errno := handler.socketStart(request, socketTimeoutEnd(timeout)); errno != 0 {
					return linuxErrno(errno)
				}
				resumed, pending = true, handler.socketPending()
			}
			if !call.socket.Datagram {
				if state, live := socketReadiness(call); live && state.Ready&(linuxsockapi.ReadyOut|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup) == 0 {
					if result, done := handler.socketStall(call, linuxsockapi.ReadyOut, nonBlocking, false, partial, timeout); done {
						return result
					}
					pending = handler.socketPending()
					continue
				}
			}
			message := socketRequestFor(linuxsockapi.OpWrite, call.socket.Identity)
			if transfer.hasDestination {
				message.Opcode, message.Address, message.Port = linuxsockapi.OpSendTo, transfer.destination.Address, transfer.destination.Port
			}
			fill := socketFill{active: true, address: transfer.address, wire: transfer.wire, position: partial, length: transfer.limit - partial}
			reply, errno, blocked = handler.socketExchange(call, message, fill)
		}
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			handler.socketEnd()
			return socketShort(partial, errno)
		}
		switch reply.Result {
		case linuxsockapi.ResultOK:
		case linuxsockapi.ResultWouldBlock:
			linuxcompat.NoteNet(linuxcompat.NetWouldBlock, 1)
			if result, done := handler.socketStall(call, linuxsockapi.ReadyOut, nonBlocking, false, partial, timeout); done {
				return result
			}
			pending = handler.socketPending()
			continue
		default:
			handler.socketEnd()
			context := linuxcompat.SocketCallWrite
			if call.socket.Datagram {
				context = linuxcompat.SocketCallSend
			}
			return socketShort(partial, linuxcompat.SocketErrno(reply.Result, context))
		}
		partial += uint64(reply.Length)
		sent = true
		linuxcompat.NoteNet(linuxcompat.NetBytesSent, uint64(reply.Length))
		if call.socket.Datagram {
			handler.socketEnd()
			linuxcompat.NoteNet(linuxcompat.NetDatagramsSent, 1)
			return linuxcompat.Result{Value: partial}
		}
		if partial >= transfer.limit {
			handler.socketEnd()
			return linuxcompat.Result{Value: partial}
		}
		handler.socketProgress(partial)
		pending = handler.socketPending()
	}
	handler.socketEnd()
	return linuxcompat.Result{Value: partial}
}
