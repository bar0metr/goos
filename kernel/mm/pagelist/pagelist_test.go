package pagelist

import "testing"

// memory is a fake allocator whose pages are Go slices.
type memory struct {
	pages     map[uint64][]byte
	next      uint64
	limit     int
	allocated int
}

func newMemory(limit int) *memory {
	return &memory{pages: map[uint64][]byte{}, next: PageSize, limit: limit}
}

func (m *memory) Allocate() (uint64, bool) {
	if m.allocated == m.limit {
		return 0, false
	}
	m.allocated++
	address := m.next
	m.next += PageSize
	m.pages[address] = make([]byte, PageSize)
	return address, true
}

func (m *memory) Free(physical uint64) bool {
	if _, ok := m.pages[physical]; !ok {
		return false
	}
	delete(m.pages, physical)
	m.allocated--
	return true
}

func (m *memory) view(physical uint64) ([]byte, bool) {
	page, ok := m.pages[physical]
	return page, ok
}

func TestAppendAtAcrossIndexPages(t *testing.T) {
	m := newMemory(1 << 20)
	var list List
	const count = 3*FramesPerIndex + 7
	frames := make([]uint64, count)
	for index := range frames {
		frame, _ := m.Allocate()
		frames[index] = frame
		if !list.Append(m, m.view, frame) {
			t.Fatalf("append %d", index)
		}
	}
	if list.Len() != count || list.IndexPages() != 4 {
		t.Fatalf("len %d index pages %d", list.Len(), list.IndexPages())
	}
	for index, want := range frames {
		if got, ok := list.At(m.view, uint64(index)); !ok || got != want {
			t.Fatalf("frame %d = %#x, want %#x", index, got, want)
		}
	}
	if _, ok := list.At(m.view, count); ok {
		t.Fatal("read past the end")
	}
	if !list.Release(m, m.view) || m.allocated != 0 || list.Len() != 0 {
		t.Fatalf("release left %d pages", m.allocated)
	}
}

func TestAppendRefusesBadFramesAndFullLists(t *testing.T) {
	m := newMemory(2)
	var list List
	if list.Append(m, m.view, 0) || list.Append(m, m.view, 100) {
		t.Fatal("a bad frame was accepted")
	}
	frame, _ := m.Allocate()
	if !list.Append(m, m.view, frame) {
		t.Fatal("append")
	}
	// The allocator is spent, so the next index page cannot be had.
	list.count = FramesPerIndex
	if list.Append(m, m.view, frame) {
		t.Fatal("append without an index page")
	}
	list.count = MaxFrames
	if list.Append(m, m.view, frame) {
		t.Fatal("append past MaxFrames")
	}
}
