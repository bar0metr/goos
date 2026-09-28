package vm

import (
	"goos/kernel/boot"
	"testing"
)

func mappedFramebuffer() boot.FramebufferInfo {
	return boot.FramebufferInfo{
		Address: 0x80000123, Size: 1024*768*4 + 0x123,
		Width: 1024, Height: 768, PixelsPerScan: 1024, BytesPerPixel: 4,
		Format: boot.PixelFormatRGBX8888, RedMask: 0xff, GreenMask: 0xff00,
		BlueMask: 0xff0000, ReservedMask: 0xff000000, Available: true,
	}
}

func TestFramebufferWindowUsesDedicatedKernelSlot(t *testing.T) {
	if framebufferSlot != directMapSlot+1 || framebufferSlot == kernelLowSlot {
		t.Fatalf("framebuffer slot %d collides with direct-map %d or kernel-low %d", framebufferSlot, directMapSlot, kernelLowSlot)
	}
	if FramebufferBase < UserAddressLimit || FramebufferBase+FramebufferWindowSize < FramebufferBase {
		t.Fatalf("invalid framebuffer window [%#x, %#x)", FramebufferBase, FramebufferBase+FramebufferWindowSize)
	}
}

func TestPlanFramebufferMappingPreservesPhysicalOffset(t *testing.T) {
	mapping, ok := planFramebufferMapping(mappedFramebuffer())
	if !ok {
		t.Fatal("valid framebuffer mapping rejected")
	}
	if mapping.physicalStart != 0x80000000 || mapping.physicalEnd != 0x80301000 ||
		mapping.virtualStart != FramebufferBase+0x123 || mapping.visible != 1024*768*4 {
		t.Fatalf("unexpected mapping: %#v", mapping)
	}
}

func TestPlanFramebufferMappingRejectsPhysicalAddressOutsideMask(t *testing.T) {
	info := mappedFramebuffer()
	info.Address = addressMask + PageSize
	if _, ok := planFramebufferMapping(info); ok {
		t.Fatal("out-of-range physical framebuffer accepted")
	}
}
