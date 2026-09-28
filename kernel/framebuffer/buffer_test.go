package framebuffer

import (
	"goos/kernel/boot"
	"testing"
)

func testBuffer(width, height, stride int) (Buffer, []uint32, bool) {
	pixels := make([]uint32, stride*height)
	info := boot.FramebufferInfo{
		Address: 0x80000000, Size: uint64(len(pixels) * 4),
		Width: uint32(width), Height: uint32(height), PixelsPerScan: uint32(stride), BytesPerPixel: 4,
		Format: boot.PixelFormatRGBX8888, RedMask: 0xff, GreenMask: 0xff00,
		BlueMask: 0xff0000, ReservedMask: 0xff000000, Available: true,
	}
	buffer, ok := newBuffer(pixels, info)
	return buffer, pixels, ok
}

func TestPutPixelHonoursVisibleBoundsAndStride(t *testing.T) {
	buffer, pixels, ok := testBuffer(640, 400, 644)
	if !ok {
		t.Fatal("valid buffer rejected")
	}
	buffer.PutPixel(0, 0, 1)
	buffer.PutPixel(639, 399, 2)
	buffer.PutPixel(640, 0, 3)
	buffer.PutPixel(-1, 0, 4)
	if pixels[0] != 1 || pixels[399*644+639] != 2 {
		t.Fatal("visible edge pixels were not written")
	}
	for row := 0; row < 400; row++ {
		for column := 640; column < 644; column++ {
			if pixels[row*644+column] != 0 {
				t.Fatalf("stride padding changed at row %d column %d", row, column)
			}
		}
	}
}

func TestFillRectClipsWithoutOverflow(t *testing.T) {
	buffer, pixels, ok := testBuffer(640, 400, 640)
	if !ok {
		t.Fatal("valid buffer rejected")
	}
	buffer.FillRect(-2, -1, 4, 3, 7)
	for _, index := range []int{0, 1, 640, 641} {
		if pixels[index] != 7 {
			t.Fatalf("clipped pixel %d not filled", index)
		}
	}
	buffer.FillRect(10, 10, 0, 10, 9)
	buffer.FillRect(10, 10, 10, 0, 9)
	buffer.FillRect(640, 400, 10, 10, 9)
	buffer.FillRect(-int(^uint(0)>>1)-1, -int(^uint(0)>>1)-1, int(^uint(0)>>1), int(^uint(0)>>1), 9)
	if pixels[10*640+10] != 0 || pixels[len(pixels)-1] != 0 {
		t.Fatal("empty or outside rectangle changed pixels")
	}
}

func TestClearLeavesStridePaddingUntouched(t *testing.T) {
	buffer, pixels, ok := testBuffer(640, 400, 643)
	if !ok {
		t.Fatal("valid buffer rejected")
	}
	buffer.Clear(0x123456)
	for row := 0; row < 400; row++ {
		if pixels[row*643] != 0x123456 || pixels[row*643+639] != 0x123456 {
			t.Fatalf("visible row %d was not cleared", row)
		}
		if pixels[row*643+640] != 0 || pixels[row*643+642] != 0 {
			t.Fatalf("padding row %d was modified", row)
		}
	}
}

func TestColorUsesFramebufferMasks(t *testing.T) {
	buffer, _, ok := testBuffer(640, 400, 640)
	if !ok {
		t.Fatal("valid buffer rejected")
	}
	if pixel := buffer.Color(0x12, 0x34, 0x56); pixel != 0x00563412 {
		t.Fatalf("RGBX pixel = %#x", pixel)
	}
	buffer.red, buffer.green, buffer.blue = 0x3ff, 0xffc00, 0x3ff00000
	if pixel := buffer.Color(255, 255, 255); pixel != 0x3fffffff {
		t.Fatalf("10:10:10 pixel = %#x", pixel)
	}
}

func TestNewBufferRejectsInvalidGeometry(t *testing.T) {
	if _, _, ok := testBuffer(641, 400, 640); ok {
		t.Fatal("stride shorter than width accepted")
	}
	pixels := make([]uint32, 640*399)
	info := boot.FramebufferInfo{
		Address: 0x80000000, Size: 640 * 400 * 4, Width: 640, Height: 400, PixelsPerScan: 640,
		BytesPerPixel: 4, Format: boot.PixelFormatRGBX8888, RedMask: 0xff, GreenMask: 0xff00,
		BlueMask: 0xff0000, ReservedMask: 0xff000000, Available: true,
	}
	if _, ok := newBuffer(pixels, info); ok {
		t.Fatal("short backing memory accepted")
	}
}
