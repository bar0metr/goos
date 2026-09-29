package loader

// Origin names where an image's bytes live, so a page can be read from there
// when a process first touches it (ADR 0106): an extent of the system image,
// by its key, or an executable object, by its identity. Exactly one field is
// set for a lazy image.
type Origin struct {
	Key    uint64
	Object uint64
}

// Valid reports whether the origin names one unambiguous lazy source.
func (origin Origin) Valid() bool { return (origin.Key == 0) != (origin.Object == 0) }

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
	page     func(index uint64) ([]byte, bool)
	pageSize uint64
	size     uint64
}

// Contiguous is a source whose bytes lie in one slice.
func Contiguous(data []byte) Source {
	return Source{data: data, size: uint64(len(data))}
}

// Paged is a source of size bytes held in whole pages of pageSize bytes that
// page returns by number; the bytes past size in the last page are not part
// of it. page must be a function that captures nothing: the kernel's heap
// never frees.
func Paged(page func(index uint64) ([]byte, bool), pageSize uint64, size uint64) (Source, bool) {
	if page == nil || pageSize == 0 || size == 0 {
		return Source{}, false
	}
	return Source{page: page, pageSize: pageSize, size: size}, true
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
	if source.page == nil {
		return source.data[offset : offset+length], true
	}
	if length == 0 {
		return nil, true
	}
	pageSize := source.pageSize
	first, within := offset/pageSize, offset%pageSize
	if within+length <= pageSize {
		page, ok := source.page(first)
		if !ok || uint64(len(page)) != pageSize {
			return nil, false
		}
		return page[within : within+length], true
	}
	if uint64(len(scratch)) < length {
		return nil, false
	}
	for copied := uint64(0); copied < length; {
		index, within := (offset+copied)/pageSize, (offset+copied)%pageSize
		page, ok := source.page(index)
		if !ok || uint64(len(page)) != pageSize {
			return nil, false
		}
		copied += uint64(copy(scratch[copied:length], page[within:]))
	}
	return scratch[:length], true
}

// CopyOut copies the length bytes at offset into target, which must hold
// them, a page at a time.
func (source Source) CopyOut(offset uint64, target []byte) bool {
	length := uint64(len(target))
	if offset > source.size || length > source.size-offset {
		return false
	}
	if source.page == nil {
		copy(target, source.data[offset:offset+length])
		return true
	}
	for copied := uint64(0); copied < length; {
		index, within := (offset+copied)/source.pageSize, (offset+copied)%source.pageSize
		page, ok := source.page(index)
		if !ok || uint64(len(page)) != source.pageSize {
			return false
		}
		copied += uint64(copy(target[copied:], page[within:]))
	}
	return true
}
