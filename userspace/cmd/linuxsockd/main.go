//go:build goos_userspace

// Command linuxsockd keeps the Linux socket objects of every Linux family
// (ADR 0210). The kernel talks GLNXSOCK3 to it over one family session per
// Linux launch; it keeps each socket over a native TCP or UDP endpoint in
// netd. PID 1 hands it the family sessions and the native sessions over
// GLNXCTL2. It holds no VFS, configuration, device or launch authority.
//
// The logic is userspace/linuxsock. This file owns the handles: the control
// channel, each family's channel and mapped pool, and the event loop.
package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/linuxsock"
	"goos/userspace/linuxsockapi"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

const exitFailed = 1

// familyLink is the service's end of one family session.
type familyLink struct {
	channel  handle.ID
	pool     handle.ID
	poolBase uint64
	poolSize uint64
}

var (
	consoleHandle, controlHandle handle.ID

	core     linuxsock.Service
	families [linuxsock.MaxFamilies]familyLink

	interests     [kcall.WaitAnyMaxObjects]sys.Interest
	interestOwner [kcall.WaitAnyMaxObjects]int

	wire         [ipc.WireMaxSize]byte
	message      ipc.Message
	reply        ipc.Message
	control      linuxsockapi.ControlRequest
	controlReply linuxsockapi.ControlReply
)

// The interest owners besides family indexes.
const (
	ownerControl = -1
	ownerTCP     = -2
	ownerUDP     = -3
)

// Each family takes one interest, plus control and the two native sessions.
var _ [kcall.WaitAnyMaxObjects - (linuxsock.MaxFamilies + 3)]byte

