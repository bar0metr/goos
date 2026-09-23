package loader

const (
	// MaxSharedSegments and MaxSharedPages bound the read-only image pages the
	// kernel keeps. The system image is immutable and small, so the bound is a
	// guard rather than a working limit: a launch that does not fit loads a
	// private copy instead of failing.
	MaxSharedSegments = 128
	MaxSharedPages    = 1024
)

// SharedPages remembers the physical frames that hold the read-only segments
// of images from the immutable system image. Every process running an image
// maps the same frames rather than a private copy of its code (ADR 0081).
//
// The frames are filled once, before any process can see them, and are never
// mapped writable or freed: they belong to the system image, not to a process.
type SharedPages struct {
	segments [MaxSharedSegments]sharedSegment
	frames   [MaxSharedPages]uint64
	count    int
	used     int
	hits     uint64
}

type sharedSegment struct {
	key     uint64
	segment int
	first   int
	pages   int
}

// Shareable reports whether a segment may be backed by shared frames. Only a
// segment no process may write qualifies; the parser already refuses a segment
// that is both writable and executable.
func Shareable(segment Segment) bool { return segment.Flags&SegmentWritable == 0 }

// Lookup returns the frames recorded for segment of the image named by key.
// The page count must match, so a key reused for a different layout misses.
func (cache *SharedPages) Lookup(key uint64, segment int, pages int) ([]uint64, bool) {
	if key == 0 {
		return nil, false
	}
	for index := 0; index < cache.count; index++ {
		entry := cache.segments[index]
		if entry.key == key && entry.segment == segment && entry.pages == pages {
			cache.hits++
			return cache.frames[entry.first : entry.first+entry.pages], true
		}
	}
	return nil, false
}

// Reserve returns room for pages frames. The caller fills them and records
// them with Commit, or abandons them by not committing.
func (cache *SharedPages) Reserve(pages int) ([]uint64, bool) {
	if pages <= 0 || cache.count == len(cache.segments) || pages > len(cache.frames)-cache.used {
		return nil, false
	}
	return cache.frames[cache.used : cache.used+pages], true
}

// Commit records the frames last reserved as segment of the image named key.
func (cache *SharedPages) Commit(key uint64, segment int, pages int) bool {
	if key == 0 || pages <= 0 || cache.count == len(cache.segments) || pages > len(cache.frames)-cache.used {
		return false
	}
	cache.segments[cache.count] = sharedSegment{key: key, segment: segment, first: cache.used, pages: pages}
	cache.count++
	cache.used += pages
	return true
}

// Hits counts launches that mapped recorded frames instead of copying.
func (cache *SharedPages) Hits() uint64 { return cache.hits }

// Pages is how many frames the cache holds.
func (cache *SharedPages) Pages() int { return cache.used }
