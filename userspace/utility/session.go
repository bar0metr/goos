package utility

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/lock"
	"sync/atomic"
)

// transport moves wire messages on a channel. Failed sends do not transfer
// attachments. A successful receive transfers ownership of all its handles.
type transport interface {
	send(channel handle.ID, wire []byte) kcall.Status
	waitReceive(channel handle.ID, wire []byte, ticks uint64) kcall.Status
	close(handle.ID)
}

// session serializes access to a channel and its codec storage. Synchronous
// clients hold it for the entire exchange, including the wait. Split TCP/UDP
// APIs remain single-owner protocols, not concurrent request multiplexers.
//
// An abandoned exchange permanently quarantines that handle ID. Correlation
// filtering cannot make a late server access to shared memory safe. bind may
// select a different endpoint, but never revives a quarantined one. Endpoints
// are borrowed: neither rebind nor quarantine closes them.
type session[T transport] struct {
	mu          lock.Mutex
	channel     atomic.Uint64
	quarantined []handle.ID
	correlation uint64
	wire        [ipc.WireMaxSize]byte
	outgoing    ipc.Message
	incoming    ipc.Message
	io          T
}

// bind waits for the current exchange and selects a borrowed endpoint. A zero
// channel means unavailable. Handle IDs include a generation; a new endpoint
// in a reused slot is distinct from the quarantined ID.
func (s *session[T]) bind(channel handle.ID) {
	s.hold()
	defer s.end()
	for _, unavailable := range s.quarantined {
		if unavailable == channel {
			s.channel.Store(0)
			return
		}
	}
	s.channel.Store(uint64(channel))
}

// bound is also usable by exit without taking a potentially held lock.
func (s *session[T]) bound() handle.ID { return handle.ID(s.channel.Load()) }
func (s *session[T]) hold()            { s.mu.Lock() }

func (s *session[T]) begin() uint64 {
	s.hold()
	return s.next()
}

func (s *session[T]) next() uint64 {
	s.correlation++
	if s.correlation == 0 {
		s.correlation++
	}
	return s.correlation
}

func (s *session[T]) end() { s.mu.Unlock() }

// abandon is called under the lock whenever a sent request is not completed
// by a valid protocol reply, including timeout, cancellation and peer death.
// Retained IDs prevent BindVFS(old) from exposing a late reply after a rebind.
func (s *session[T]) abandon() {
	if channel := s.bound(); channel != 0 {
		s.channel.Store(0)
		s.quarantined = append(s.quarantined, channel)
	}
}

func (s *session[T]) send(message *ipc.Message) bool {
	if s.bound() == 0 {
		return false
	}
	size, ok := ipc.EncodeFrom(message, s.wire[:])
	if !ok {
		return false
	}
	if s.io.send(s.bound(), s.wire[:size]) != kcall.StatusOK {
		s.abandon()
		return false
	}
	return true
}

// receive leaves timeout handling to callers which deliberately retry. Every
// other failed wait or malformed wire abandons the endpoint. Clearing the wire
// first prevents a short delivery from inheriting the previous reply's bytes.
func (s *session[T]) receive(ticks uint64) (kcall.Status, bool) {
	if s.bound() == 0 {
		return kcall.StatusPeerDied, false
	}
	clear(s.wire[:])
	s.incoming = ipc.Message{}
	if status := s.io.waitReceive(s.bound(), s.wire[:], ticks); status != kcall.StatusOK {
		if status != kcall.StatusTimeout {
			s.abandon()
		}
		return status, false
	}
	size := ipc.WireHeaderSize + (int(s.wire[24]) | int(s.wire[25])<<8) + int(s.wire[26])*8
	valid := size <= len(s.wire) && ipc.DecodeInto(s.wire[:size], &s.incoming)
	if !valid {
		// Even a protocol-invalid delivery may own transferred handles. The
		// kernel supplies the wire framing; inspect only bounded handle slots.
		payload, count := int(s.wire[24])|int(s.wire[25])<<8, int(s.wire[26])
		offset := ipc.WireHeaderSize + payload
		if count <= ipc.MaxAttachedHandles && offset+count*8 <= len(s.wire) {
			for index := 0; index < count; index++ {
				id := handle.ID(binary.LittleEndian.Uint64(s.wire[offset+index*8:]))
				if id != 0 {
					s.io.close(id)
				}
			}
		}
		s.incoming = ipc.Message{}
		s.abandon()
	}
	return kcall.StatusOK, valid
}

