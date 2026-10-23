//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/lock"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tty"
	userwait "goos/userspace/wait"
)

// resize is the terminal's resize endpoint and the newest geometry read from
// it. Its lock covers the endpoint, the snapshot and the frame buffer, and is
// never held across a wait.
var resize struct {
	mu       lock.Mutex
	endpoint handle.ID
	snapshot tty.Snapshot
	frame    [tty.ResizeFrameSize]byte
}

func bindResize(data startup.Data) {
	resize.mu.Lock()
	resize.endpoint, _ = data.Handle(tty.ResizeRole)
	if geometry, valid := TerminalGeometry(data); valid {
		resize.snapshot.Apply(geometry)
	}
	resize.mu.Unlock()
}

func replaceResize(endpoint handle.ID) {
	resize.mu.Lock()
	replaceResizeLocked(endpoint)
	resize.mu.Unlock()
}

func replaceResizeLocked(endpoint handle.ID) {
	if resize.endpoint != 0 {
		sys.HandleClose(resize.endpoint)
	}
	resize.endpoint = endpoint
}

// TakeResize drains a bounded batch and applies only its newest sequence.
// The snapshot survives endpoint replacement and late startup snapshots.
func TakeResize() (tty.Geometry, bool) {
	resize.mu.Lock()
	defer resize.mu.Unlock()
	changed := false
	for budget := 0; resize.endpoint != 0 && budget < 8; budget++ {
		count, status := sys.StreamRead(resize.endpoint, resize.frame[:])
		if status == kcall.StatusResourceExhausted {
			break
		}
		if status != kcall.StatusOK || count != len(resize.frame) {
			replaceResizeLocked(0)
			break
		}
		geometry, valid := tty.DecodeResizeFrame(resize.frame[:])
		if valid && geometry.Authoritative && geometry.ValidRemote() && resize.snapshot.Apply(geometry) {
			changed = true
		}
	}
	geometry, _ := resize.snapshot.Current()
	return geometry, changed
}

// ReadTerminalWithin waits for input or resize in one native WaitAny. Remote
// callers use an infinite deadline; serial callers keep cursor-position probes.
// The terminal has one reader: a program calls this from one goroutine.
func ReadTerminalWithin(target []byte, deadline uint64) (uint64, int, tty.Geometry, bool) {
	for {
		geometry, changed := TakeResize()
		count, status := sys.StreamRead(handle.ID(stdinHandle), target)
		if changed {
			if status != kcall.StatusOK {
				count = 0
			}
			return 0, count, geometry, true
		}
		if status != kcall.StatusResourceExhausted {
			return uint64(status), count, tty.Geometry{}, false
		}
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: handle.ID(stdinHandle), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count = 1
		resize.mu.Lock()
		if resize.endpoint != 0 {
			interests[count] = userwait.Interest{Handle: resize.endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
		}
		resize.mu.Unlock()
		if _, status := userwait.Any(&interests, count, deadline); status != kcall.StatusOK {
			if status == kcall.StatusTimeout {
				status = kcall.StatusResourceExhausted
			}
			return uint64(status), 0, tty.Geometry{}, false
		}
	}
}
