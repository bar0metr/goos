//go:build uefi

package main

// The kernel's data plane of a GLNXSOCK2 session (ADR 0210): copying a send
// into TX slots and committing them, taking received bytes from the RX
// slots a socket has queued, draining the complete ring, and ringing the
// service's doorbell. The ownership rules are in compat/linux/socketdata.go.

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/userspace/linuxsockapi"
)

// drainSocketData takes the complete ring of a session. It reports false
// once the session failed.
func drainSocketData(familyID uint64, endpoint linuxcompat.NetEndpoint) bool {
	rings, found := socketPoolPage(endpoint, linuxsockapi.RingPage)
	if !found {
		failSocketSession(familyID, endpoint, socketFailedRing)
		return false
	}
	if !linuxsockapi.CompletePending(rings) {
		return true
	}
	doorbell, status := linuxcompat.DrainComplete(familyID, endpoint.Session, rings)
	if status == linuxcompat.DataBroken {
		failSocketSession(familyID, endpoint, socketFailedRing)
		return false
	}
	if doorbell {
		ringSocketDoorbell(endpoint, rings)
	}
	return true
}

// releaseSocketOrphans hands back RX slots of sockets the family does not
// know, after a drain of the channel. It reports false once the session
// failed.
func releaseSocketOrphans(familyID uint64, endpoint linuxcompat.NetEndpoint) bool {
	rings, found := socketPoolPage(endpoint, linuxsockapi.RingPage)
	if !found {
		failSocketSession(familyID, endpoint, socketFailedRing)
		return false
	}
	doorbell, status := linuxcompat.ReleaseOrphans(familyID, endpoint.Session, rings)
	if status == linuxcompat.DataBroken {
		failSocketSession(familyID, endpoint, socketFailedRing)
		return false
	}
	if doorbell {
		ringSocketDoorbell(endpoint, rings)
	}
	return true
}

// ringSocketDoorbell sends the Doorbell notification a claim made due. One
// that finds the queue full withdraws the claim: the service drains the
// submit ring before each message it takes, and the queue holds some.
func ringSocketDoorbell(endpoint linuxcompat.NetEndpoint, rings []byte) {
	message := &thisCPU().linuxSocket.bell
	request := linuxsockapi.Request{Opcode: linuxsockapi.OpDoorbell}
	if !linuxsockapi.EncodeRequest(&request, 0, message) {
		linuxsockapi.DropDoorbell(rings)
		return
	}
	if channelSend(endpoint.Channel, endpoint.Side, message, nil, nil, nil) != ipc.StatusOK {
		linuxsockapi.DropDoorbell(rings)
		return
	}
	linuxcompat.NoteNet(linuxcompat.NetDoorbells, 1)
	wakeIPCWaiters(endpoint.Channel, endpoint.Side^1, ipc.WaitMessage)
}

// userPiece returns where byte position of a transfer lives in the
// program and how many bytes follow it there.
func userPiece(transfer *socketTransfer, position uint64) (uint64, uint64, bool) {
	if transfer.wire != nil {
		return linuxcompat.VectorPiece(transfer.wire, position)
	}
	return transfer.address + position, transfer.limit - position, true
}

// fillSlot copies length bytes of a send, from byte position of the
// transfer on, into TX slot. It stops at the first byte the program cannot
// read and returns how many it copied.
func (handler *linuxCallHandler) fillSlot(call *socketCall, slot uint8, transfer *socketTransfer, position uint64, length uint64) uint64 {
	copied := uint64(0)
	for copied < length {
		page, found := socketPoolPage(call.endpoint, linuxsockapi.TXPage(slot, int(copied/vm.PageSize)))
		if !found {
			break
		}
		address, room, ok := userPiece(transfer, position+copied)
		if !ok {
			break
		}
		within := copied % vm.PageSize
		room = min(room, length-copied, vm.PageSize-address%vm.PageSize, vm.PageSize-within)
		if !vm.CopyIn(handler.root, address, page[within:within+room]) {
			break
		}
		copied += room
	}
	linuxcompat.NoteNet(linuxcompat.NetBytesToPool, copied)
	return copied
}

// deliverSlot copies length bytes of RX slot from offset on to the receive
// buffer at byte position, and returns how many reached it.
func (handler *linuxCallHandler) deliverSlot(call *socketCall, slot uint8, offset uint64, transfer *socketTransfer, position uint64, length uint64) uint64 {
	copied := uint64(0)
	for copied < length {
		at := offset + copied
		page, found := socketPoolPage(call.endpoint, linuxsockapi.RXPage(slot, int(at/vm.PageSize)))
		if !found {
			break
		}
		address, room, ok := userPiece(transfer, position+copied)
		if !ok {
			break
		}
		within := at % vm.PageSize
		room = min(room, length-copied, vm.PageSize-address%vm.PageSize, vm.PageSize-within)
		if !vm.CopyOut(handler.root, address, page[within:within+room]) {
			break
		}
		copied += room
	}
	linuxcompat.NoteNet(linuxcompat.NetBytesFromPool, copied)
	return copied
}

