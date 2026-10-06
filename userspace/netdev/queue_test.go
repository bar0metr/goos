package netdev

import (
	"encoding/binary"
	"testing"
)

func TestQueueLayout(t *testing.T) {
	tests := []struct {
		size                   uint16
		available, used, total uint64
		ok                     bool
	}{
		{0, 0, 0, 0, false},
		{1, 0, 0, 0, false},
		{3, 0, 0, 0, false},
		{255, 0, 0, 0, false},
		{512, 0, 0, 0, false},
		{0xffff, 0, 0, 0, false},
		{2, 32, 4096, 8192, true},
		{8, 128, 4096, 8192, true},
		{128, 2048, 4096, 8192, true},
		{256, 4096, 8192, 12288, true},
	}
	for _, test := range tests {
		available, used, total, ok := QueueLayout(test.size)
		if ok != test.ok || available != test.available || used != test.used || total != test.total {
			t.Fatalf("size %d: %d %d %d %v", test.size, available, used, total, ok)
		}
	}
	// The virtio-blk driver's formula for the same layout.
	for size := uint16(2); size <= MaxQueueSize; size *= 2 {
		_, used, _, _ := QueueLayout(size)
		if want := align(uint64(16)*uint64(size)+4+uint64(size)*2+2, 4096); used != want {
			t.Fatalf("size %d: used %d, want %d", size, used, want)
		}
	}
}

func TestQueueInit(t *testing.T) {
	var q Queue
	memory := make([]byte, 8192)
	for index := range memory {
		memory[index] = 0xaa
	}
	if q.Init(memory[:8191], 0x10000, 8) {
		t.Fatal("short memory")
	}
	if q.Init(memory, 0x10001, 8) {
		t.Fatal("unaligned physical")
	}
	if q.Init(memory, ^uint64(0)&^(QueueAlign-1), 8) {
		t.Fatal("physical wraps")
	}
	if q.Init(memory, (maxQueuePFN+1)*QueueAlign, 8) {
		t.Fatal("physical beyond the PFN register")
	}
	if q.Init(memory, 0x10000, 6) {
		t.Fatal("size not a power of two")
	}
	if !q.Init(memory, 0x10000, 8) || q.Size() != 8 || q.InFlight() != 0 {
		t.Fatal("init")
	}
	for index, value := range memory {
		if value != 0 {
			t.Fatalf("byte %d not cleared", index)
		}
	}
}

type device struct {
	memory []byte
	size   uint16
	used   uint16
	seen   uint16
}

func newQueue(t *testing.T, size uint16) (*Queue, *device) {
	t.Helper()
	_, _, total, _ := QueueLayout(size)
	q := new(Queue)
	memory := make([]byte, total)
	if !q.Init(memory, 0x200000, size) {
		t.Fatal("init")
	}
	return q, &device{memory: memory, size: size}
}

// take returns the next published available entry, as the device sees it.
func (d *device) take() (id uint16, ok bool) {
	available, _, _, _ := QueueLayout(d.size)
	published := binary.LittleEndian.Uint16(d.memory[available+2:])
	if published == d.seen {
		return 0, false
	}
	id = binary.LittleEndian.Uint16(d.memory[available+4+2*uint64(d.seen%d.size):])
	d.seen++
	return id, true
}

func (d *device) descriptor(id uint16) (physical uint64, length uint32, flags uint16) {
	entry := d.memory[16*uint64(id):]
	return binary.LittleEndian.Uint64(entry), binary.LittleEndian.Uint32(entry[8:]), binary.LittleEndian.Uint16(entry[12:])
}

// complete writes a used entry and advances the used index by advance.
func (d *device) complete(id uint32, written uint32, advance uint16) {
	_, used, _, _ := QueueLayout(d.size)
	entry := used + 4 + 8*uint64(d.used%d.size)
	binary.LittleEndian.PutUint32(d.memory[entry:], id)
	binary.LittleEndian.PutUint32(d.memory[entry+4:], written)
	d.used += advance
	binary.LittleEndian.PutUint16(d.memory[used+2:], d.used)
}

