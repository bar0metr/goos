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

// Source is the bytes of one executable: an extent of the system image, which
// is contiguous, or the pages of an executable object, which are not
// (ADR 0097). The loader reads both through it, so validation and loading see
// the same bytes whichever source they come from.
type Source struct {
	data     []byte
	pages    [][]byte
	pageSize uint64
	size     uint64
}

// Contiguous is a source whose bytes lie in one slice.
func Contiguous(data []byte) Source {
	return Source{data: data, size: uint64(len(data))}
}

// Paged is a source of size bytes held in whole pages of pageSize bytes, in
// order; the bytes past size in the last page are not part of it.
func Paged(pages [][]byte, pageSize uint64, size uint64) (Source, bool) {
	if pageSize == 0 || size == 0 || uint64(len(pages)) != (size+pageSize-1)/pageSize {
		return Source{}, false
	}
	for _, page := range pages {
		if uint64(len(page)) != pageSize {
			return Source{}, false
		}
	}
	return Source{pages: pages, pageSize: pageSize, size: size}, true
}

// Len is the number of bytes in the source.
func (source Source) Len() uint64 { return source.size }

// Read returns the length bytes at offset. Bytes that lie in one slice of the
// source are returned in place; bytes that cross a page are gathered into
// scratch, which must hold length bytes.
func (source Source) Read(offset uint64, length uint64, scratch []byte) ([]byte, bool) {
	if offset > source.size || length > source.size-offset {
		return nil, false
	}
	if source.pages == nil {
		return source.data[offset : offset+length], true
	}
	if length == 0 {
		return nil, true
	}
	pageSize := source.pageSize
	first, within := offset/pageSize, offset%pageSize
	if within+length <= pageSize {
		return source.pages[first][within : within+length], true
	}
	if uint64(len(scratch)) < length {
		return nil, false
	}
	for copied := uint64(0); copied < length; {
		page, within := (offset+copied)/pageSize, (offset+copied)%pageSize
		copied += uint64(copy(scratch[copied:length], source.pages[page][within:]))
	}
	return scratch[:length], true
}
