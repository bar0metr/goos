package uefi

import (
	"goos/kernel/boot"
	"testing"
	"unsafe"
)

func testGOPMode(width, height uint32) gopModeInformation {
	return gopModeInformation{
		horizontal:        width,
		vertical:          height,
		pixelFormat:       gopPixelRedGreenBlueReserved8BitPerColor,
		pixelsPerScanLine: width,
	}
}

func TestGOPStructureLayoutsMatchUEFI(t *testing.T) {
	if size := unsafe.Sizeof(gopModeInformation{}); size != 36 {
		t.Fatalf("mode information size = %d, want 36", size)
	}
	if size := unsafe.Sizeof(graphicsOutputProtocol{}); size != 32 {
		t.Fatalf("protocol size = %d, want 32", size)
	}
	if size := unsafe.Sizeof(graphicsOutputProtocolMode{}); size != 40 {
		t.Fatalf("protocol mode size = %d, want 40", size)
	}
}

func TestGOPModeSelectionUsesSpecifiedPreferenceOrder(t *testing.T) {
	current := uint32(7)
	var choice gopModeChoice
	choice = chooseGOPMode(choice, 7, current, testGOPMode(1920, 1080))
	choice = chooseGOPMode(choice, 6, current, testGOPMode(800, 600))
	choice = chooseGOPMode(choice, 5, current, testGOPMode(1280, 800))
	choice = chooseGOPMode(choice, 4, current, testGOPMode(1280, 720))
	choice = chooseGOPMode(choice, 3, current, testGOPMode(1024, 768))
	if !choice.valid || choice.number != 5 || choice.rank != 0 {
		t.Fatalf("unexpected choice: %#v", choice)
	}
}

func TestGOPModeSelectionPrefersCurrentBeforeArbitraryMode(t *testing.T) {
	var choice gopModeChoice
	choice = chooseGOPMode(choice, 1, 2, testGOPMode(1920, 1080))
	choice = chooseGOPMode(choice, 2, 2, testGOPMode(1600, 900))
	if choice.number != 2 || choice.rank != 4 {
		t.Fatalf("unexpected choice: %#v", choice)
	}
}

func TestGOPModeSelectionRejectsUnsupportedModes(t *testing.T) {
	tests := []gopModeInformation{
		testGOPMode(639, 480),
		testGOPMode(1024, 399),
		testGOPMode(8193, 768),
		{version: 1, horizontal: 1024, vertical: 768, pixelsPerScanLine: 1024},
		{horizontal: 1024, vertical: 768, pixelsPerScanLine: 1000},
		{horizontal: 1024, vertical: 768, pixelsPerScanLine: 1024, pixelFormat: gopPixelBltOnly},
	}
	for number, mode := range tests {
		if choice := chooseGOPMode(gopModeChoice{}, uint32(number), 0, mode); choice.valid {
			t.Fatalf("accepted mode %d: %#v", number, mode)
		}
	}
}

func TestGOPModeSelectionSkipsPreferredModeWithInvalidMasks(t *testing.T) {
	preferred := testGOPMode(1024, 768)
	preferred.pixelFormat = gopPixelBitMask
	preferred.pixelInformation = gopPixelMasks{red: 0xff, green: 0xff, blue: 0xff0000}
	fallback := testGOPMode(800, 600)

	choice := chooseGOPMode(gopModeChoice{}, 1, 0, preferred)
	choice = chooseGOPMode(choice, 2, 0, fallback)
	if !choice.valid || choice.number != 2 {
		t.Fatalf("invalid preferred mode displaced fallback: %#v", choice)
	}
}

func TestFramebufferFromGOPNormalizesFormats(t *testing.T) {
	tests := []struct {
		firmware uint32
		internal boot.PixelFormat
		red      uint32
		blue     uint32
	}{
		{gopPixelRedGreenBlueReserved8BitPerColor, boot.PixelFormatRGBX8888, 0x000000ff, 0x00ff0000},
		{gopPixelBlueGreenRedReserved8BitPerColor, boot.PixelFormatBGRX8888, 0x00ff0000, 0x000000ff},
	}
	for _, test := range tests {
		mode := testGOPMode(1024, 768)
		mode.pixelFormat = test.firmware
		framebuffer, ok := framebufferFromGOP(mode, 0x80000000, 1024*768*4)
		if !ok || framebuffer.Format != test.internal || framebuffer.RedMask != test.red || framebuffer.BlueMask != test.blue {
			t.Fatalf("unexpected normalization: %#v, valid=%t", framebuffer, ok)
		}
	}
}

func TestFramebufferFromGOPValidatesBitMasksAndSize(t *testing.T) {
	mode := testGOPMode(1024, 768)
	mode.pixelFormat = gopPixelBitMask
	mode.pixelInformation = gopPixelMasks{red: 0xff, green: 0xff00, blue: 0xff0000, reserved: 0xff000000}
	if _, ok := framebufferFromGOP(mode, 0x80000000, 1024*768*4); !ok {
		t.Fatal("valid bit-mask framebuffer rejected")
	}
	mode.pixelInformation.green = 0xff
	if _, ok := framebufferFromGOP(mode, 0x80000000, 1024*768*4); ok {
		t.Fatal("overlapping masks accepted")
	}
	mode.pixelInformation.green = 0xff00
	if _, ok := framebufferFromGOP(mode, 0x80000000, 1024*768*4-1); ok {
		t.Fatal("truncated framebuffer accepted")
	}
}

func TestFramebufferFromGOPRejectsInvalidFirmwareMetadata(t *testing.T) {
	valid := testGOPMode(1024, 768)
	tests := []struct {
		name    string
		mode    gopModeInformation
		address uint64
		size    uint64
	}{
		{name: "zero address", mode: valid, size: 1024 * 768 * 4},
		{name: "zero size", mode: valid, address: 0x80000000},
		{name: "short stride", mode: func() gopModeInformation { mode := valid; mode.pixelsPerScanLine = 1023; return mode }(), address: 0x80000000, size: 1024 * 768 * 4},
		{name: "oversized allocation", mode: valid, address: 0x80000000, size: boot.MaxFramebufferSize + 1},
		{name: "address overflow", mode: valid, address: ^uint64(0) - 1024, size: 1024 * 768 * 4},
		{name: "unsupported format", mode: func() gopModeInformation { mode := valid; mode.pixelFormat = gopPixelBltOnly; return mode }(), address: 0x80000000, size: 1024 * 768 * 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := framebufferFromGOP(test.mode, test.address, test.size); ok {
				t.Fatal("invalid firmware metadata accepted")
			}
		})
	}
}
