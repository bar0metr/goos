//go:build goos_userspace

package main

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

// A TCP session speaks GTCP 2 or 3 through state, or GTCP 4 through ring.
// These methods pick the one the session started with.

func (s *tcpSession) layout() tcpapi.Layout {
	if s.four {
		return tcpapi.Layout4
	}
	return s.state.Layout()
}

func (s *tcpSession) start(index, quota int, layout tcpapi.Layout, tx, rx []byte, now uint64) bool {
	s.four = layout.Version == tcpapi.Version4
	if s.four {
		return s.ring.Start(&stack, index, quota, tx, rx, now)
	}
	return s.state.StartLayout(&stack, index, quota, layout, tx, rx, now)
}

func (s *tcpSession) end(now uint64) {
	if s.four {
		s.ring.End(&stack, now)
		return
	}
	s.state.End(&stack, now)
}

func (s *tcpSession) decode(in *ipc.Message, out *tcpapi.Request) (uint64, bool) {
	if s.four {
		return s.ring.Decode(in, out)
	}
	return s.state.Decode(in, out)
}

func (s *tcpSession) encode(reply *tcpapi.Reply, correlation uint64, out *ipc.Message) bool {
	if s.four {
		return s.ring.Encode(reply, correlation, out)
	}
	return s.state.Encode(reply, correlation, out)
}

func (s *tcpSession) handle(index int, request *tcpapi.Request, correlation, now uint64) tcpserve.Fault {
	if s.four {
		if request.Opcode == tcpapi.OpDoorbell {
			gtcpDoorbells[0]++
		}
		return s.ring.Handle(&stack, request, correlation, now, tcpReplier{index})
	}
	return s.state.Handle(&stack, request, correlation, now, tcpReplier{index})
}

func (s *tcpSession) deliver(index int, now uint64) (tcpserve.Fault, bool) {
	if s.four {
		return s.ring.Deliver(&stack, now, tcpReplier{index})
	}
	return s.state.Deliver(&stack, now, tcpReplier{index})
}

func (s *tcpSession) progressed() bool {
	if s.four {
		return s.ring.Progressed()
	}
	return s.state.Progressed()
}

func (s *tcpSession) runnable() (uint32, string, bool) {
	if s.four {
		// The loop checks only after armTCP found every session idle.
		return s.ring.RunnableArmed(&stack)
	}
	return s.state.Runnable(&stack)
}

func (s *tcpSession) waiting() int {
	if s.four {
		return s.ring.Waiting()
	}
	return s.state.Waiting()
}

func (s *tcpSession) lent() uint64 {
	if s.four {
		return uint64(s.ring.RXLent())
	}
	return uint64(s.state.RXLentBits())
}

// Doorbell sends a GTCP 4 session's OpDoorbell. A full queue holds it, as
// it holds a reply; the client sleeps until it arrives.
func (r tcpReplier) Doorbell() bool {
	current := &tcpSessions[r.index]
	if current.doorbellHeld {
		return true
	}
	switch sendTCPDoorbell(current) {
	case kcall.StatusOK:
	case kcall.StatusPeerDied, kcall.StatusCorruptData:
		return false
	default:
		current.doorbellHeld = true
	}
	return true
}

func sendTCPDoorbell(current *tcpSession) kcall.Status {
	if !tcpapi.EncodeDoorbell4(&reply) {
		return kcall.StatusCorruptData
	}
	size, ok := ipc.EncodeFrom(&reply, wire[:])
	if !ok {
		return kcall.StatusCorruptData
	}
	status := sys.IPCSend(current.channel, wire[:size])
	if status == kcall.StatusOK {
		gtcpDoorbells[1]++
	}
	return status
}

// gtcpDoorbells counts the GTCP 4 doorbells netd took and sent.
var gtcpDoorbells [2]uint64

// armTCP arms every GTCP 4 session's doorbell before netd sleeps. It
// reports false when a client published ring entries meanwhile; then netd
// must run Deliver before it sleeps.
func armTCP() bool {
	idle := true
	for index := range tcpSessions {
		current := &tcpSessions[index]
		if current.channel != 0 && current.four && !current.ring.Idle() {
			idle = false
		}
	}
	return idle
}
