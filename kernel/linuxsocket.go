//go:build uefi

package main

// The Linux socket calls (ADR 0210): socket, bind, listen, accept and
// accept4, connect, shutdown, getsockname, getpeername, setsockopt and
// getsockopt. The send and receive calls are in linuxsocketio.go, the
// session machinery in linuxsocketsession.go.

import (
	"encoding/binary"

	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	kernelwait "goos/kernel/wait"
	"goos/userspace/linuxsockapi"
)

// linuxListenBacklog is SOMAXCONN, where Linux clamps a backlog.
const linuxListenBacklog = 4096

// Socket serves the socket calls.
//
//go:noinline
func (handler *linuxCallHandler) Socket(request linuxcompat.Request) linuxcompat.Result {
	if handler.state == nil {
		return linuxErrno(linuxabi.EBADF)
	}
	switch request.Number {
	case linuxabi.SysSocket:
		return handler.socketCreate(request)
	case linuxabi.SysConnect:
		return handler.socketConnect(request)
	case linuxabi.SysAccept, linuxabi.SysAccept4:
		return handler.socketAccept(request)
	case linuxabi.SysSendto, linuxabi.SysSendmsg:
		return handler.socketSendCall(request)
	case linuxabi.SysRecvfrom, linuxabi.SysRecvmsg:
		return handler.socketReceiveCall(request)
	case linuxabi.SysShutdown, linuxabi.SysBind, linuxabi.SysListen, linuxabi.SysGetsockname, linuxabi.SysGetpeername:
		return handler.socketControl(request)
	case linuxabi.SysSetsockopt, linuxabi.SysGetsockopt:
		return handler.socketOption(request)
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// socketRequestFor returns the processor's request scratch, cleared.
func socketRequestFor(opcode linuxsockapi.Opcode, socket uint64) *linuxsockapi.Request {
	request := &thisCPU().linuxSocket.request
	*request = linuxsockapi.Request{Opcode: opcode, Socket: socket}
	return request
}

// awaiting reports a continuation with a request in flight.
func awaiting(pending linuxcompat.Pending, resumed bool) bool {
	return resumed && pending.Issued && linuxcompat.SocketRequestCorrelation(pending.Correlation)
}

// woken reports a continuation back from a readiness sleep.
func woken(pending linuxcompat.Pending, resumed bool) bool {
	return resumed && pending.Issued && pending.Correlation == linuxcompat.SocketSleepCorrelation
}

// socketCreate serves socket. Without a network session for the family the
// call fails with ENETDOWN.
//
//go:noinline
func (handler *linuxCallHandler) socketCreate(request linuxcompat.Request) linuxcompat.Result {
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	kind, errno := linuxcompat.DecodeSocket(request.Arguments[0], request.Arguments[1], request.Arguments[2])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	familyID := handler.state.FamilyID
	endpoint, live, _ := linuxcompat.NetSession(familyID)
	if !live {
		handler.socketEnd()
		return linuxErrno(linuxabi.ENETDOWN)
	}
	call := &thisCPU().linuxSocket.call
	*call = socketCall{familyID: familyID, endpoint: endpoint, live: true}
	var reply linuxsockapi.Reply
	var blocked bool
	if awaiting(pending, resumed) {
		reply, errno, blocked = handler.socketAwait(call)
	} else {
		if errno := handler.state.FDs.Reserve(); errno != 0 {
			handler.socketEnd()
			return linuxErrno(errno)
		}
		if !linuxcompat.HasSocketRoom(familyID) {
			handler.socketEnd()
			return linuxErrno(linuxabi.EMFILE)
		}
		if !resumed {
			if errno := handler.socketStart(request, 0); errno != 0 {
				return linuxErrno(errno)
			}
		}
		message := socketRequestFor(linuxsockapi.OpOpen, 0)
		message.Length = linuxsockapi.KindStream
		if kind.Datagram {
			message.Length = linuxsockapi.KindDatagram
		}
		reply, errno, blocked = handler.socketExchange(call, message)
	}
	if blocked {
		return linuxBlocked
	}
	handler.socketEnd()
	if errno != 0 {
		return linuxErrno(errno)
	}
	if reply.Result != linuxsockapi.ResultOK {
		return linuxErrno(linuxcompat.SocketErrno(reply.Result, linuxcompat.SocketCallOpen))
	}
	return handler.installSocket(call, reply.Socket, kind)
}

// installSocket gives a socket the service opened or accepted a description
// and a descriptor. One the family cannot hold is closed again: EMFILE.
func (handler *linuxCallHandler) installSocket(call *socketCall, identity uint64, kind linuxcompat.SocketType) linuxcompat.Result {
	object, created := linuxcompat.NewSocket(call.familyID, call.endpoint.Session, identity, kind.Datagram)
	if !created {
		sendSocketClose(call.familyID, call.endpoint, identity)
		return linuxErrno(linuxabi.EMFILE)
	}
	flags := uint32(0)
	if kind.NonBlock {
		flags = linuxabi.OpenNonBlock
	}
	fd, errno := linuxcompat.AllocateFD(handler.root, linuxcompat.FileDescription{Kind: linuxcompat.FDSocket, Flags: flags, Object: object}, kind.CloseOnExec)
	if errno != 0 {
		releaseSocketDescription(object)
		return linuxErrno(linuxabi.EMFILE)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

// socketAddressIn copies and checks an address a program passes.
func (handler *linuxCallHandler) socketAddressIn(address uint64, length uint64, use linuxcompat.AddressUse) (linuxcompat.SocketAddress, bool, linuxabi.Errno) {
	wire := thisCPU().linuxSocket.address[:]
	size, errno := linuxcompat.SockaddrInputLength(length)
	if errno != 0 {
		return linuxcompat.SocketAddress{}, false, errno
	}
	if size != 0 && !vm.CopyIn(handler.root, address, wire[:size]) {
		return linuxcompat.SocketAddress{}, false, linuxabi.EFAULT
	}
	return linuxcompat.DecodeSockaddrIn(wire[:size], use)
}

// socketAddressOut writes an address the kernel returns: at most the
// caller's length of it, then the full length. optional lets a null
// address skip both, as accept and recvfrom allow.
func (handler *linuxCallHandler) socketAddressOut(address uint64, lengthAddress uint64, value linuxcompat.SocketAddress, actual int, optional bool) linuxabi.Errno {
	scratch := &thisCPU().linuxSocket
	if optional && address == 0 {
		return 0
	}
	if !vm.CopyIn(handler.root, lengthAddress, scratch.word[:]) {
		return linuxabi.EFAULT
	}
	size, errno := linuxcompat.SockaddrOutputLength(binary.LittleEndian.Uint32(scratch.word[:]), actual)
	if errno != 0 {
		return errno
	}
	linuxcompat.EncodeSockaddrIn(&scratch.address, value)
	if size != 0 && !vm.CopyOut(handler.root, address, scratch.address[:size]) {
		return linuxabi.EFAULT
	}
	binary.LittleEndian.PutUint32(scratch.word[:], uint32(actual))
	if !vm.CopyOut(handler.root, lengthAddress, scratch.word[:]) {
		return linuxabi.EFAULT
	}
	return 0
}

// socketControl serves the calls that are one exchange: bind, listen,
// shutdown, getsockname and getpeername.
//
//go:noinline
func (handler *linuxCallHandler) socketControl(request linuxcompat.Request) linuxcompat.Result {
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
	var reply linuxsockapi.Reply
	var blocked bool
	if awaiting(pending, resumed) {
		reply, errno, blocked = handler.socketAwait(call)
	} else {
		message := socketRequestFor(0, call.socket.Identity)
		switch request.Number {
		case linuxabi.SysBind:
			address, _, errno := handler.socketAddressIn(arguments[1], arguments[2], linuxcompat.AddressBind)
			if errno == 0 {
				errno = linuxcompat.BindPortErrno(address.Port)
			}
			if errno != 0 {
				handler.socketEnd()
				return linuxErrno(errno)
			}
			message.Opcode, message.Address, message.Port = linuxsockapi.OpBind, address.Address, address.Port
			if call.socket.ReuseAddr {
				message.Flags = linuxsockapi.FlagReuseAddr
			}
		case linuxabi.SysListen:
			if call.socket.Datagram {
				handler.socketEnd()
				return linuxErrno(linuxabi.EOPNOTSUPP)
			}
			backlog := int32(uint32(arguments[1]))
			if backlog < 0 || backlog > linuxListenBacklog {
				backlog = linuxListenBacklog
			}
			message.Opcode, message.Length = linuxsockapi.OpListen, uint32(backlog)
		case linuxabi.SysShutdown:
			if arguments[1] > linuxabi.ShutdownBoth {
				handler.socketEnd()
				return linuxErrno(linuxabi.EINVAL)
			}
			message.Opcode, message.Length = linuxsockapi.OpShutdown, uint32(arguments[1])
		case linuxabi.SysGetsockname:
			message.Opcode = linuxsockapi.OpGetSockName
		default:
			message.Opcode = linuxsockapi.OpGetPeerName
		}
		if !resumed {
			if errno := handler.socketStart(request, 0); errno != 0 {
				return linuxErrno(errno)
			}
		}
		reply, errno, blocked = handler.socketExchange(call, message)
	}
	if blocked {
		return linuxBlocked
	}
	handler.socketEnd()
	if errno != 0 {
		return linuxErrno(errno)
	}
	return handler.socketControlReply(request, call, reply)
}

//go:noinline
func (handler *linuxCallHandler) socketControlReply(request linuxcompat.Request, call *socketCall, reply linuxsockapi.Reply) linuxcompat.Result {
	arguments := &request.Arguments
	context := linuxcompat.SocketCallName
	switch request.Number {
	case linuxabi.SysBind:
		context = linuxcompat.SocketCallBind
	case linuxabi.SysListen:
		context = linuxcompat.SocketCallListen
	case linuxabi.SysShutdown:
		context = linuxcompat.SocketCallShutdown
	}
	if reply.Result != linuxsockapi.ResultOK {
		return linuxErrno(linuxcompat.SocketErrno(reply.Result, context))
	}
	socket := call.socket
	switch request.Number {
	case linuxabi.SysListen:
		socket.Listening = true
	case linuxabi.SysShutdown:
		socket.ShutRead = socket.ShutRead || arguments[1] != linuxabi.ShutdownWrite
		socket.ShutWrite = socket.ShutWrite || arguments[1] != linuxabi.ShutdownRead
	case linuxabi.SysGetsockname:
		return linuxErrno(handler.socketAddressOut(arguments[1], arguments[2],
			linuxcompat.SocketAddress{Address: reply.Local, Port: reply.LocalPort}, linuxcompat.SockaddrInSize, false))
	case linuxabi.SysGetpeername:
		return linuxErrno(handler.socketAddressOut(arguments[1], arguments[2],
			linuxcompat.SocketAddress{Address: reply.Address, Port: reply.Port}, linuxcompat.SockaddrInSize, false))
	}
	linuxcompat.StoreSocket(call.object, socket)
	return linuxcompat.Result{}
}

// socketTimeoutEnd is the absolute end of a call's SO_RCVTIMEO or
// SO_SNDTIMEO, or zero for none.
func socketTimeoutEnd(timeout uint64) uint64 {
	if timeout == 0 {
		return 0
	}
	return nowNanos() + timeout
}

// socketNonBlocking reports a call that must not wait: an O_NONBLOCK
// description or MSG_DONTWAIT.
func socketNonBlocking(call *socketCall, flags uint64) bool {
	return call.description.Flags&linuxabi.OpenNonBlock != 0 || flags&linuxabi.MessageDontWait != 0
}

// socketSlept handles the wake of a readiness sleep. done reports that
// result ends the call: a handled signal, or the timeout, which answers
// timedOut, or the bytes moved so far when there are some.
func (handler *linuxCallHandler) socketSlept(pending linuxcompat.Pending, timeout uint64, timedOut linuxabi.Errno) (linuxcompat.Result, bool) {
	handler.socketProgress(pending.Partial)
	switch pending.Wake {
	case linuxcompat.WakeInterrupted:
		handler.socketEnd()
		linuxcompat.NoteNet(linuxcompat.NetInterrupted, 1)
		if pending.Partial != 0 {
			return linuxcompat.Result{Value: pending.Partial}, true
		}
		return handler.interrupted(timeout == 0), true
	case linuxcompat.WakeTimeout:
		handler.socketEnd()
		if pending.Partial != 0 {
			return linuxcompat.Result{Value: pending.Partial}, true
		}
		return linuxErrno(timedOut), true
	}
	return linuxcompat.Result{}, false
}

// socketWait sleeps a call until want. done reports that result ends the
// call; otherwise readiness changed already and the call goes on.
func (handler *linuxCallHandler) socketWait(call *socketCall, want linuxsockapi.Ready, end uint64, timeout uint64, partial uint64) (linuxcompat.Result, bool) {
	switch handler.socketSleep(call, want, end) {
	case sleepBlocked:
		return linuxBlocked, true
	case sleepSignalled:
		handler.socketEnd()
		linuxcompat.NoteNet(linuxcompat.NetInterrupted, 1)
		if partial != 0 {
			return linuxcompat.Result{Value: partial}, true
		}
		return handler.interrupted(timeout == 0), true
	case sleepFailed:
		handler.socketEnd()
		return linuxErrno(linuxabi.ENOMEM), true
	}
	return linuxcompat.Result{}, false
}

// connectPhase is the partial count of a connect: whether the service
// already answered InProgress, so the call waits for the outcome.
const connectWaiting = 1

// socketConnect serves connect. A stream connect that cannot finish at once
// is EINPROGRESS on a nonblocking socket; a blocking one waits for the
// socket to turn writable, then reads the outcome with GetPendingError, as
// SO_ERROR does. SO_SNDTIMEO bounds that wait and ends it with EINPROGRESS.
//
//go:noinline
func (handler *linuxCallHandler) socketConnect(request linuxcompat.Request) linuxcompat.Result {
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call, errno := handler.socketDescriptor(request.Arguments[0])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	timeout := call.socket.SendTimeout
	if !resumed && call.socket.Connected && !call.socket.Datagram {
		handler.socketEnd()
		return linuxErrno(linuxabi.EISCONN)
	}
	if !resumed && call.socket.Connecting {
		// A connect already under way: once it finished, this one reports
		// its outcome, 0 or the error, and a later one EISCONN; before
		// that a nonblocking one is EALREADY and a blocking one waits.
		if state, live := socketReadiness(call); live && state.Ready&(linuxsockapi.ReadyOut|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup) == 0 && socketNonBlocking(call, 0) {
			handler.socketEnd()
			return linuxErrno(linuxabi.EALREADY)
		}
		if errno := handler.socketStart(request, socketTimeoutEnd(timeout)); errno != 0 {
			return linuxErrno(errno)
		}
		handler.socketProgress(connectWaiting)
		pending, resumed = handler.socketPending(), true
	}
	for round := 0; round < 4; round++ {
		var reply linuxsockapi.Reply
		var blocked bool
		switch {
		case awaiting(pending, resumed):
			reply, errno, blocked = handler.socketAwait(call)
		case woken(pending, resumed) || resumed && pending.Partial == connectWaiting:
			if woken(pending, resumed) {
				if result, done := handler.socketSlept(pending, timeout, linuxabi.EINPROGRESS); done {
					return result
				}
			}
			state, live := socketReadiness(call)
			if live && state.Ready&(linuxsockapi.ReadyOut|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup) == 0 {
				if result, done := handler.socketWait(call, linuxsockapi.ReadyOut, pending.Note, timeout, 0); done {
					return result
				}
				state, live = socketReadiness(call)
			}
			if live && state.State&linuxsockapi.EntrySend != 0 && state.Error == linuxsockapi.ResultOK && state.Ready&linuxsockapi.ReadyError == 0 {
				// The entry shows the stream established with no error, all
				// SO_ERROR would say.
				handler.socketEnd()
				socket := call.socket
				socket.Connecting, socket.Connected = false, true
				linuxcompat.StoreSocket(call.object, socket)
				linuxcompat.NoteNet(linuxcompat.NetConnects, 1)
				return linuxcompat.Result{}
			}
			reply, errno, blocked = handler.socketExchange(call, socketRequestFor(linuxsockapi.OpGetPendingError, call.socket.Identity))
		default:
			address, unspecified, errno := handler.socketAddressIn(request.Arguments[1], request.Arguments[2], linuxcompat.AddressConnect)
			if errno == 0 && unspecified {
				// Dissolving an association has no GLNXSOCK2 operation.
				errno = linuxabi.EOPNOTSUPP
			}
			if errno == 0 && address.Port == 0 {
				errno = linuxabi.ECONNREFUSED
				if call.socket.Datagram {
					errno = linuxabi.EINVAL
				}
			}
			if errno != 0 {
				handler.socketEnd()
				return linuxErrno(errno)
			}
			if !resumed {
				if errno := handler.socketStart(request, socketTimeoutEnd(timeout)); errno != 0 {
					return linuxErrno(errno)
				}
			}
			message := socketRequestFor(linuxsockapi.OpConnect, call.socket.Identity)
			message.Address, message.Port = address.Address, address.Port
			reply, errno, blocked = handler.socketExchange(call, message)
		}
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			handler.socketEnd()
			return linuxErrno(errno)
		}
		socket := call.socket
		switch {
		case reply.Opcode == linuxsockapi.OpGetPendingError:
			handler.socketEnd()
			socket.Connecting = false
			linuxcompat.StoreSocket(call.object, socket)
			if outcome := linuxsockapi.Result(reply.Value); outcome != linuxsockapi.ResultOK {
				return linuxErrno(linuxcompat.SocketErrno(outcome, linuxcompat.SocketCallConnect))
			}
			socket.Connected = !socket.Datagram
			linuxcompat.StoreSocket(call.object, socket)
			linuxcompat.NoteNet(linuxcompat.NetConnects, 1)
			return linuxcompat.Result{}
		case reply.Result == linuxsockapi.ResultInProgress && !call.socket.Datagram:
			socket.Connecting = true
			linuxcompat.StoreSocket(call.object, socket)
			if socketNonBlocking(call, 0) {
				handler.socketEnd()
				return linuxErrno(linuxabi.EINPROGRESS)
			}
			handler.socketProgress(connectWaiting)
			pending, resumed = handler.socketPending(), true
			continue
		}
		handler.socketEnd()
		socket.Connecting = false
		linuxcompat.StoreSocket(call.object, socket)
		if reply.Result != linuxsockapi.ResultOK {
			return linuxErrno(linuxcompat.SocketErrno(reply.Result, linuxcompat.SocketCallConnect))
		}
		if !socket.Datagram {
			socket.Connected = true
			linuxcompat.StoreSocket(call.object, socket)
		}
		linuxcompat.NoteNet(linuxcompat.NetConnects, 1)
		return linuxcompat.Result{}
	}
	handler.socketEnd()
	return linuxErrno(linuxabi.EIO)
}

// socketAccept serves accept and accept4. A descriptor and a socket slot
// are checked first, so a full table is EMFILE before any child is taken.
// A child that arrives is installed before the call returns; the reply
// wait it arrives in is not interrupted, so no child goes missing.
//
//go:noinline
func (handler *linuxCallHandler) socketAccept(request linuxcompat.Request) linuxcompat.Result {
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	kind := linuxcompat.SocketType{}
	if request.Number == linuxabi.SysAccept4 {
		var errno linuxabi.Errno
		if kind, errno = linuxcompat.AcceptFlags(request.Arguments[3]); errno != 0 {
			handler.socketEnd()
			return linuxErrno(errno)
		}
	}
	call, errno := handler.socketDescriptor(request.Arguments[0])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	if call.socket.Datagram {
		handler.socketEnd()
		return linuxErrno(linuxabi.EOPNOTSUPP)
	}
	if !call.socket.Listening {
		handler.socketEnd()
		return linuxErrno(linuxabi.EINVAL)
	}
	timeout := call.socket.RecvTimeout
	for round := 0; round < 4; round++ {
		var reply linuxsockapi.Reply
		var blocked bool
		if awaiting(pending, resumed) {
			reply, errno, blocked = handler.socketAwait(call)
		} else {
			if woken(pending, resumed) {
				if result, done := handler.socketSlept(pending, timeout, linuxabi.EAGAIN); done {
					return result
				}
			}
			if errno := handler.state.FDs.Reserve(); errno != 0 {
				handler.socketEnd()
				return linuxErrno(errno)
			}
			if !linuxcompat.HasSocketRoom(call.familyID) {
				handler.socketEnd()
				return linuxErrno(linuxabi.EMFILE)
			}
			if !resumed {
				if errno := handler.socketStart(request, socketTimeoutEnd(timeout)); errno != 0 {
					return linuxErrno(errno)
				}
				resumed = true
			}
			reply, errno, blocked = handler.socketExchange(call, socketRequestFor(linuxsockapi.OpAccept, call.socket.Identity))
		}
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			handler.socketEnd()
			return linuxErrno(errno)
		}
		if reply.Result == linuxsockapi.ResultWouldBlock {
			linuxcompat.NoteNet(linuxcompat.NetWouldBlock, 1)
			if socketNonBlocking(call, 0) {
				handler.socketEnd()
				return linuxErrno(linuxabi.EAGAIN)
			}
			if result, done := handler.socketWait(call, linuxsockapi.ReadyIn, handler.socketPending().Note, timeout, 0); done {
				return result
			}
			pending = handler.socketPending()
			continue
		}
		handler.socketEnd()
		if reply.Result != linuxsockapi.ResultOK {
			return linuxErrno(linuxcompat.SocketErrno(reply.Result, linuxcompat.SocketCallAccept))
		}
		installed := handler.installSocket(call, reply.Value, kind)
		if installed.Errno != 0 {
			return installed
		}
		linuxcompat.NoteNet(linuxcompat.NetAccepts, 1)
		// The descriptor is the program's now; Linux reports a bad address
		// buffer after taking the connection, and so does this.
		if errno := handler.socketAddressOut(request.Arguments[1], request.Arguments[2],
			linuxcompat.SocketAddress{Address: reply.Address, Port: reply.Port}, linuxcompat.SockaddrInSize, true); errno != 0 {
			return linuxErrno(errno)
		}
		return installed
	}
	handler.socketEnd()
	return linuxErrno(linuxabi.EIO)
}

// socketOption serves setsockopt and getsockopt.
//
//go:noinline
func (handler *linuxCallHandler) socketOption(request linuxcompat.Request) linuxcompat.Result {
	scratch := &thisCPU().linuxSocket
	pending, resumed, result, done := handler.socketResume(request)
	if done {
		return result
	}
	call, errno := handler.socketDescriptor(request.Arguments[0])
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	set := request.Number == linuxabi.SysSetsockopt
	level, name := request.Arguments[1], request.Arguments[2]
	place, errno := linuxcompat.ClassifyOption(level, name, call.socket.Datagram, set)
	if errno != 0 {
		handler.socketEnd()
		return linuxErrno(errno)
	}
	value := uint64(0)
	if set {
		size, errno := linuxcompat.OptionInputLength(request.Arguments[4])
		if errno == 0 && size != 0 && !vm.CopyIn(handler.root, request.Arguments[3], scratch.option[:size]) {
			errno = linuxabi.EFAULT
		}
		if errno == 0 {
			value, errno = handler.setKernelOption(call, place, name, scratch.option[:size])
		}
		if errno != 0 || place == linuxcompat.OptionKernel {
			handler.socketEnd()
			return linuxErrno(errno)
		}
	} else if place == linuxcompat.OptionKernel {
		handler.socketEnd()
		return handler.getKernelOption(request, call, name)
	}
	var reply linuxsockapi.Reply
	var blocked bool
	if awaiting(pending, resumed) {
		reply, errno, blocked = handler.socketAwait(call)
	} else {
		message := socketRequestFor(linuxsockapi.OpGetPendingError, call.socket.Identity)
		if place == linuxcompat.OptionService {
			message.Opcode, message.Level, message.Name = linuxsockapi.OpGetOption, uint32(level), uint32(name)
			if set {
				message.Opcode, message.Value = linuxsockapi.OpSetOption, value
			}
		}
		if !resumed {
			if errno := handler.socketStart(request, 0); errno != 0 {
				return linuxErrno(errno)
			}
		}
		reply, errno, blocked = handler.socketExchange(call, message)
	}
	if blocked {
		return linuxBlocked
	}
	handler.socketEnd()
	if errno == linuxabi.ENETRESET && place == linuxcompat.OptionPendingError {
		// A socket of a failed session reports the failure as SO_ERROR.
		return handler.putOptionInt(request, int32(linuxabi.ENETRESET))
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if reply.Result != linuxsockapi.ResultOK {
		return linuxErrno(linuxcompat.SocketErrno(reply.Result, linuxcompat.SocketCallOption))
	}
	if set {
		return linuxcompat.Result{}
	}
	if place == linuxcompat.OptionPendingError {
		return handler.putOptionInt(request, int32(linuxcompat.SocketErrno(linuxsockapi.Result(reply.Value), linuxcompat.SocketCallConnect)))
	}
	return handler.putOptionInt(request, int32(reply.Value))
}

// setKernelOption applies an option the kernel keeps and returns the value
// a service option carries.
//
//go:noinline
func (handler *linuxCallHandler) setKernelOption(call *socketCall, place linuxcompat.OptionPlace, name uint64, wire []byte) (uint64, linuxabi.Errno) {
	socket := call.socket
	switch {
	case place == linuxcompat.OptionService:
		value, errno := linuxcompat.DecodeOptionInt(wire)
		return uint64(uint32(value)), errno
	case name == linuxabi.OptionRecvTimeout || name == linuxabi.OptionSendTimeout:
		timeout, errno := linuxcompat.DecodeTimeout(wire)
		if errno != 0 {
			return 0, errno
		}
		if name == linuxabi.OptionRecvTimeout {
			socket.RecvTimeout = timeout
		} else {
			socket.SendTimeout = timeout
		}
	case name == linuxabi.OptionLinger:
		off, errno := linuxcompat.DecodeLingerOff(wire)
		if errno != 0 {
			return 0, errno
		}
		if !off {
			return 0, linuxabi.ENOPROTOOPT
		}
		return 0, 0
	case linuxcompat.OffOption(name):
		value, errno := linuxcompat.DecodeOptionInt(wire)
		if errno == 0 && value != 0 {
			errno = linuxabi.ENOPROTOOPT
		}
		return 0, errno
	default:
		value, errno := linuxcompat.DecodeOptionInt(wire)
		if errno != 0 {
			return 0, errno
		}
		socket.ReuseAddr = value != 0
	}
	linuxcompat.StoreSocket(call.object, socket)
	return 0, 0
}

// getKernelOption answers an option from the socket table.
//
//go:noinline
func (handler *linuxCallHandler) getKernelOption(request linuxcompat.Request, call *socketCall, name uint64) linuxcompat.Result {
	scratch := &thisCPU().linuxSocket
	socket := call.socket
	switch name {
	case linuxabi.OptionType:
		if socket.Datagram {
			return handler.putOptionInt(request, int32(linuxabi.SocketDatagram))
		}
		return handler.putOptionInt(request, int32(linuxabi.SocketStream))
	case linuxabi.OptionDomain:
		return handler.putOptionInt(request, int32(linuxabi.FamilyInet))
	case linuxabi.OptionProtocol:
		if socket.Datagram {
			return handler.putOptionInt(request, int32(linuxabi.ProtocolUDP))
		}
		return handler.putOptionInt(request, int32(linuxabi.ProtocolTCP))
	case linuxabi.OptionAcceptConn:
		return handler.putOptionInt(request, optionBool(socket.Listening))
	case linuxabi.OptionReuseAddr:
		return handler.putOptionInt(request, optionBool(socket.ReuseAddr))
	case linuxabi.OptionRecvTimeout, linuxabi.OptionSendTimeout:
		timeout := socket.RecvTimeout
		if name == linuxabi.OptionSendTimeout {
			timeout = socket.SendTimeout
		}
		var wire [linuxcompat.TimevalSize]byte
		linuxcompat.EncodeTimeout(&wire, timeout)
		copy(scratch.option[:], wire[:])
		return handler.putOption(request, scratch.option[:linuxcompat.TimevalSize])
	case linuxabi.OptionLinger:
		clear(scratch.option[:8])
		return handler.putOption(request, scratch.option[:8])
	}
	return handler.putOptionInt(request, 0)
}

func optionBool(value bool) int32 {
	if value {
		return 1
	}
	return 0
}

func (handler *linuxCallHandler) putOptionInt(request linuxcompat.Request, value int32) linuxcompat.Result {
	scratch := &thisCPU().linuxSocket
	binary.LittleEndian.PutUint32(scratch.option[:], uint32(value))
	return handler.putOption(request, scratch.option[:4])
}

// putOption writes a getsockopt answer, cut to the caller's length, and the
// length written.
func (handler *linuxCallHandler) putOption(request linuxcompat.Request, value []byte) linuxcompat.Result {
	scratch := &thisCPU().linuxSocket
	if !vm.CopyIn(handler.root, request.Arguments[4], scratch.word[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	size, errno := linuxcompat.OptionOutputLength(binary.LittleEndian.Uint32(scratch.word[:]), len(value))
	if errno != 0 {
		return linuxErrno(errno)
	}
	if size != 0 && !vm.CopyOut(handler.root, request.Arguments[3], value[:size]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	binary.LittleEndian.PutUint32(scratch.word[:], uint32(size))
	if !vm.CopyOut(handler.root, request.Arguments[4], scratch.word[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

// socketIoctl serves the socket ioctls: FIONREAD from the readiness page,
// FIONBIO, and SIOCATMARK, always false since no urgent data exists. A
// device request (0x89xx) is EOPNOTSUPP, every other one ENOTTY, as Linux
// answers a request the socket layer does not know.
//
//go:noinline
func (handler *linuxCallHandler) socketIoctl(description *linuxcompat.FileDescription, command uint64, argument uint64) linuxcompat.Result {
	scratch := &thisCPU().linuxSocket
	call := &scratch.call
	if errno := handler.socketOf(call, description); errno != 0 {
		return linuxErrno(errno)
	}
	switch command {
	case linuxabi.IoctlFIONREAD:
		pumpSocketSession(call.familyID, call.endpoint, handler.task)
		refreshSocket(call)
		state, live := socketReadiness(call)
		if !live {
			return linuxErrno(linuxabi.ENETRESET)
		}
		binary.LittleEndian.PutUint32(scratch.word[:], state.Readable)
	case linuxabi.IoctlFIONBIO:
		if !vm.CopyIn(handler.root, argument, scratch.word[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		if binary.LittleEndian.Uint32(scratch.word[:]) != 0 {
			description.Flags |= linuxabi.OpenNonBlock
		} else {
			description.Flags &^= linuxabi.OpenNonBlock
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlSIOCATMARK:
		binary.LittleEndian.PutUint32(scratch.word[:], 0)
	default:
		if command&0xff00 == 0x8900 {
			return linuxErrno(linuxabi.EOPNOTSUPP)
		}
		return linuxErrno(linuxabi.ENOTTY)
	}
	if !vm.CopyOut(handler.root, argument, scratch.word[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

// pollSocket evaluates a socket for poll from the readiness page after
// draining the session. watch reports the channel wait that wakes a poll
// on it.
//
//go:noinline
func pollSocket(description linuxcompat.FileDescription, events uint16) (uint16, kernelwait.Entry, bool) {
	scratch := &thisCPU().linuxSocket
	call := &scratch.call
	socket, familyID, live, found := linuxcompat.SocketOf(description.Object)
	if !found {
		return linuxcompat.PollInvalid, kernelwait.Entry{}, false
	}
	endpoint, sessionLive, _ := linuxcompat.NetSession(familyID)
	*call = socketCall{familyID: familyID, endpoint: endpoint, object: description.Object, socket: socket,
		live: live && sessionLive && endpoint.Session == socket.Session}
	if call.live {
		pumpSocketSession(familyID, endpoint, thisCPU().linuxHandler.task)
		refreshSocket(call)
	}
	state, ready := socketReadiness(call)
	revents := linuxcompat.SocketPollRevents(events, state.Ready, ready, call.socket.ShutRead)
	if revents != 0 || !ready {
		return revents, kernelwait.Entry{}, false
	}
	return 0, kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindChannel, Object: endpoint.Channel, Side: endpoint.Side},
		Events: kernelwait.Readable | kernelwait.PeerClosed}, true
}

// rearmSocketInterests closes the gap between a poll's scan and its
// registration: a session message queued in between published its event to
// nobody, so it is published again for the registration just made.
func rearmSocketInterests(entries []kernelwait.Entry) {
	for index := range entries {
		key := entries[index].Key
		if key.Kind != kernelwait.KindChannel {
			continue
		}
		channel, live := channelRegistry.Acquire(key.Object)
		if !live {
			continue
		}
		status := channel.ReceiveStatus(key.Side)
		channel.Unlock()
		if status == ipc.StatusOK {
			publishWaitAny(key, kernelwait.Readable)
		} else if status != ipc.StatusNotFound {
			publishWaitAny(key, kernelwait.PeerClosed)
		}
	}
}