func (s *session[T]) received(ticks uint64) bool {
	status, valid := s.receive(ticks)
	if status != kcall.StatusOK || !valid {
		s.abandon()
		return false
	}
	return true
}

// reject releases attachments of a decoded but unacceptable message. A client
// which accepts a handle-bearing notice takes ownership instead.
func (s *session[T]) reject() {
	for index := 0; index < int(s.incoming.HandleCount); index++ {
		s.io.close(s.incoming.Handles[index])
	}
	s.incoming = ipc.Message{}
	s.abandon()
}

type requestPaths interface {
	Hold()
	Release()
	Prepare(*vfs.Request) (handle.ID, bool)
	Name(int) []byte
	Abandon()
}

// vfsSession keeps the native transaction path host-testable. The path page
// and session are held until all returned slices belong to the caller.
type vfsSession[T transport, P requestPaths] struct {
	session[T]
	paths P
}

func (s *vfsSession[T, P]) call(request vfs.Request, ticks uint64, attempts int) (vfs.Reply, bool) {
	s.hold()
	defer s.end()
	s.paths.Hold()
	defer s.paths.Release()
	reply, ok := s.exchange(request, 0, ticks, attempts)
	if ok && (request.Operation == vfs.OperationReadDir || request.Operation == vfs.OperationReadMount) && reply.Status == vfs.StatusNameTooLong {
		request.NameReturn = true
		reply, ok = s.exchange(request, 0, ticks, attempts)
	}
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

func (s *vfsSession[T, P]) callAttached(request vfs.Request, attached handle.ID, ticks uint64, attempts int) (vfs.Reply, bool) {
	s.hold()
	defer s.end()
	return s.exchange(request, attached, ticks, attempts)
}

// exchange requires the session and, unless a bulk handle is attached, the
// path page. Any failure after send quarantines the endpoint and detaches the
// page; closing an endpoint would not revoke the server's memory mappings.
func (s *vfsSession[T, P]) exchange(request vfs.Request, attached handle.ID, ticks uint64, attempts int) (vfs.Reply, bool) {
	usesPage := attached == 0
	if s.bound() == 0 {
		if attached != 0 {
			s.io.close(attached)
		}
		return vfs.Reply{}, false
	}
	if usesPage {
		var ok bool
		if attached, ok = s.paths.Prepare(&request); !ok {
			return vfs.Reply{}, false
		}
	}
	correlation := s.next()
	var ok bool
	s.outgoing, ok = vfs.EncodeRequest(request, correlation)
	if ok && attached != 0 {
		s.outgoing.HandleCount, s.outgoing.Handles[0] = 1, attached
	}
	if !ok || !s.send(&s.outgoing) {
		if attached != 0 {
			s.io.close(attached)
		}
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < attempts; attempt++ {
		status, valid := s.receive(ticks)
		if status == kcall.StatusTimeout {
			continue
		}
		if status == kcall.StatusOK && valid {
			// DecodeReply's by-value copy gives Name and Data independent storage.
			reply, decoded := vfs.DecodeReply(s.incoming)
			if decoded && s.incoming.HandleCount == 0 && s.incoming.Correlation == correlation && reply.Operation == request.Operation &&
				(reply.AttachedName == 0 || usesPage && request.NameReturn && reply.Status == vfs.StatusOK) &&
				(!request.Operation.Bulk() || reply.Size <= uint64(request.Size)) {
				return reply, true
			}
			s.reject()
		}
		break
	}
	s.abandon()
	if usesPage && attached != 0 {
		s.paths.Abandon()
	}
	return vfs.Reply{}, false
}

// receiveProtocol is the split TCP/UDP receive path. Invalid protocol replies
// have the same ownership/quarantine rules as synchronous exchanges.
func receiveProtocol[T transport, R any](s *session[T], ticks uint64, reply *R, decode func(*ipc.Message, *R) (uint64, bool)) (uint64, bool) {
	if s.received(ticks) {
		if correlation, ok := decode(&s.incoming, reply); ok {
			return correlation, true
		}
		s.reject()
	}
	var empty R
	*reply = empty
	return 0, false
}
