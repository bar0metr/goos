// Package diag keeps a bounded trace of kernel wait and object-lifecycle
// events for the goos_diag build, so a stalled or failed boot can be explained
// from the serial log without adding output to normal builds.
package diag

import "goos/kernel/lock"

type Kind uint8

const (
	WaitBegin Kind = iota + 1
	WaitReady
	WaitRecheck
	WaitComplete
	WaitTimeout
	WaitTaken
	WaitCancelExit
	WaitDenied
	ChannelCreated
	ChannelClosed
	HandleTransferred
	ProcessExited
	ProcessSpawned
)

var kindNames = [...]string{
	WaitBegin:         "wait-begin",
	WaitReady:         "wait-ready",
	WaitRecheck:       "wait-recheck",
	WaitComplete:      "wait-complete",
	WaitTimeout:       "wait-timeout",
	WaitTaken:         "wait-taken",
	WaitCancelExit:    "wait-cancel-exit",
	WaitDenied:        "wait-denied",
	ChannelCreated:    "channel-created",
	ChannelClosed:     "channel-closed",
	HandleTransferred: "handle-transferred",
	ProcessExited:     "process-exited",
	ProcessSpawned:    "process-spawned",
}

func (kind Kind) String() string {
	if int(kind) < len(kindNames) && kindNames[kind] != "" {
		return kindNames[kind]
	}
	return "unknown"
}

// Deny names the check that rejected a wait interest.
type Deny uint64

const (
	DenyOwner Deny = iota + 1
	DenyHandle
	DenyRights
	DenyEvents
	DenyObject
	DenyType
)

var denyNames = [...]string{
	DenyOwner:  "caller-not-registered",
	DenyHandle: "handle-closed-or-stale",
	DenyRights: "rights",
	DenyEvents: "events",
	DenyObject: "object-released",
	DenyType:   "type",
}

func (deny Deny) String() string {
	if int(deny) < len(denyNames) && denyNames[deny] != "" {
		return denyNames[deny]
	}
	return "unknown"
}

// Close names why a channel endpoint was closed.
const (
	CloseByHandle   = 1
	CloseByTeardown = 2
)

// Event times are nanoseconds since the timer started.
type Event struct {
	Time    uint64
	Kind    Kind
	Process uint64
	Task    uint64
	Object  uint64
	Value   uint64
}

const RingSize = 128

// Ring retains the most recent RingSize events. Processors record at once;
// the ring's lock serializes them.
type Ring struct {
	lock   lock.Spin
	events [RingSize]Event
	next   int
	total  uint64
}

func (ring *Ring) take() {
	ring.lock.SetLevel(lock.LevelLog)
	ring.lock.Lock()
}

func (ring *Ring) Record(event Event) {
	if !Enabled {
		return
	}
	ring.take()
	ring.events[ring.next] = event
	ring.next = (ring.next + 1) % RingSize
	ring.total++
	ring.lock.Unlock()
}

func (ring *Ring) Total() uint64 {
	ring.take()
	total := ring.total
	ring.lock.Unlock()
	return total
}

// Len reports how many events are retained.
func (ring *Ring) Len() int {
	ring.take()
	length := ring.length()
	ring.lock.Unlock()
	return length
}

func (ring *Ring) length() int {
	if ring.total < RingSize {
		return int(ring.total)
	}
	return RingSize
}

// At returns the index-th retained event, oldest first.
func (ring *Ring) At(index int) Event {
	ring.take()
	start := (ring.next - ring.length() + RingSize) % RingSize
	event := ring.events[(start+index)%RingSize]
	ring.lock.Unlock()
	return event
}