// takeRX moves what a socket has queued into the receive buffer, from
// byte position on: stream bytes from the oldest slot on, or one datagram.
// took reports that something was taken, which an empty datagram is.
func (handler *linuxCallHandler) takeRX(call *socketCall, transfer *socketTransfer, position uint64, peek bool, discard bool) (moved uint64, took bool, errno linuxabi.Errno) {
	scratch := &thisCPU().linuxSocket
	count, _ := linuxcompat.RXQueue(call.familyID, call.endpoint.Session, call.socket.Identity, &scratch.pieces)
	if count == 0 {
		return 0, false, 0
	}
	rings, found := socketPoolPage(call.endpoint, linuxsockapi.RingPage)
	if !found {
		return 0, false, linuxabi.ENETRESET
	}
	doorbell := false
	if call.socket.Datagram {
		piece := &scratch.pieces[0]
		want := min(uint64(piece.Length), transfer.limit)
		copied := handler.deliverSlot(call, piece.Slot, 0, transfer, 0, want)
		if copied < want {
			return 0, false, linuxabi.EFAULT
		}
		transfer.source = linuxcompat.SocketAddress{Address: piece.Address, Port: piece.Port}
		transfer.datagramLength, transfer.truncated = uint64(piece.Length), uint64(piece.Length) > transfer.limit
		if !peek && !consumeRX(call, rings, piece, uint64(piece.Length), true, &doorbell) {
			return 0, false, linuxabi.ENETRESET
		}
		if doorbell {
			ringSocketDoorbell(call.endpoint, rings)
		}
		return copied, true, 0
	}
	for index := 0; index < count && position+moved < transfer.limit; index++ {
		piece := &scratch.pieces[index]
		want := min(uint64(piece.Length-piece.Offset), transfer.limit-position-moved)
		copied := want
		if !discard {
			copied = handler.deliverSlot(call, piece.Slot, uint64(piece.Offset), transfer, position+moved, want)
		}
		if !peek && copied != 0 && !consumeRX(call, rings, piece, copied, false, &doorbell) {
			return moved, moved != 0, linuxabi.ENETRESET
		}
		moved += copied
		if copied < want {
			if moved == 0 && position == 0 {
				errno = linuxabi.EFAULT
			}
			break
		}
	}
	if doorbell {
		ringSocketDoorbell(call.endpoint, rings)
	}
	return moved, moved != 0, errno
}

// consumeRX records what a read took of a queued slot. It reports false
// once the session failed.
func consumeRX(call *socketCall, rings []byte, piece *linuxcompat.RXPiece, n uint64, whole bool, doorbell *bool) bool {
	bell, status := linuxcompat.RXConsume(call.familyID, call.endpoint.Session, rings, piece.Slot, uint32(n), whole)
	if status == linuxcompat.DataBroken {
		failSocketSession(call.familyID, call.endpoint, socketFailedRing)
		return false
	}
	*doorbell = *doorbell || bell
	return true
}

// commitTX copies the next piece of a send into a TX slot and commits it.
// status DataNone means the socket has no slot credit now.
func (handler *linuxCallHandler) commitTX(call *socketCall, transfer *socketTransfer, position uint64) (uint64, linuxcompat.DataStatus, linuxabi.Errno) {
	slot, status := linuxcompat.TXReserve(call.familyID, call.endpoint.Session, call.socket.Identity)
	if status != linuxcompat.DataOK {
		return 0, status, 0
	}
	length := min(transfer.limit-position, uint64(linuxsockapi.SlotSize))
	copied := handler.fillSlot(call, slot, transfer, position, length)
	if copied == 0 && length != 0 || call.socket.Datagram && copied < length {
		linuxcompat.TXAbort(call.familyID, slot)
		return 0, linuxcompat.DataOK, linuxabi.EFAULT
	}
	rings, found := socketPoolPage(call.endpoint, linuxsockapi.RingPage)
	if !found {
		linuxcompat.TXAbort(call.familyID, slot)
		return 0, linuxcompat.DataBroken, linuxabi.ENETRESET
	}
	doorbell, status := linuxcompat.TXCommit(call.familyID, call.endpoint.Session, rings, slot, uint32(copied),
		transfer.hasDestination && call.socket.Datagram, transfer.destination.Address, transfer.destination.Port)
	if status != linuxcompat.DataOK {
		failSocketSession(call.familyID, call.endpoint, socketFailedRing)
		return 0, linuxcompat.DataBroken, linuxabi.ENETRESET
	}
	if doorbell {
		ringSocketDoorbell(call.endpoint, rings)
	}
	return copied, linuxcompat.DataOK, 0
}
