package uefi

import "goos/kernel/boot"

const (
	gopPixelRedGreenBlueReserved8BitPerColor uint32 = iota
	gopPixelBlueGreenRedReserved8BitPerColor
	gopPixelBitMask
	gopPixelBltOnly
)

type gopPixelMasks struct {
	red      uint32
	green    uint32
	blue     uint32
	reserved uint32
}

// gopModeInformation mirrors EFI_GRAPHICS_OUTPUT_MODE_INFORMATION. It is used
// only while boot services and firmware-owned mode records remain valid.
type gopModeInformation struct {
	version           uint32
	horizontal        uint32
	vertical          uint32
	pixelFormat       uint32
	pixelInformation  gopPixelMasks
	pixelsPerScanLine uint32
}

type graphicsOutputProtocol struct {
	queryMode uintptr
	setMode   uintptr
	blt       uintptr
	mode      *graphicsOutputProtocolMode
}

type graphicsOutputProtocolMode struct {
	maxMode         uint32
	mode            uint32
	info            *gopModeInformation
	sizeOfInfo      uintptr
	framebufferBase uint64
	framebufferSize uintptr
}

type gopModeChoice struct {
	number uint32
	rank   uint8
	valid  bool
}

func chooseGOPMode(best gopModeChoice, number, current uint32, info gopModeInformation) gopModeChoice {
	if !validGOPMode(info) {
		return best
	}
	rank := gopModeRank(info.horizontal, info.vertical, number == current)
	if !best.valid || rank < best.rank || rank == best.rank && number < best.number {
		return gopModeChoice{number: number, rank: rank, valid: true}
	}
	return best
}

func gopModeRank(width, height uint32, current bool) uint8 {
	switch {
	case width == 1024 && height == 768:
		return 0
	case width == 1280 && height == 720:
		return 1
	case width == 1280 && height == 800:
		return 2
	case width == 800 && height == 600:
		return 3
	case current:
		return 4
	default:
		return 5
	}
}

func validGOPMode(info gopModeInformation) bool {
	if info.version != 0 {
		return false
	}
	// A synthetic bounded range lets the architecture-neutral validator check
	// geometry and masks before mode selection; the real range is checked again
	// after SetMode exposes its base and reported size.
	_, ok := framebufferFromGOP(info, 1, boot.MaxFramebufferSize)
	return ok
}

func framebufferFromGOP(info gopModeInformation, address, size uint64) (boot.FramebufferInfo, bool) {
	format, red, green, blue, reserved, ok := normalizeGOPPixelFormat(info.pixelFormat, info.pixelInformation)
	if !ok {
		return boot.FramebufferInfo{}, false
	}
	framebuffer := boot.FramebufferInfo{
		Address:       address,
		Size:          size,
		Width:         info.horizontal,
		Height:        info.vertical,
		PixelsPerScan: info.pixelsPerScanLine,
		BytesPerPixel: 4,
		Format:        format,
		RedMask:       red,
		GreenMask:     green,
		BlueMask:      blue,
		ReservedMask:  reserved,
		Available:     true,
	}
	return framebuffer, framebuffer.Valid()
}

func normalizeGOPPixelFormat(format uint32, masks gopPixelMasks) (boot.PixelFormat, uint32, uint32, uint32, uint32, bool) {
	switch format {
	case gopPixelRedGreenBlueReserved8BitPerColor:
		return boot.PixelFormatRGBX8888, 0x000000ff, 0x0000ff00, 0x00ff0000, 0xff000000, true
	case gopPixelBlueGreenRedReserved8BitPerColor:
		return boot.PixelFormatBGRX8888, 0x00ff0000, 0x0000ff00, 0x000000ff, 0xff000000, true
	case gopPixelBitMask:
		return boot.PixelFormatBitMask, masks.red, masks.green, masks.blue, masks.reserved, true
	default:
		return boot.PixelFormatUnknown, 0, 0, 0, 0, false
	}
}
