package linuxsock

import "goos/userspace/linuxsockapi"

// The socket registry. Sockets live in chunks of socketChunk that the
// service allocates as use grows and keeps once allocated, so a *socket
// stays valid while the socket lives. A global index names a socket: the
// chunk times socketChunk plus the position in it. A stack of free global
// indexes makes allocation and release constant time, and each family maps
// its own indexes to global ones (family.slots), so a lookup by identity
// never scans. Operations live the same way in opTable.
//
// Three quotas bound the sockets, opening and accepted-ahead ones
// included:
//
//   - Limits.Family per family. One past it answers ResultFamilyLimit,
//     which the kernel reports as EMFILE from socket() and accept(): the
//     family is the caller's own allotment, and closing its own sockets
//     frees room.
//   - Limits.System across families. One past it answers
//     ResultServiceLimit, ENFILE from socket() and accept(): the shared
//     table is full.
//   - Limits.Reserve per live family. A family below its reserve always
//     gets a socket while the others have not taken that room: another
//     family's socket is admitted only if the reserves every other family
//     still lacks stay free. A family added while the table holds less
//     room than its reserve gets it as other sockets close.
//
// The family check runs first, so a family at its own quota never reads
// as a full service.

// Limits are the socket quotas of linuxsockd.
type Limits struct {
	// System bounds the sockets of every family together.
	System int
	// Family bounds the sockets of one family, at most
	// linuxsockapi.MaxSockets.
	Family int
	// Reserve is the sockets kept back for each live family.
	Reserve int
}

const (
	// DefaultSystemSockets, DefaultFamilySockets and DefaultFamilyReserve
	// are the production quotas: two families at their quota fill the
	// service, and every one of MaxFamilies families keeps 32 sockets,
	// half of the system quota in all.
	DefaultSystemSockets = 1024
	DefaultFamilySockets = linuxsockapi.MaxSockets
	DefaultFamilyReserve = 32
	// MaxSystemSockets bounds Limits.System: global indexes fit the int16
	// fields that hold them, plus one.
	MaxSystemSockets = 4096

	socketChunk = 64
	opChunk     = 64
)

// DefaultLimits returns the production quotas.
func DefaultLimits() Limits {
	return Limits{System: DefaultSystemSockets, Family: DefaultFamilySockets, Reserve: DefaultFamilyReserve}
}

// Valid reports whether the quotas fit the protocol and the tables: every
// reserve fits the system quota at once.
func (l Limits) Valid() bool {
	return l.Family >= 1 && l.Family <= linuxsockapi.MaxSockets && l.System >= l.Family && l.System <= MaxSystemSockets &&
		l.Reserve >= 0 && l.Reserve <= l.Family && MaxFamilies*l.Reserve <= l.System
}

// opLimit bounds the operations: every family's requests, two internal
// operations per socket (an error fetch and a readable refresh), one
// accept ahead per socket, the data plane's and the warm pools'.
func (l Limits) opLimit() int {
	return MaxFamilies*FamilyOps + 3*l.System + dataOps + warmOps
}

// SetLimits replaces the quotas. It refuses quotas that are not Valid, and
// any change while a socket is open.
func (s *Service) SetLimits(l Limits) bool {
	if !l.Valid() || s.sockets.live != 0 {
		return false
	}
	s.limits = l
	s.sockets.reserved = 0
	for index := range s.families {
		if s.families[index].used {
			s.sockets.reserved += l.Reserve
		}
	}
	return true
}

// Limits returns the quotas in force.
func (s *Service) Limits() Limits { return s.limits }

// socketTable holds the sockets. live counts the used ones; reserved sums
// what every live family still lacks of its reserve.
type socketTable struct {
	chunks   []*[socketChunk]socket
	free     []int16
	live     int
	reserved int
}

func (s *Service) sock(global int) *socket {
	return &s.sockets.chunks[global/socketChunk][global%socketChunk]
}

