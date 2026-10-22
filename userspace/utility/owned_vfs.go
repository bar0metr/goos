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
type ownedVFS[T clockTransport, P requestPaths] struct {
	vfsSession[T, P]
	// lost is the attachment generation of the latest filesystem
	// replacement notice the control endpoint delivered, or zero.
	lost uint64
}

func (s *ownedVFS[T, P]) callOn(channel handle.ID, request vfs.Request, attached handle.ID, correlation, budget uint64) (vfs.Reply, bool) {
	return s.callObserved(channel, request, attached, correlation, budget, nil)
}

// VFSFailure is a caller-owned diagnostic snapshot captured before quarantine.
// Phase distinguishes send/receive/wire/reply/deadline failures; Status is the
// raw syscall status except locally detected protocol/deadline failures.
// Completed VFS error replies have Phase complete and Status OK.
type VFSFailure struct {
	Phase            string
	Status           kcall.Status
	Sent             bool
	ReplyCorrelation uint64
	ReplyOperation   vfs.Operation
}

func (s *ownedVFS[T, P]) callObserved(channel handle.ID, request vfs.Request, attached handle.ID, correlation, budget uint64, failure *VFSFailure) (vfs.Reply, bool) {
	if failure != nil {
		*failure = VFSFailure{Phase: "start", Status: kcall.StatusInvalidArgument}
	}
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	start, status := s.io.now()
	if failure != nil {
		switch {
		case s.bound() == 0:
			failure.Phase, failure.Status = "bound", kcall.StatusPeerDied
		case status != kcall.StatusOK:
			failure.Phase, failure.Status = "clock", status
		}
	}
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
	reply, ok := s.exchangeObserved(request, attached, budget, 1, deadline, s.io.now, failure)
	if ok && reply.AttachedName != 0 {
		name := s.paths.Name(int(reply.AttachedName))
		if len(name) != int(reply.AttachedName) {
			if failure != nil {
				failure.Phase, failure.Status = "name", kcall.StatusCorruptData
			}
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
	// A filesystem replacement notice the VFS sent before this exchange's
	// reply is kept for the owner, not taken for a malformed reply.
	for status == kcall.StatusOK && valid && s.noteLost() {
		if ticks = remaining(deadline, s.io.now); ticks == 0 {
			s.abandon()
			return 0, kcall.StatusTimeout, true
		}
		status, valid = s.receive(ticks)
	}
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

// noteLost records the received message if it is a filesystem replacement
// notice. Notices carry no handles.
func (s *ownedVFS[T, P]) noteLost() bool {
	generation, ok := vfs.DecodeFilesystemLostNotice(&s.incoming)
	if ok {
		s.lost = generation
		s.incoming = ipc.Message{}
	}
	return ok
}

// pollControlOn drains, without waiting, the messages a control endpoint
// received outside an exchange. It keeps filesystem replacement notices and
// discards anything else, closing its handles: no request is outstanding, so
// nothing else is owed. A quarantined endpoint is not read. It reports
// whether the endpoint is still usable.
func (s *ownedVFS[T, P]) pollControlOn(channel handle.ID) bool {
	s.hold()
	defer s.end()
	s.bindHeld(channel)
	for s.bound() != 0 {
		clear(s.wire[:])
		s.incoming = ipc.Message{}
		if s.io.waitReceive(s.bound(), s.wire[:], 0) != kcall.StatusOK {
			break
		}
		if decodeIPC(s.wire[:], &s.incoming, s.io.close) && !s.noteLost() {
			for index := 0; index < int(s.incoming.HandleCount); index++ {
				s.io.close(s.incoming.Handles[index])
			}
		}
		s.incoming = ipc.Message{}
	}
	return s.bound() != 0
}

// takeLost returns and clears the latest filesystem replacement notice.
func (s *ownedVFS[T, P]) takeLost() (uint64, bool) {
	s.hold()
	defer s.end()
	generation := s.lost
	s.lost = 0
	return generation, generation != 0
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
