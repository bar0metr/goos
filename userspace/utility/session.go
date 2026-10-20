package utility

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/lock"
	"sync/atomic"
)

// transport moves wire messages on a channel. The native one calls the
// kernel; host tests supply a fake service.
type transport interface {
	send(channel handle.ID, wire []byte) kcall.Status
	waitReceive(channel handle.ID, wire []byte, ticks uint64) kcall.Status
}

// session is the client end of one channel to a service that has at most one
// request of this client outstanding: the VFS, PID 1's launch channel, the
// network and resolver services, and a TCP or UDP session.
//
// A session is a serialized resource. A caller takes it with begin, sends
// one request, waits for the reply, and gives it back with end, so its lock
// is held across the IPC wait. That is the point: two requests in flight on
// one channel would race for each other's replies. Concurrent callers queue
// on the lock; each one waits at most its own bound once it holds the
// session. The wire buffer and the two messages belong to the holder.
//
// The channel is an atomic word so that exit and BindVFS can read and
// replace it without the lock; the correlation counter is per session.
type session[T transport] struct {
	mu          lock.Mutex
	channel     atomic.Uint64
	correlation uint64
	wire        [ipc.WireMaxSize]byte
	outgoing    ipc.Message
	incoming    ipc.Message
	io          T
}

// bind sets the channel. A zero channel means the session is not granted.
func (s *session[T]) bind(channel handle.ID) { s.channel.Store(uint64(channel)) }

// bound returns the channel, zero when there is none.
func (s *session[T]) bound() handle.ID { return handle.ID(s.channel.Load()) }

// hold takes the session without starting a request.
func (s *session[T]) hold() { s.mu.Lock() }

// begin takes the session and returns the correlation for its next request,
// never zero.
func (s *session[T]) begin() uint64 {
	s.mu.Lock()
	return s.next()
}

// next returns the correlation for the holder's next request, never zero.
func (s *session[T]) next() uint64 {
	s.correlation++
	if s.correlation == 0 {
		s.correlation++
	}
	return s.correlation
}

// end gives the session back.
func (s *session[T]) end() { s.mu.Unlock() }

// send encodes message into the wire buffer and sends it. The caller holds
// the session.
func (s *session[T]) send(message *ipc.Message) bool {
	size, ok := ipc.EncodeFrom(message, s.wire[:])
	return ok && s.io.send(s.bound(), s.wire[:size]) == kcall.StatusOK
}

// receive waits up to ticks for the next message on the channel and decodes
// it into s.incoming. It returns the wait's status and, when a message
// arrived, whether it decoded. The caller holds the session.
func (s *session[T]) receive(ticks uint64) (kcall.Status, bool) {
	if status := s.io.waitReceive(s.bound(), s.wire[:], ticks); status != kcall.StatusOK {
		return status, false
	}
	size := ipc.WireHeaderSize + (int(s.wire[24]) | int(s.wire[25])<<8) + int(s.wire[26])*8
	return kcall.StatusOK, size <= len(s.wire) && ipc.DecodeInto(s.wire[:size], &s.incoming)
}

// received is receive for callers that treat a message that does not decode
// like a failed wait.
func (s *session[T]) received(ticks uint64) bool {
	status, valid := s.receive(ticks)
	return status == kcall.StatusOK && valid
}