// socketCap is the sockets allocated so far, used or free. Scans stop
// there, not at the quota.
func (s *Service) socketCap() int { return len(s.sockets.chunks) * socketChunk }

// lacking is what a family of count sockets still lacks of its reserve.
func (s *Service) lacking(count int) int { return max(0, s.limits.Reserve-count) }

// admit says whether family index may hold one more socket.
func (s *Service) admit(index int) linuxsockapi.Result {
	f := &s.families[index]
	if f.count >= s.limits.Family || f.freeCount == 0 {
		return linuxsockapi.ResultFamilyLimit
	}
	others := s.sockets.reserved - s.lacking(f.count)
	if s.sockets.live+1+others > s.limits.System {
		return linuxsockapi.ResultServiceLimit
	}
	return linuxsockapi.ResultOK
}

// takeGlobal pops a free global index, allocating a chunk when none is
// free.
func (s *Service) takeGlobal() int {
	t := &s.sockets
	if len(t.free) == 0 {
		if s.socketCap()+socketChunk > MaxSystemSockets {
			return -1
		}
		base := s.socketCap()
		t.chunks = append(t.chunks, new([socketChunk]socket))
		for i := socketChunk - 1; i >= 0; i-- {
			t.free = append(t.free, int16(base+i))
		}
	}
	global := int(t.free[len(t.free)-1])
	t.free = t.free[:len(t.free)-1]
	return global
}

// takeSlot and putSlot keep a family's free index stack. Indexes come
// back on top, so a released index is the next one taken, under a new
// generation.
func (f *family) takeSlot() int {
	f.freeCount--
	return int(f.freeSlots[f.freeCount])
}

func (f *family) putSlot(slot int) {
	f.freeSlots[f.freeCount] = uint16(slot)
	f.freeCount++
}

func (f *family) initSlots() {
	for slot := 0; slot < linuxsockapi.MaxSockets; slot++ {
		f.freeSlots[slot] = uint16(linuxsockapi.MaxSockets - 1 - slot)
	}
	f.freeCount = linuxsockapi.MaxSockets
}

// countSocket and uncountSocket move a family's use and the reserve sum
// together.
func (s *Service) countSocket(f *family) {
	s.sockets.reserved -= s.lacking(f.count)
	f.count++
	s.sockets.reserved += s.lacking(f.count)
	s.sockets.live++
}

func (s *Service) uncountSocket(f *family) {
	s.sockets.reserved -= s.lacking(f.count)
	f.count--
	s.sockets.reserved += s.lacking(f.count)
	s.sockets.live--
}

// opTable holds the operations the same way.
type opTable struct {
	chunks []*[opChunk]op
	free   []int16
}

func (s *Service) op(index int) *op { return &s.ops.chunks[index/opChunk][index%opChunk] }

func (s *Service) opCap() int { return len(s.ops.chunks) * opChunk }

// takeOp pops a free operation, allocating a chunk within opLimit when
// none is free.
func (s *Service) takeOp() *op {
	t := &s.ops
	if len(t.free) == 0 {
		if s.opCap() >= s.limits.opLimit() {
			return nil
		}
		base := s.opCap()
		chunk := new([opChunk]op)
		for i := range chunk {
			chunk[i].index = int16(base + i)
		}
		t.chunks = append(t.chunks, chunk)
		for i := opChunk - 1; i >= 0; i-- {
			t.free = append(t.free, int16(base+i))
		}
	}
	o := s.op(int(t.free[len(t.free)-1]))
	t.free = t.free[:len(t.free)-1]
	return o
}

func (s *Service) putOp(o *op) { s.ops.free = append(s.ops.free, o.index) }

// The largest operation table fits the int16 index and the tag's 16 bits.
var _ [1<<15 - (MaxFamilies*FamilyOps + 3*MaxSystemSockets + dataOps + warmOps + opChunk)]byte
