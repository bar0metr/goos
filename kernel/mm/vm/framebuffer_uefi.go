//go:build uefi && amd64

package vm

import (
	"goos/kernel/boot"
	"goos/kernel/mm/pmm"
	"unsafe"
)

// MapFramebuffer installs a dedicated supervisor-only RW+NX+UC mapping and
// returns the virtual address corresponding exactly to info.Address.
func MapFramebuffer(allocator *pmm.Allocator, rootAddress uint64, info boot.FramebufferInfo) (uint64, bool) {
	mapping, ok := planFramebufferMapping(info)
	if !ok || allocator == nil || rootAddress%PageSize != 0 || rootAddress >= IdentityMapLimit {
		return 0, false
	}
	// SAFETY: rootAddress is the exclusively owned kernel root returned by
	// BuildIdentity. Slot 257 is reserved for this mapping and must be empty.
	root := (*table)(unsafe.Pointer(uintptr(rootAddress)))
	if root[framebufferSlot]&entryPresent != 0 {
		return 0, false
	}
	for physical := mapping.physicalStart; physical < mapping.physicalEnd; physical += PageSize {
		virtual := FramebufferBase + physical - mapping.physicalStart
		pdpt, ok := kernelTable(allocator, root, (virtual>>39)&0x1ff)
		if !ok {
			return 0, false
		}
		pd, ok := kernelTable(allocator, pdpt, (virtual>>30)&0x1ff)
		if !ok {
			return 0, false
		}
		pt, ok := kernelTable(allocator, pd, (virtual>>21)&0x1ff)
		if !ok {
			return 0, false
		}
		entry := &pt[(virtual>>12)&0x1ff]
		if *entry&entryPresent != 0 {
			return 0, false
		}
		value, ok := uncachedPageEntry(physical)
		if !ok {
			return 0, false
		}
		*entry = value
	}
	return mapping.virtualStart, true
}
