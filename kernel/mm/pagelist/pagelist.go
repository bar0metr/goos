// Package pagelist keeps an ordered list of physical frames for a kernel
// object whose size is not known at build time — an executable object, a
// shared-memory object (ADR 0106). The frame addresses are stored in index
// pages, 512 to a page, taken from the page allocator as the list grows, so an
// object costs a fixed root of MaxIndexPages entries plus one index page per
// 2 MiB it holds, instead of a fixed array sized for the largest object.
package pagelist

const (
	PageSize = uint64(4096)
	// FramesPerIndex is how many frame addresses one index page holds.
	FramesPerIndex = PageSize / 8
	// MaxIndexPages bounds one list: 128 index pages, 65536 frames, 256 MiB.
	// It is an intentional system limit on the size of one object.
	MaxIndexPages = 128
	// MaxFrames is the most frames a list holds.
	MaxFrames = MaxIndexPages * FramesPerIndex
)

// Frames allocates and frees physical pages. Allocate returns a page the
// caller owns; its contents are unspecified.
type Frames interface {
	Allocate() (uint64, bool)
	Free(physical uint64) bool
}

// View returns the kernel's view of one physical page.
type View func(physical uint64) ([]byte, bool)

// List is the frames of one object, in order.
type List struct {
	index [MaxIndexPages]uint64
	count uint64
}

// Reset empties the list without freeing anything; the caller has released
// its pages or never had any.
func (list *List) Reset() {
	for index := range list.index {
		list.index[index] = 0
	}
	list.count = 0
}

// Len is the number of frames in the list.
func (list *List) Len() uint64 { return list.count }

// Append adds frame at the end, taking a new index page when the last one is
// full.
func (list *List) Append(frames Frames, view View, frame uint64) bool {
	if frame == 0 || frame%PageSize != 0 || list.count == MaxFrames {
		return false
	}
	slot := list.count % FramesPerIndex
	root := list.count / FramesPerIndex
	if slot == 0 {
		page, ok := frames.Allocate()
		if !ok {
			return false
		}
		list.index[root] = page
	}
	data, ok := view(list.index[root])
	if !ok {
		if slot == 0 {
			frames.Free(list.index[root])
			list.index[root] = 0
		}
		return false
	}
	putUint64(data[slot*8:], frame)
	list.count++
	return true
}

// At returns frame number position.
func (list *List) At(view View, position uint64) (uint64, bool) {
	if position >= list.count {
		return 0, false
	}
	data, ok := view(list.index[position/FramesPerIndex])
	if !ok {
		return 0, false
	}
	return getUint64(data[position%FramesPerIndex*8:]), true
}

// Release frees every frame in the list and every index page, and empties it.
// It reports whether every page went back; a page the allocator refused is
// left and the rest are still freed.
func (list *List) Release(frames Frames, view View) bool {
	ok := true
	for root := uint64(0); root*FramesPerIndex < list.count; root++ {
		data, viewed := view(list.index[root])
		if !viewed {
			ok = false
		} else {
			last := list.count - root*FramesPerIndex
			if last > FramesPerIndex {
				last = FramesPerIndex
			}
			for slot := uint64(0); slot < last; slot++ {
				ok = frames.Free(getUint64(data[slot*8:])) && ok
			}
		}
		ok = frames.Free(list.index[root]) && ok
		list.index[root] = 0
	}
	list.count = 0
	return ok
}

// IndexPages is the number of index pages the list holds.
func (list *List) IndexPages() uint64 { return (list.count + FramesPerIndex - 1) / FramesPerIndex }

// IndexPagesFor is the number of index pages a list of count frames needs.
func IndexPagesFor(count uint64) uint64 { return (count + FramesPerIndex - 1) / FramesPerIndex }

func putUint64(data []byte, value uint64) {
	for index := 0; index < 8; index++ {
		data[index] = byte(value >> uint(index*8))
	}
}

func getUint64(data []byte) uint64 {
	value := uint64(0)
	for index := 7; index >= 0; index-- {
		value = value<<8 | uint64(data[index])
	}
	return value
}
