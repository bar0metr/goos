//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tty"
	userwait "goos/userspace/wait"
)

var resizeHandle handle.ID
var resizeSnapshot tty.Snapshot
var resizeWire [tty.ResizeFrameSize]byte

func bindResize(data startup.Data) {
	resizeHandle, _ = data.Handle(tty.ResizeRole)
	if geometry, valid := TerminalGeometry(data); valid {
		resizeSnapshot.Apply(geometry)
	}
}

func replaceResize(endpoint handle.ID) {
	if resizeHandle != 0 {
		sys.HandleClose(resizeHandle)
	}
	resizeHandle = endpoint
}

// TakeResize drains a bounded batch and applies only its newest sequence.
// The snapshot survives endpoint replacement and late startup snapshots.
func TakeResize() (tty.Geometry, bool) {
	changed := false
	for budget := 0; resizeHandle != 0 && budget < 8; budget++ {
		count, status := sys.StreamRead(resizeHandle, resizeWire[:])
		if status == kcall.StatusResourceExhausted {
			break
		}
		if status != kcall.StatusOK || count != len(resizeWire) {
			replaceResize(0)
			break
		}
		geometry, valid := tty.DecodeResizeFrame(resizeWire[:])
		if valid && geometry.Authoritative && geometry.ValidRemote() && resizeSnapshot.Apply(geometry) {
			changed = true
		}
	}
	geometry, _ := resizeSnapshot.Current()
	return geometry, changed
}

// ReadTerminalWithin waits for input or resize in one native WaitAny. Remote
// callers use an infinite deadline; serial callers keep cursor-position probes.
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
		if resizeHandle != 0 {
			interests[count] = userwait.Interest{Handle: resizeHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
		}
		if _, status := userwait.Any(&interests, count, deadline); status != kcall.StatusOK {
			if status == kcall.StatusTimeout {
				status = kcall.StatusResourceExhausted
			}
			return uint64(status), 0, tty.Geometry{}, false
		}
	}
}
