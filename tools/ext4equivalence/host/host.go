package host

import (
	"sync"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
)

type Interest struct {
	Handle handle.ID
	Events kcall.WaitEvent
}
type Result struct{ ReadyMask uint64 }

const MaxObjects = kcall.WaitAnyMaxObjects
const VFS handle.ID = 1
const Block handle.ID = 2
const Reader handle.ID = 3
const Writer handle.ID = 4

var Requests chan []byte
var Replies chan []byte
var changed chan struct{}
var mu sync.Mutex
var wake int
var stopped bool
var batching bool

func BeginBatch() { mu.Lock(); batching = true; mu.Unlock() }
func EndBatch()   { mu.Lock(); batching = false; mu.Unlock(); notify() }

func Reset() {
	Requests = make(chan []byte, 256)
	Replies = make(chan []byte, 256)
	changed = make(chan struct{}, 1)
	mu.Lock()
	wake = 0
	stopped = false
	batching = false
	mu.Unlock()
}
func notify() {
	select {
	case changed <- struct{}{}:
	default:
	}
}
func Submit(data []byte) { Requests <- append([]byte(nil), data...); notify() }
func Stop()              { mu.Lock(); stopped = true; mu.Unlock(); notify() }

func allowedEvents(id handle.ID) kcall.WaitEvent {
	switch id {
	case VFS, Block:
		return kcall.WaitReadable | kcall.WaitWritable | kcall.WaitPeerClosed
	case Reader:
		return kcall.WaitReadable | kcall.WaitPeerClosed
	case Writer:
		return kcall.WaitWritable | kcall.WaitPeerClosed
	}
	return 0
}

func Any(interests *[MaxObjects]Interest, count int, timeout uint64) (Result, kcall.Status) {
	if interests == nil || count < 0 || count > MaxObjects {
		return Result{}, kcall.StatusInvalidArgument
	}
	for i := 0; i < count; i++ {
		allowed := allowedEvents(interests[i].Handle)
		// Native resolveWaitAny denies unknown handles and invalid event sets.
		if allowed == 0 || interests[i].Events == 0 || interests[i].Events & ^allowed != 0 {
			return Result{}, kcall.StatusPermissionDenied
		}
	}
	for {
		var mask uint64
		mu.Lock()
		for i := 0; i < count; i++ {
			var ready kcall.WaitEvent
			switch interests[i].Handle {
			case VFS:
				if !batching && len(Requests) > 0 {
					ready |= kcall.WaitReadable
				}
				if len(Replies) < cap(Replies) {
					ready |= kcall.WaitWritable
				}
			case Block:
				if stopped {
					ready |= kcall.WaitPeerClosed
				} else {
					ready |= kcall.WaitWritable
				}
			case Reader:
				if wake > 0 {
					ready |= kcall.WaitReadable
				}
			case Writer:
				ready |= kcall.WaitWritable
			}
			if ready&interests[i].Events != 0 {
				mask |= 1 << i
			}
		}
		mu.Unlock()
		if mask != 0 {
			return Result{mask}, kcall.StatusOK
		}
		if timeout == 0 {
			return Result{}, kcall.StatusTimeout
		}
		<-changed
	}
}
func IPCReceive(id handle.ID, target []byte) kcall.Status {
	switch id {
	case VFS:
	case Block:
		mu.Lock()
		closed := stopped
		mu.Unlock()
		if closed {
			return kcall.StatusPeerDied
		}
		return kcall.StatusNotFound
	default:
		return kcall.StatusPermissionDenied
	}
	if len(target) < ipc.WireMaxSize {
		return kcall.StatusInvalidArgument
	}
	select {
	case data := <-Requests:
		copy(target, data)
		return kcall.StatusOK
	default:
		return kcall.StatusNotFound
	}
}
func IPCSend(id handle.ID, data []byte) kcall.Status {
	if id == Block {
		// The fixture supplies block reads through Device, not a block peer.
		return kcall.StatusUnsupported
	}
	if id != VFS {
		return kcall.StatusPermissionDenied
	}
	select {
	case Replies <- append([]byte(nil), data...):
		return kcall.StatusOK
	default:
		return kcall.StatusResourceExhausted
	}
}
func StreamCreate() (handle.ID, handle.ID, kcall.Status) { return Reader, Writer, kcall.StatusOK }
func StreamWrite(id handle.ID, data []byte) (uint64, kcall.Status) {
	if id != Writer {
		return 0, kcall.StatusPermissionDenied
	}
	mu.Lock()
	wake += len(data)
	mu.Unlock()
	notify()
	return uint64(len(data)), kcall.StatusOK
}
func StreamRead(id handle.ID, data []byte) (uint64, kcall.Status) {
	if id != Reader {
		return 0, kcall.StatusPermissionDenied
	}
	mu.Lock()
	defer mu.Unlock()
	n := min(wake, len(data))
	wake -= n
	if n == 0 {
		return 0, kcall.StatusResourceExhausted
	}
	return uint64(n), kcall.StatusOK
}
func Warn(string, ...any) bool { return true }

type ExitCode int

func Exit(code uint64) { panic(ExitCode(code)) }
