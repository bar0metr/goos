//go:build uefi && amd64

package framebuffer

import (
	"goos/kernel/boot"
	"unsafe"
)

// New is the package's only raw framebuffer pointer boundary.
func New(address uint64, info boot.FramebufferInfo) (Buffer, bool) {
	if address == 0 || address%4 != 0 || !info.Valid() {
		return Buffer{}, false
	}
	count := uint64(info.PixelsPerScan) * uint64(info.Height)
	if count == 0 || count > uint64(^uint(0)>>1) {
		return Buffer{}, false
	}
	// SAFETY: Phase 2 mapped count writable uint32 pixels at address with
	// supervisor-only RW+NX permissions. The slice never leaves this package.
	pixels := unsafe.Slice((*uint32)(unsafe.Pointer(uintptr(address))), int(count))
	return newBuffer(pixels, info)
}