//export userspaceMain
func userspaceMain() {
	arena.InitService()
	arena.OnExhausted(func() { fatal("LINUXSOCKD_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("LINUXSOCKD_STARTUP_INVALID\n")
	}
	consoleHandle, _ = data.Handle("console")
	syslog.Init(consoleHandle, "linuxsockd")
	if controlHandle, ok = data.Handle("control"); !ok {
		fatal("LINUXSOCKD_CONTROL_MISSING\n")
	}
	core.Init(backend{}, transport{})
	core.SetStreamRing(true)
	core.SetWarm(linuxsock.MaxWarm)
	core.SetLinkView(&tcpRing)
	mark("LINUXSOCKD_STARTED_OK\n")
	serve()
}

func serve() {
	for {
		// The rings first: whatever the TCP session published must be taken
		// before linuxsockd sleeps, and its doorbell armed.
		timeout := kcall.WaitAnyInfiniteTimeout
		if settleRing() {
			// Release entries wait for ring room; netd says nothing when it
			// takes entries, so come back after a tick.
			timeout = 1
		}
		count := buildInterests()
		result, status := userwait.Any(&interests, count, timeout)
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK {
			fatal("LINUXSOCKD_WAIT_FAILED\n")
		}
		for index := 0; index < count; index++ {
			if result.ReadyMask&(1<<index) == 0 {
				continue
			}
			switch owner := interestOwner[index]; owner {
			case ownerControl:
				serviceControl()
			case ownerTCP:
				serviceLink(linuxsock.TCP)
			case ownerUDP:
				serviceLink(linuxsock.UDP)
			default:
				serviceFamily(owner)
			}
		}
		if validation.Enabled && core.ProgressHoles() != 0 {
			fatal("LINUXSOCKD_PROGRESS_HOLE\n")
		}
	}
}

// buildInterests waits on a family's channel for reading while it has room
// for a request, for writing while it holds messages a full queue refused,
// and always for its peer's end.
func buildInterests() int {
	count := 0
	interests[count] = sys.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	interestOwner[count] = ownerControl
	count++
	for _, proto := range [...]linuxsock.Proto{linuxsock.TCP, linuxsock.UDP} {
		if links[proto].channel != 0 {
			interests[count] = sys.Interest{Handle: links[proto].channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			if links[proto].wantsWritable() {
				interests[count].Events |= kcall.WaitWritable
			}
			interestOwner[count] = ownerTCP
			if proto == linuxsock.UDP {
				interestOwner[count] = ownerUDP
			}
			count++
		}
	}
	for index := range families {
		if families[index].channel == 0 {
			continue
		}
		events := kcall.WaitPeerClosed
		if core.CanReceive(index) {
			events |= kcall.WaitReadable
		}
		if core.Pending(index) {
			events |= kcall.WaitWritable
		}
		interests[count] = sys.Interest{Handle: families[index].channel, Events: events}
		interestOwner[count] = index
		count++
	}
	return count
}

func receiveOn(channel handle.ID) kcall.Status {
	status := sys.IPCReceive(channel, wire[:])
	if status != kcall.StatusOK {
		return status
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &message) {
		return kcall.StatusCorruptData
	}
	return kcall.StatusOK
}

func sendOn(channel handle.ID, value *ipc.Message) kcall.Status {
	size, ok := ipc.EncodeFrom(value, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	return sys.IPCSend(channel, wire[:size])
}

func closeAttached() {
	for _, attached := range message.Handles[:message.HandleCount] {
		if attached != 0 {
			sys.HandleClose(attached)
		}
	}
}

// --- families ----------------------------------------------------------------

// serviceFamily sends what the family's queue held back, then reads its
// requests while it has room for them. A peer that has gone takes the
// family with it.
func serviceFamily(index int) {
	link := &families[index]
	core.Flush(index)
	for link.channel != 0 && core.CanReceive(index) {
		status := receiveOn(link.channel)
		if status == kcall.StatusNotFound {
			break
		}
		if status != kcall.StatusOK {
			// A dead peer, or a message that does not decode at all.
			closeAttached()
			core.RemoveFamily(index)
			return
		}
		if message.HandleCount != 0 {
			closeAttached()
			core.RemoveFamily(index)
			return
		}
		core.Receive(index, &message)
	}
}

// transport is the linuxsock.Transport over the family channels.
type transport struct{}

func (transport) Send(index int, value *ipc.Message) bool {
	link := &families[index]
	if link.channel == 0 {
		return false
	}
	switch sendOn(link.channel, value) {
	case kcall.StatusOK:
		return true
	case kcall.StatusPeerDied:
		// The event loop sees the peer's end and removes the family.
	}
	return false
}

func (transport) Drop(index int) {
	link := &families[index]
	if link.channel == 0 {
		return
	}
	sys.HandleClose(link.channel)
	sys.VMUnmap(link.poolBase, link.poolSize)
	sys.HandleClose(link.pool)
	*link = familyLink{}
}

// --- control -----------------------------------------------------------------

func serviceControl() {
	for {
		status := receiveOn(controlHandle)
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			fatal("LINUXSOCKD_CONTROL_GONE\n")
		}
		if status != kcall.StatusOK {
			closeAttached()
			continue
		}
		correlation, valid := linuxsockapi.DecodeControlRequest(&message, &control)
		if !valid {
			opcode, attributed, known := linuxsockapi.ControlRequestOpcode(&message)
			closeAttached()
			if known {
				answerControl(linuxsockapi.ControlReply{Opcode: opcode, Result: linuxsockapi.ControlInvalidArgument}, attributed)
			}
			continue
		}
		answer := linuxsockapi.ControlReply{Opcode: control.Opcode}
		switch control.Opcode {
		case linuxsockapi.ControlAddFamily:
			answer.Token, answer.Result = control.Token, addFamily(control.Token, control.Session)
		case linuxsockapi.ControlRemoveFamily:
			answer.Token, answer.Result = control.Token, linuxsockapi.ControlNotFound
			if index, found := core.FamilyByToken(control.Token); found {
				core.RemoveFamily(index)
				answer.Result = linuxsockapi.ControlOK
			}
		case linuxsockapi.ControlSetTCP:
			answer.Generation, answer.Result = control.Generation, setBackend(linuxsock.TCP, control.Generation)
		case linuxsockapi.ControlSetUDP:
			answer.Generation, answer.Result = control.Generation, setBackend(linuxsock.UDP, control.Generation)
		case linuxsockapi.ControlStatus:
			core.Status(&answer)
		}
		answerControl(answer, correlation)
	}
}

func answerControl(answer linuxsockapi.ControlReply, correlation uint64) {
	controlReply = answer
	if linuxsockapi.EncodeControlReply(&controlReply, correlation, &reply) {
		sendOn(controlHandle, &reply)
	}
}

// addFamily takes a family's channel end and pool. The channel must carry
// exactly Read, Write and Wait and the pool Read and Write.
func addFamily(token uint64, session uint32) linuxsockapi.ControlResult {
	channel, pool := message.Handles[0], message.Handles[1]
	refuse := func(result linuxsockapi.ControlResult) linuxsockapi.ControlResult {
		sys.HandleClose(channel)
		sys.HandleClose(pool)
		return result
	}
	if !handleIs(channel, handle.TypeChannel, capability.Read|capability.Write|capability.Wait) ||
		!handleIs(pool, handle.TypeMemory, capability.Read|capability.Write) {
		return refuse(linuxsockapi.ControlInvalidArgument)
	}
	base, size, status := sys.MemoryMapAnywhere(pool, true)
	if status != kcall.StatusOK {
		return refuse(linuxsockapi.ControlInvalidArgument)
	}
	if size < linuxsockapi.PoolBytes {
		sys.VMUnmap(base, size)
		return refuse(linuxsockapi.ControlInvalidArgument)
	}
	index, result := core.AddFamily(token, session, sys.Window(base, linuxsockapi.PoolBytes))
	if result != linuxsockapi.ControlOK {
		sys.VMUnmap(base, size)
		return refuse(result)
	}
	families[index] = familyLink{channel: channel, pool: pool, poolBase: base, poolSize: size}
	return linuxsockapi.ControlOK
}

// setBackend installs a native session from the request's three handles.
func setBackend(proto linuxsock.Proto, generation uint64) linuxsockapi.ControlResult {
	channel, tx, rx := message.Handles[0], message.Handles[1], message.Handles[2]
	if generation <= links[proto].generation || generation <= core.BackendGeneration(proto) {
		closeAttached()
		return linuxsockapi.ControlStale
	}
	if !setLink(proto, generation, channel, tx, rx) {
		closeAttached()
		return linuxsockapi.ControlInvalidArgument
	}
	if result := core.BackendUp(proto, generation); result != linuxsockapi.ControlOK {
		dropLink(proto)
		return result
	}
	syslog.Info("native session bound", syslog.Uint("protocol", uint64(proto)), syslog.Uint("generation", generation))
	return linuxsockapi.ControlOK
}

// handleIs checks a received handle's type and exact rights.
func handleIs(id handle.ID, typeID handle.Type, rights capability.Rights) bool {
	var record [kcall.HandleInfoSize]byte
	slot := uint64(uint32(id)) - 1
	if uint32(id) == 0 || sys.HandleInfo(slot, &record) != kcall.StatusOK {
		return false
	}
	recordID := uint64(record[0]) | uint64(record[1])<<8 | uint64(record[2])<<16 | uint64(record[3])<<24 |
		uint64(record[4])<<32 | uint64(record[5])<<40 | uint64(record[6])<<48 | uint64(record[7])<<56
	got := capability.Rights(uint32(record[12]) | uint32(record[13])<<8 | uint32(record[14])<<16 | uint32(record[15])<<24)
	return recordID == uint64(id) && handle.Type(record[8]) == typeID && got == rights
}

// --- reporting -----------------------------------------------------------------

func mark(text string) {
	if validation.Enabled {
		sys.ConsoleWriteString(consoleHandle, text)
	}
}

func fatal(text string) {
	if !syslog.Error(text[:len(text)-1]) {
		sys.ConsoleWriteString(consoleHandle, text)
	}
	sys.Exit(exitFailed)
}

func main() {}
