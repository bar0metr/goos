//go:build goos_userspace

package arena

import (
	"sync/atomic"
	"unsafe"
)

//go:extern _goos_image_start
var imageStart [0]byte

// imageSum keeps the reads in loadImage from being optimized away: link-time
// optimization drops plain stores to a global nothing reads, and the loads
// with them. An atomic add stays.
var imageSum atomic.Uint32

// loadImage reads one byte of every page from the start of the image to the
// end of its globals, so the kernel brings each page in now.
func loadImage() {
	start := uintptr(unsafe.Pointer(&imageStart)) &^ (pageBytes - 1)
	end := uintptr(unsafe.Pointer(&globalsEnd))
	sum := byte(0)
	for address := start; address < end; address += pageBytes {
		// SAFETY: every page from the image start to the end of the
		// globals belongs to a segment of this program's image, mapped
		// readable for its whole life.
		sum += *(*byte)(pointerAt(address))
	}
	imageSum.Add(uint32(sum))
}
