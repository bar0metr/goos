package vm

import "goos/kernel/boot"

const (
	// FramebufferBase begins PML4 slot 257, immediately after the one-slot
	// direct map. The window is private to the kernel root and never copied to
	// process roots.
	FramebufferBase       = uint64(0xffff808000000000)
	FramebufferWindowSize = uint64(512 << 20)
	framebufferSlot       = (FramebufferBase >> 39) & 0x1ff
)

type framebufferMapping struct {
	physicalStart uint64
	physicalEnd   uint64
	virtualStart  uint64
	visible       uint64
}

func planFramebufferMapping(info boot.FramebufferInfo) (framebufferMapping, bool) {
	physicalStart, physicalEnd, ok := info.MappingRange()
	if !ok || physicalEnd-physicalStart > FramebufferWindowSize || physicalStart&^addressMask != 0 ||
		(physicalEnd-PageSize)&^addressMask != 0 {
		return framebufferMapping{}, false
	}
	offset := info.Address - physicalStart
	virtual := FramebufferBase + offset
	if virtual < FramebufferBase || virtual+info.VisibleBytes() < virtual ||
		virtual+info.VisibleBytes() > FramebufferBase+FramebufferWindowSize {
		return framebufferMapping{}, false
	}
	return framebufferMapping{
		physicalStart: physicalStart,
		physicalEnd:   physicalEnd,
		virtualStart:  virtual,
		visible:       info.VisibleBytes(),
	}, true
}
