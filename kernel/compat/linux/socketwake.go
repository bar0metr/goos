package linux

import (
	"sync/atomic"

	"goos/userspace/linuxsockapi"
)

// Targeted socket wakes (ADR 0210). A call that sleeps on a socket waits on
// that socket's key, not on the session channel, so a ReadyChanged from the
// service wakes only the calls whose sockets changed. Whoever drains the
// session runs the wake scan after it: each live socket whose readiness
// entry has a new sequence, or that a ring entry named, gets one wake.
//
// The kernel drains a session in two places: in the service's own send,
// right after the message lands, and in a socket call that looks at its
// socket. BeginPump and EndPump let one drain run at a time; a drain asked
// for while another runs makes that one go round again.

// netChannels mirrors the kernel side of each attached session's channel,
// context by context, so a send finds whether it reached a session without
// the tables lock. Zero is no live session. Writers hold the tables lock.
var netChannels [MaxNetFamilies]atomic.Uint64

// mirrorChannel publishes the context's channel while its session is live.
func (context *NetContext) mirrorChannel() {
	for index := range netContexts {
		if &netContexts[index] != context {
			continue
		}
		if context.familyID != 0 && context.attached && !context.failed {
			netChannels[index].Store(channelKey(context.endpoint.Channel, context.endpoint.Side))
		} else {
			netChannels[index].Store(0)
		}
		return
	}
}

func channelKey(channel uint64, side uint8) uint64 { return channel<<1 | uint64(side&1) }

// NetFamilyOfChannel finds the live session whose kernel side is channel's
// side.
func NetFamilyOfChannel(channel uint64, side uint8) (familyID uint64, endpoint NetEndpoint, found bool) {
	key := channelKey(channel, side)
	if channel == 0 {
		return 0, NetEndpoint{}, false
	}
	for index := range netChannels {
		if netChannels[index].Load() != key {
			continue
		}
		tablesLock.Lock()
		context := &netContexts[index]
		if context.familyID != 0 && context.attached && !context.failed &&
			context.endpoint.Channel == channel && context.endpoint.Side == side {
			familyID, endpoint, found = context.familyID, context.endpoint, true
		}
		tablesLock.Unlock()
		return familyID, endpoint, found
	}
	return 0, NetEndpoint{}, false
}

// BeginPump claims the drain of a family's session. false means another
// processor drains it now and will drain again for this caller.
func BeginPump(familyID uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return true
	}
	if context.pumping {
		context.pumpAgain = true
		return false
	}
	context.pumping = true
	return true
}

// EndPump gives up the drain. again reports that another caller asked for a
// drain meanwhile; the drain stays claimed and the caller goes round again.
func EndPump(familyID uint64) (again bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return false
	}
	if context.pumpAgain {
		context.pumpAgain = false
		return true
	}
	context.pumping = false
	return false
}

// NoteReadyChanged records a ReadyChanged the drain took, for the wake scan.
func NoteReadyChanged(familyID uint64) {
	tablesLock.Lock()
	if context, _, found := netContextHeld(familyID); found {
		context.rescan = true
	}
	tablesLock.Unlock()
}

// touchHeld marks a live socket for the next wake scan.
func (context *NetContext) touchHeld(identity uint64) {
	if socket, found := context.liveSocketHeld(identity); found {
		socket.touched = true
	}
}

// ReadinessView is the readiness pages of a session's pool, in order.
type ReadinessView [linuxsockapi.ReadinessPages][]byte

// TakeRescan reports whether a ReadyChanged or a ring entry asked for a
// wake scan since the last one, and clears the request.
func TakeRescan(familyID uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.rescan {
		return false
	}
	context.rescan = false
	return true
}

// ScanSocketWakes lists the description objects of the sockets that need a
// wake, from slot cursor on, into out. next is where a further call goes
// on, or -1 when the scan is done.
func ScanSocketWakes(familyID uint64, session uint32, pages *ReadinessView, cursor int, out []uint64) (count int, next int) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, contextIndex, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return 0, -1
	}
	for slot := cursor; slot < MaxSockets; slot++ {
		socket := &context.sockets[slot]
		if !socket.used || socket.Session != session {
			continue
		}
		index, valid := linuxsockapi.SocketIndex(socket.Identity)
		if !valid {
			continue
		}
		page := pages[linuxsockapi.EntryPage(index)-linuxsockapi.ReadinessPage]
		sequence, ok := linuxsockapi.EntrySequence(page, index)
		if !ok || sequence == socket.seen && sequence&1 == 0 && !socket.touched {
			continue
		}
		if count == len(out) {
			// The socket stays changed for the call that goes on here.
			return count, slot
		}
		socket.seen, socket.touched = sequence, false
		socket.wakes++
		out[count] = socketObject(contextIndex, slot, socket.generation)
		count++
	}
	return count, -1
}

// FamilySocketObjects lists the description objects of a family's sockets
// from slot cursor on, for wakes that reach all of them: a failed session.
func FamilySocketObjects(familyID uint64, cursor int, out []uint64) (count int, next int) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, contextIndex, found := netContextHeld(familyID)
	if !found {
		return 0, -1
	}
	for slot := cursor; slot < MaxSockets; slot++ {
		socket := &context.sockets[slot]
		if !socket.used {
			continue
		}
		if count == len(out) {
			return count, slot
		}
		out[count] = socketObject(contextIndex, slot, socket.generation)
		count++
	}
	return count, -1
}

// ReplyReady reports whether a reply waits for its owner to take it.
func ReplyReady(familyID uint64, correlation uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return false
	}
	request, ok := context.requestHeld(correlation)
	return ok && request.state == requestReplied
}
