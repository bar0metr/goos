package loader

// MaxImageExtent bounds the offset and the length of an executable named
// inside an image object. Both must fit in 32 bits so that together they form
// one shared-page cache key; the system image itself is far smaller (ADR 0087).
const MaxImageExtent = 1<<32 - 1

// ImageExtent returns the bytes a launch names inside an image object, and the
// key the shared-page cache files their read-only segments under.
//
// The caller has already checked the authority to execute from the image; this
// checks only that the extent lies inside it. Whether the bytes are a native
// image is for the loader's validator to decide, so an extent that covers a
// data file or straddles two members is refused there, not here.
//
// The key is the extent itself. The image is immutable for the whole boot, so
// one extent always names the same bytes and a cached segment can never be
// served for different contents. Two extents that begin at the same offset
// with different lengths get different keys, so a caller cannot make a short
// extent hit frames that a longer one filled.
func ImageExtent(image []byte, offset uint64, size uint64) ([]byte, uint64, bool) {
	if size == 0 || offset > MaxImageExtent || size > MaxImageExtent || offset > uint64(len(image)) || size > uint64(len(image))-offset {
		return nil, 0, false
	}
	return image[offset : offset+size], offset<<32 | size, true
}
