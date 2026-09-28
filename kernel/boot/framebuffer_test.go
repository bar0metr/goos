package boot

import "testing"

func validFramebuffer() FramebufferInfo {
	return FramebufferInfo{
		Address:       0x80000000,
		Size:          1024 * 768 * 4,
		Width:         1024,
		Height:        768,
		PixelsPerScan: 1024,
		BytesPerPixel: 4,
		Format:        PixelFormatRGBX8888,
		RedMask:       0x000000ff,
		GreenMask:     0x0000ff00,
		BlueMask:      0x00ff0000,
		ReservedMask:  0xff000000,
		Available:     true,
	}
}

func TestFramebufferInfoAcceptsValidatedGeometry(t *testing.T) {
	framebuffer := validFramebuffer()
	framebuffer.PixelsPerScan = 1056
	framebuffer.Size = uint64(framebuffer.PixelsPerScan) * uint64(framebuffer.Height) * 4
	if !framebuffer.Valid() {
		t.Fatal("valid framebuffer rejected")
	}
	if framebuffer.VisibleBytes() != framebuffer.Size {
		t.Fatalf("visible bytes = %d, want %d", framebuffer.VisibleBytes(), framebuffer.Size)
	}
}

func TestFramebufferInfoRejectsUntrustedMetadata(t *testing.T) {
	tests := []struct {
		name   string
		change func(*FramebufferInfo)
	}{
		{"unavailable", func(value *FramebufferInfo) { value.Available = false }},
		{"zero address", func(value *FramebufferInfo) { value.Address = 0 }},
		{"zero size", func(value *FramebufferInfo) { value.Size = 0 }},
		{"narrow", func(value *FramebufferInfo) { value.Width = 639 }},
		{"short", func(value *FramebufferInfo) { value.Height = 399 }},
		{"wide", func(value *FramebufferInfo) { value.Width = 8193; value.PixelsPerScan = 8193 }},
		{"tall", func(value *FramebufferInfo) { value.Height = 8193 }},
		{"short stride", func(value *FramebufferInfo) { value.PixelsPerScan = value.Width - 1 }},
		{"wrong depth", func(value *FramebufferInfo) { value.BytesPerPixel = 3 }},
		{"oversized", func(value *FramebufferInfo) { value.Size = 256<<20 + 1 }},
		{"address wrap", func(value *FramebufferInfo) { value.Address = ^uint64(0) - value.Size + 1 }},
		{"truncated", func(value *FramebufferInfo) { value.Size-- }},
		{"unknown format", func(value *FramebufferInfo) { value.Format = PixelFormatUnknown }},
		{"wrong standard masks", func(value *FramebufferInfo) { value.RedMask = 0xff000000 }},
		{"overlapping masks", func(value *FramebufferInfo) {
			value.Format = PixelFormatBitMask
			value.RedMask = 0xff
			value.GreenMask = 0xff
			value.BlueMask = 0xff0000
			value.ReservedMask = 0xff000000
		}},
		{"missing colour mask", func(value *FramebufferInfo) {
			value.Format = PixelFormatBitMask
			value.RedMask = 0xff
			value.GreenMask = 0xff00
			value.BlueMask = 0
			value.ReservedMask = 0xff000000
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			framebuffer := validFramebuffer()
			test.change(&framebuffer)
			if framebuffer.Valid() {
				t.Fatalf("accepted invalid framebuffer: %#v", framebuffer)
			}
		})
	}
}

func TestFramebufferInfoAcceptsNonOverlappingBitMasks(t *testing.T) {
	framebuffer := validFramebuffer()
	framebuffer.Format = PixelFormatBitMask
	framebuffer.RedMask = 0x000003ff
	framebuffer.GreenMask = 0x000ffc00
	framebuffer.BlueMask = 0x3ff00000
	framebuffer.ReservedMask = 0xc0000000
	if !framebuffer.Valid() {
		t.Fatal("valid 10:10:10:2 masks rejected")
	}
}

func TestFramebufferMappingRangeCoversUnalignedVisibleBytes(t *testing.T) {
	framebuffer := validFramebuffer()
	framebuffer.Address += 123
	framebuffer.Size += 123
	start, end, ok := framebuffer.MappingRange()
	if !ok {
		t.Fatal("valid unaligned framebuffer range rejected")
	}
	if start != 0x80000000 || end != 0x80301000 {
		t.Fatalf("mapping range = [%#x, %#x), want [0x80000000, 0x80301000)", start, end)
	}
}

func TestFramebufferUsableMemoryOverlap(t *testing.T) {
	framebuffer := validFramebuffer()
	start, end, _ := framebuffer.MappingRange()
	tests := []struct {
		name    string
		regions []MemoryRegion
		overlap bool
	}{
		{"reserved owner", []MemoryRegion{{Start: start, Pages: (end - start) / PageSize, Kind: MemoryReserved}}, false},
		{"usable first page", []MemoryRegion{{Start: start, Pages: 1, Kind: MemoryUsable}}, true},
		{"usable last page", []MemoryRegion{{Start: end - PageSize, Pages: 1, Kind: MemoryUsable}}, true},
		{"usable after", []MemoryRegion{{Start: end, Pages: 1, Kind: MemoryUsable}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := framebuffer.OverlapsUsable(test.regions); got != test.overlap {
				t.Fatalf("overlap = %t, want %t", got, test.overlap)
			}
		})
	}
}
