package utility

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
)

type clockTransport interface {
	transport
	now() (uint64, kcall.Status)
}

// ownedVFS shares the native utility transaction implementation with shell
// and supervisor callers that own several endpoint IDs. The caller provides
// correlations from its service-loop sequence, including asynchronous sends.
// Endpoint ownership/closure and acquisition of fresh authority stay outside.
type ownedVFS[T clockTransport, P requestPaths] struct{ vfsSession[T, P] }

func (s *ownedVFS[T, P]) callOn(channel handle.ID, request vfs.Request, attached handle.ID, correlation, budget uint64) (vfs.Reply, bool) {
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	start, status := s.io.now()
	if s.bound() == 0 || correlation == 0 || status != kcall.StatusOK || budget == 0 {
		if attached != 0 {
			s.io.close(attached)
		}
		return vfs.Reply{}, false
	}
	deadline := start + budget
	if deadline < start {
		deadline = ^uint64(0)
	}
	s.paths.Hold()
	defer s.paths.Release()
	// The next correlation must match the caller's shared synchronous/async
	// sequence; the owner skips zero before calling.
	s.correlation = correlation - 1
	reply, ok := s.exchangeDeadline(request, attached, budget, 1, deadline, s.io.now)
	if ok && reply.AttachedName != 0 {
		name := s.paths.Name(int(reply.AttachedName))
		if len(name) != int(reply.AttachedName) {
			s.abandon()
			s.paths.Abandon()
			return vfs.Reply{}, false
		}
		reply.Name = append([]byte(nil), name...)
	}
	return reply, ok
}

func (s *ownedVFS[T, P]) rejectOn(channel handle.ID, message *ipc.Message) {
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	if message != nil {
		s.incoming = *message
		for index := 0; index < int(s.incoming.HandleCount) && index < len(s.incoming.Handles); index++ {
			s.io.close(s.incoming.Handles[index])
		}
		*message = ipc.Message{}
		s.incoming = ipc.Message{}
	}
	s.abandon()
}

func remaining(deadline uint64, now func() (uint64, kcall.Status)) uint64 {
	current, status := now()
	if status != kcall.StatusOK || current >= deadline {
		return 0
	}
	return deadline - current
}

// decodeIPC accepts a kernel delivery window (not an arbitrary truncated
// network packet). Even malformed protocol bytes may carry receiver-owned
// handles. Reclaim bounded handle slots before discarding an invalid message.
func decodeIPC(wire []byte, message *ipc.Message, close func(handle.ID)) bool {
	*message = ipc.Message{}
	if len(wire) < ipc.WireHeaderSize {
		return false
	}
	payload, count := int(wire[24])|int(wire[25])<<8, int(wire[26])
	offset := ipc.WireHeaderSize + payload
	size := offset + count*8
	if size <= len(wire) && ipc.DecodeInto(wire[:size], message) {
		return true
	}
	if count <= ipc.MaxAttachedHandles && size <= len(wire) {
		for index := 0; index < count; index++ {
			id := handle.ID(binary.LittleEndian.Uint64(wire[offset+index*8:]))
			if id != 0 {
				close(id)
			}
		}
	}
	*message = ipc.Message{}
	return false
}

// controlOn handles the VFS control protocol's uniform status reply. Sent
// tells the owner whether request attachments transferred. Unlike Call, an
// unsent control attachment remains the caller's responsibility.
func (s *ownedVFS[T, P]) controlOn(channel handle.ID, request *ipc.Message, budget uint64) (vfs.Status, kcall.Status, bool) {
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	if s.bound() == 0 {
		return 0, kcall.StatusPeerDied, false
	}
	start, status := s.io.now()
	if status != kcall.StatusOK || budget == 0 {
		return 0, kcall.StatusTimeout, false
	}
	if request == nil || request.ProtocolID != vfs.ControlProtocolID || request.Version != vfs.ControlVersion || request.Kind != ipc.KindRequest || request.Correlation == 0 {
		return 0, kcall.StatusInvalidArgument, false
	}
	deadline := start + budget
	if deadline < start {
		deadline = ^uint64(0)
	}
	if remaining(deadline, s.io.now) == 0 {
		return 0, kcall.StatusTimeout, false
	}
	if !s.send(request) {
		return 0, kcall.StatusIOError, false
	}
	ticks := remaining(deadline, s.io.now)
	if ticks == 0 {
		s.abandon()
		return 0, kcall.StatusTimeout, true
	}
	status, valid := s.receive(ticks)
	if status != kcall.StatusOK || !valid {
		s.abandon()
		if status == kcall.StatusOK {
			status = kcall.StatusCorruptData
		}
		return 0, status, true
	}
	result, valid := vfs.DecodeAddSessionReply(s.incoming)
	if !valid || s.incoming.Correlation != request.Correlation {
		s.reject()
		return 0, kcall.StatusCorruptData, true
	}
	return result, kcall.StatusOK, true
}

// completeOn validates an asynchronously sent request. A decoded error status
// is a completion fence; a malformed/future reply is not, even if the peer
// claims it already finished the work. No rejected handle remains accepted.
func (s *ownedVFS[T, P]) completeOn(channel handle.ID, request vfs.Request, correlation uint64, message *ipc.Message, status kcall.Status) (vfs.Reply, bool) {
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	reply, valid := vfs.DecodeReply(*message)
	if s.bound() != 0 && status == kcall.StatusOK && valid && message.HandleCount == 0 && message.Correlation == correlation &&
		reply.Operation == request.Operation && reply.AttachedName == 0 &&
		(!request.Operation.Bulk() || reply.Size <= uint64(request.Size)) &&
		(request.Operation != vfs.OperationPrepareExecutable || reply.Status != vfs.StatusOK || reply.Size == uint64(request.Size)) {
		return reply, true
	}
	s.incoming = *message
	s.reject()
	*message = ipc.Message{}
	return vfs.Reply{}, false
}
