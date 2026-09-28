package framebuffer

import (
	"goos/kernel/boot"
	"math/bits"
)

// Buffer owns rendering access to one validated linear framebuffer. The raw
// memory slice is deliberately private to this package.
type Buffer struct {
	pixels []uint32
	width  int
	height int
	stride int
	red    uint32
	green  uint32
	blue   uint32
}

func newBuffer(pixels []uint32, info boot.FramebufferInfo) (Buffer, bool) {
	if !info.Valid() || uint64(int(info.Width)) != uint64(info.Width) ||
		uint64(int(info.Height)) != uint64(info.Height) || uint64(int(info.PixelsPerScan)) != uint64(info.PixelsPerScan) {
		return Buffer{}, false
	}
	width, height, stride := int(info.Width), int(info.Height), int(info.PixelsPerScan)
	if stride <= 0 || height <= 0 || height > len(pixels)/stride {
		return Buffer{}, false
	}
	needed := stride * height
	return Buffer{
		pixels: pixels[:needed:needed], width: width, height: height, stride: stride,
		red: info.RedMask, green: info.GreenMask, blue: info.BlueMask,
	}, true
}

func (buffer *Buffer) Width() int {
	if buffer == nil {
		return 0
	}
	return buffer.width
}

func (buffer *Buffer) Height() int {
	if buffer == nil {
		return 0
	}
	return buffer.height
}

// Color converts one RGB colour to the framebuffer's validated mask layout.
func (buffer *Buffer) Color(red, green, blue uint8) uint32 {
	if buffer == nil {
		return 0
	}
	return channel(red, buffer.red) | channel(green, buffer.green) | channel(blue, buffer.blue)
}

func channel(value uint8, mask uint32) uint32 {
	count := bits.OnesCount32(mask)
	if count == 0 {
		return 0
	}
	maximum := uint64(1)<<count - 1
	scaled := uint64(value) * maximum / 255
	result := uint32(0)
	index := 0
	for bit := 0; bit < 32; bit++ {
		position := uint32(1) << bit
		if mask&position == 0 {
			continue
		}
		if scaled&(uint64(1)<<index) != 0 {
			result |= position
		}
		index++
	}
	return result
}

func (buffer *Buffer) Clear(pixel uint32) {
	if buffer == nil {
		return
	}
	buffer.FillRect(0, 0, buffer.width, buffer.height, pixel)
}

func (buffer *Buffer) PutPixel(x, y int, pixel uint32) {
	if buffer == nil || x < 0 || y < 0 || x >= buffer.width || y >= buffer.height {
		return
	}
	buffer.pixels[y*buffer.stride+x] = pixel
}

// FillRect clips the rectangle to the visible area. Width and height are
// reduced before their endpoints are formed, so hostile int values cannot
// wrap an index.
func (buffer *Buffer) FillRect(x, y, width, height int, pixel uint32) {
	if buffer == nil || width <= 0 || height <= 0 || buffer.width <= 0 || buffer.height <= 0 {
		return
	}
	if x < 0 {
		width += x
		x = 0
	}
	if y < 0 {
		height += y
		y = 0
	}
	if width <= 0 || height <= 0 || x >= buffer.width || y >= buffer.height {
		return
	}
	if width > buffer.width-x {
		width = buffer.width - x
	}
	if height > buffer.height-y {
		height = buffer.height - y
	}
	for row := y; row < y+height; row++ {
		start := row*buffer.stride + x
		for column := 0; column < width; column++ {
			buffer.pixels[start+column] = pixel
		}
	}
}