func TestQueueAddPublishComplete(t *testing.T) {
	q, d := newQueue(t, 4)
	id, ok := q.Add(0x300000, 2048, true)
	if !ok || q.InFlight() != 1 {
		t.Fatal("add")
	}
	if _, seen := d.take(); seen {
		t.Fatal("visible before publish")
	}
	q.Publish()
	got, seen := d.take()
	physical, length, flags := d.descriptor(got)
	if !seen || got != id || physical != 0x300000 || length != 2048 || flags != descriptorWrite {
		t.Fatalf("device saw %d %x %d %d", got, physical, length, flags)
	}
	if _, _, status := q.Used(); status != UsedNone {
		t.Fatal("completion before the device wrote one")
	}
	d.complete(uint32(id), 60, 1)
	done, written, status := q.Used()
	if status != UsedOK || done != id || written != 60 || q.InFlight() != 0 {
		t.Fatalf("used %d %d %d", done, written, status)
	}
	if _, _, status := q.Used(); status != UsedNone {
		t.Fatal("second completion")
	}
	readOnly, _ := q.Add(0x400000, 64, false)
	q.Publish()
	d.take()
	if _, _, flags := d.descriptor(readOnly); flags != 0 {
		t.Fatal("read-only buffer marked writable")
	}
}

func TestQueueFull(t *testing.T) {
	q, _ := newQueue(t, 4)
	for count := 0; count < 4; count++ {
		if _, ok := q.Add(0x1000*uint64(count+1), 100, true); !ok {
			t.Fatal("add")
		}
	}
	if _, ok := q.Add(0x9000, 100, true); ok || q.InFlight() != 4 {
		t.Fatal("added past the queue size")
	}
	q2, _ := newQueue(t, 4)
	if _, ok := q2.Add(0x1000, 0, true); ok {
		t.Fatal("empty buffer")
	}
	if _, ok := q2.Add(^uint64(0)-10, 100, true); ok {
		t.Fatal("buffer wraps")
	}
}

func TestQueueMalformed(t *testing.T) {
	tests := []struct {
		name   string
		damage func(q *Queue, d *device, ids []uint16)
	}{
		{"index jumps past published", func(q *Queue, d *device, ids []uint16) { d.complete(uint32(ids[0]), 1, 3) }},
		{"index runs backward", func(q *Queue, d *device, ids []uint16) { d.complete(uint32(ids[0]), 1, 0xffff) }},
		{"id out of range", func(q *Queue, d *device, ids []uint16) { d.complete(4, 1, 1) }},
		{"id huge", func(q *Queue, d *device, ids []uint16) { d.complete(0x10000|uint32(ids[0]), 1, 1) }},
		{"id not in flight", func(q *Queue, d *device, ids []uint16) { d.complete(3, 1, 1) }},
		{"id added but unpublished", func(q *Queue, d *device, ids []uint16) {
			late, _ := q.Add(0x7000, 100, true)
			d.complete(uint32(late), 1, 1)
		}},
		{"duplicate completion", func(q *Queue, d *device, ids []uint16) {
			d.complete(uint32(ids[0]), 1, 1)
			if _, _, status := q.Used(); status != UsedOK {
				panic("first completion")
			}
			d.complete(uint32(ids[0]), 1, 1)
		}},
		{"overlong write", func(q *Queue, d *device, ids []uint16) { d.complete(uint32(ids[0]), 2049, 1) }},
		{"overlong read-only", func(q *Queue, d *device, ids []uint16) { d.complete(uint32(ids[1]), 65, 1) }},
	}
	for _, test := range tests {
		q, d := newQueue(t, 4)
		first, _ := q.Add(0x1000, 2048, true)
		second, _ := q.Add(0x2000, 64, false)
		q.Publish()
		test.damage(q, d, []uint16{first, second})
		if _, _, status := q.Used(); status != UsedMalformed {
			t.Fatalf("%s: status %d", test.name, status)
		}
		if _, _, status := q.Used(); status != UsedMalformed {
			t.Fatalf("%s: queue recovered", test.name)
		}
		if _, ok := q.Add(0x3000, 10, true); ok {
			t.Fatalf("%s: broken queue accepted a buffer", test.name)
		}
	}
}

