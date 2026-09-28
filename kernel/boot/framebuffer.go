package boot

const (
	MinFramebufferWidth  = uint32(640)
	MinFramebufferHeight = uint32(400)
	MaxFramebufferWidth  = uint32(8192)
	MaxFramebufferHeight = uint32(8192)
	MaxFramebufferSize   = uint64(256 << 20)
)

// PixelFormat describes the byte layout of one 32-bit framebuffer pixel.
// Firmware-specific GOP values never cross the boot package boundary.
type PixelFormat uint8

const (
	PixelFormatUnknown PixelFormat = iota
	PixelFormatRGBX8888
	PixelFormatBGRX8888
	PixelFormatBitMask
)

// FramebufferInfo is the immutable, architecture-neutral framebuffer handoff.
// Address is physical until the kernel installs its permanent mapping.
type FramebufferInfo struct {
	Address       uint64
	Size          uint64
	Width         uint32
	Height        uint32
	PixelsPerScan uint32
	BytesPerPixel uint8
	Format        PixelFormat
	RedMask       uint32
	GreenMask     uint32
	BlueMask      uint32
	ReservedMask  uint32
	Available     bool
}

// VisibleBytes returns the number of bytes required for every scan line,
// including firmware padding. A zero result means the arithmetic overflowed.
func (framebuffer FramebufferInfo) VisibleBytes() uint64 {
	row := uint64(framebuffer.PixelsPerScan) * uint64(framebuffer.BytesPerPixel)
	visible := row * uint64(framebuffer.Height)
	if framebuffer.Height != 0 && visible/uint64(framebuffer.Height) != row {
		return 0
	}
	return visible
}

// Valid treats every field as untrusted firmware input. It is deliberately
// stricter than the drawing code needs so later boot stages can trust the
// descriptor without retaining any UEFI object.
func (framebuffer FramebufferInfo) Valid() bool {
	if !framebuffer.Available || framebuffer.Address == 0 || framebuffer.Size == 0 {
		return false
	}
	if framebuffer.Width < MinFramebufferWidth || framebuffer.Width > MaxFramebufferWidth ||
		framebuffer.Height < MinFramebufferHeight || framebuffer.Height > MaxFramebufferHeight ||
		framebuffer.PixelsPerScan < framebuffer.Width || framebuffer.BytesPerPixel != 4 {
		return false
	}
	if framebuffer.Size > MaxFramebufferSize || framebuffer.Address+framebuffer.Size < framebuffer.Address {
		return false
	}
	visible := framebuffer.VisibleBytes()
	if visible == 0 || visible > framebuffer.Size {
		return false
	}

	switch framebuffer.Format {
	case PixelFormatRGBX8888:
		return framebuffer.RedMask == 0x000000ff && framebuffer.GreenMask == 0x0000ff00 &&
			framebuffer.BlueMask == 0x00ff0000 && framebuffer.ReservedMask == 0xff000000
	case PixelFormatBGRX8888:
		return framebuffer.RedMask == 0x00ff0000 && framebuffer.GreenMask == 0x0000ff00 &&
			framebuffer.BlueMask == 0x000000ff && framebuffer.ReservedMask == 0xff000000
	case PixelFormatBitMask:
		return validPixelMasks(framebuffer.RedMask, framebuffer.GreenMask, framebuffer.BlueMask, framebuffer.ReservedMask)
	default:
		return false
	}
}

func validPixelMasks(red, green, blue, reserved uint32) bool {
	if red == 0 || green == 0 || blue == 0 {
		return false
	}
	return red&green == 0 && red&blue == 0 && red&reserved == 0 &&
		green&blue == 0 && green&reserved == 0 && blue&reserved == 0
}