// The 16-bit ring indices wrap; the queue must keep working across many
// wraps with buffers completed out of order.
func TestQueueWrap(t *testing.T) {
	q, d := newQueue(t, 4)
	const rounds = 70000
	var held [2]uint16
	for round := 0; round < rounds; round++ {
		for index := range held[:] {
			id, ok := q.Add(0x1000*uint64(index+1), 1514, true)
			if !ok {
				t.Fatalf("round %d: add", round)
			}
			held[index] = id
		}
		q.Publish()
		for count := 0; count < 2; count++ {
			if _, ok := d.take(); !ok {
				t.Fatalf("round %d: device saw nothing", round)
			}
		}
		d.complete(uint32(held[1]), 100, 1)
		d.complete(uint32(held[0]), 1514, 1)
		for _, want := range []uint16{held[1], held[0]} {
			id, _, status := q.Used()
			if status != UsedOK || id != want {
				t.Fatalf("round %d: used %d status %d", round, id, status)
			}
		}
		if q.InFlight() != 0 {
			t.Fatalf("round %d: in flight %d", round, q.InFlight())
		}
	}
	if q.usedIndex != uint16((2*rounds)%65536) {
		t.Fatal("used index did not wrap with the device")
	}
}

func TestValidReceive(t *testing.T) {
	tests := []struct {
		written uint32
		frame   uint16
		ok      bool
	}{
		{0, 0, false},
		{NetHeaderSize, 0, false},
		{NetHeaderSize + MinFrame - 1, 0, false},
		{NetHeaderSize + MinFrame, MinFrame, true},
		{NetHeaderSize + MaxFrame, MaxFrame, true},
		{NetHeaderSize + MaxFrame + 1, 0, false},
		{^uint32(0), 0, false},
	}
	for _, test := range tests {
		frame, ok := ValidReceive(test.written)
		if frame != test.frame || ok != test.ok {
			t.Fatalf("written %d: %d %v", test.written, frame, ok)
		}
	}
}

func TestValidHeader(t *testing.T) {
	header := make([]byte, NetHeaderSize)
	if !ValidHeader(header) {
		t.Fatal("zero header")
	}
	if ValidHeader(header[:NetHeaderSize-1]) {
		t.Fatal("short header")
	}
	for index := 2; index < NetHeaderSize; index++ {
		header[index] = 0x5a
	}
	if !ValidHeader(header) {
		t.Fatal("ignored fields rejected")
	}
	for _, index := range []int{headerFlags, headerSegmentType} {
		bad := make([]byte, NetHeaderSize)
		bad[index] = 2
		if ValidHeader(bad) {
			t.Fatalf("nonzero byte %d accepted", index)
		}
	}
}

// FuzzQueueUsed lets the device write anything into the used ring while the
// driver keeps buffers in flight. The queue must never report a completion
// it did not hand out, and must never panic.
func FuzzQueueUsed(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 10, 0, 0, 0}, uint16(1))
	f.Add([]byte{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, uint16(2))
	f.Add([]byte{9, 0, 0, 0, 0, 0, 0, 0}, uint16(0xffff))
	f.Fuzz(func(t *testing.T, entries []byte, index uint16) {
		const size = 8
		q, d := newQueue(t, size)
		var lengths [size]uint32
		for count := 0; count < 5; count++ {
			length := uint32(100 * (count + 1))
			id, ok := q.Add(0x1000*uint64(count+1), length, count%2 == 0)
			if !ok {
				t.Fatal("add")
			}
			lengths[id] = length
		}
		q.Publish()
		_, used, _, _ := QueueLayout(size)
		copy(d.memory[used+4:used+4+8*size], entries)
		binary.LittleEndian.PutUint16(d.memory[used+2:], index)
		seen := [size]bool{}
		for step := 0; step < 2*size; step++ {
			id, written, status := q.Used()
			if status == UsedNone {
				return
			}
			if status == UsedMalformed {
				if _, _, again := q.Used(); again != UsedMalformed {
					t.Fatal("recovered")
				}
				return
			}
			if id >= size || seen[id] || lengths[id] == 0 || written > lengths[id] {
				t.Fatalf("accepted id %d written %d", id, written)
			}
			seen[id] = true
		}
		t.Fatal("more completions than buffers")
	})
}
