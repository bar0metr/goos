package netdev

import "encoding/binary"

const (
	MaxQueueSize = 1024
	// QueueAlign is the legacy interface's ring alignment; the queue address
	// register takes a page frame number of this size.
	QueueAlign = 4096
	// NetHeaderSize is the legacy virtio-net header with no features
	// negotiated: flags, gso_type, hdr_len, gso_size, csum_start, csum_offset.
	NetHeaderSize = 10

	descriptorSize    = 16
	descriptorWrite   = 2
	ringHeader        = 4
	availableEntry    = 2
	usedEntry         = 8
	eventSize         = 2
	ringIndexOffset   = 2
	maxQueuePFN       = 0xffffffff
	headerFlags       = 0
	headerSegmentType = 1
	headerDataValid   = 2
)

// QueueLayout returns the byte offsets of the available and used rings and
// the total size of a legacy split virtqueue of size entries. Descriptors
// start at offset 0. size must be a power of two from 2 to MaxQueueSize.
func QueueLayout(size uint16) (available, used, total uint64, ok bool) {
	if size < 2 || size > MaxQueueSize || size&(size-1) != 0 {
		return 0, 0, 0, false
	}
	entries := uint64(size)
	available = descriptorSize * entries
	used = align(available+ringHeader+availableEntry*entries+eventSize, QueueAlign)
	total = used + align(ringHeader+usedEntry*entries+eventSize, QueueAlign)
	return available, used, total, true
}

func align(value, to uint64) uint64 {
	return (value + to - 1) &^ (to - 1)
}

type UsedStatus uint8

const (
	UsedNone UsedStatus = iota
	UsedOK
	UsedMalformed
)

type descriptorState uint8

const (
	descriptorFree descriptorState = iota
	descriptorAdded
	descriptorPublished
)

// Queue keeps the driver's side of one legacy split virtqueue whose rings
// live in memory. The queue decides from private shadow state and reads the
// device-writable used ring only to check it against that state. After one
// malformed completion the queue stays broken and the driver must fail.
//
// Single-descriptor buffers only: the driver never chains, so a descriptor
// id names one buffer.
type Queue struct {
	memory    []byte
	size      uint16
	available uint64
	used      uint64

	state    [MaxQueueSize]descriptorState
	length   [MaxQueueSize]uint32
	next     [MaxQueueSize]uint16
	order    [MaxQueueSize]uint16
	freeHead uint16
	free     uint16

	availableIndex uint16
	publishedIndex uint16
	usedIndex      uint16
	outstanding    uint16
	broken         bool
}

// Init binds q to memory, the CPU mapping of the ring area that starts at
// physical. It zeroes the rings and frees every descriptor.
func (q *Queue) Init(memory []byte, physical uint64, size uint16) bool {
	available, used, total, ok := QueueLayout(size)
	if !ok || uint64(len(memory)) < total || physical%QueueAlign != 0 ||
		physical+total < physical || physical/QueueAlign > maxQueuePFN {
		return false
	}
	// Reset fields in place: TinyGo puts a composite literal of this size on
	// the heap, and this heap never frees.
	q.memory, q.size, q.available, q.used = memory[:total], size, available, used
	for index := range q.memory {
		q.memory[index] = 0
	}
	for id := range q.state[:] {
		q.state[id], q.length[id], q.order[id] = descriptorFree, 0, 0
		q.next[id] = uint16(id + 1)
	}
	q.freeHead, q.free = 0, size
	q.availableIndex, q.publishedIndex, q.usedIndex, q.outstanding, q.broken = 0, 0, 0, 0, false
	return true
}

func (q *Queue) Size() uint16 { return q.size }

// Add writes one buffer into a free descriptor and appends it to the
// available ring. The device cannot see it until Publish.
func (q *Queue) Add(physical uint64, length uint32, deviceWrites bool) (id uint16, ok bool) {
	if q.broken || q.free == 0 || length == 0 || physical+uint64(length) < physical {
		return 0, false
	}
	id = q.freeHead
	q.freeHead = q.next[id]
	q.free--
	var flags uint16
	if deviceWrites {
		flags = descriptorWrite
	}
	descriptor := q.memory[uint64(id)*descriptorSize : uint64(id+1)*descriptorSize]
	binary.LittleEndian.PutUint64(descriptor[0:], physical)
	binary.LittleEndian.PutUint32(descriptor[8:], length)
	binary.LittleEndian.PutUint16(descriptor[12:], flags)
	binary.LittleEndian.PutUint16(descriptor[14:], 0)
	ring := q.available + ringHeader + availableEntry*uint64(q.availableIndex%q.size)
	binary.LittleEndian.PutUint16(q.memory[ring:], id)
	q.order[q.availableIndex%q.size] = id
	q.availableIndex++
	q.state[id] = descriptorAdded
	q.length[id] = length
	return id, true
}

// Publish makes every added buffer visible by storing the available index.
// The device may read the descriptors and ring entries as soon as it sees
// the index, so those stores must reach memory first. On amd64 the CPU does
// not reorder stores with other stores, so no fence is needed; the caller
// must still call Publish after Add returns and before notifying the device.
func (q *Queue) Publish() {
	pending := q.availableIndex - q.publishedIndex
	for step := uint16(0); step < pending && step < q.size; step++ {
		q.state[q.order[(q.publishedIndex+step)%q.size]] = descriptorPublished
	}
	q.outstanding += pending
	q.publishedIndex = q.availableIndex
	binary.LittleEndian.PutUint16(q.memory[q.available+ringIndexOffset:], q.availableIndex)
}

// Used consumes the next completion, if any. It reads the used index before
// the entry it covers; amd64 does not reorder loads with other loads. The
// index may not advance past the published buffers, and the entry must
// name a published descriptor with a written length within its buffer.
func (q *Queue) Used() (id uint16, length uint32, status UsedStatus) {
	if q.broken {
		return 0, 0, UsedMalformed
	}
	advanced := binary.LittleEndian.Uint16(q.memory[q.used+ringIndexOffset:]) - q.usedIndex
	if advanced == 0 {
		return 0, 0, UsedNone
	}
	entry := q.used + ringHeader + usedEntry*uint64(q.usedIndex%q.size)
	device := binary.LittleEndian.Uint32(q.memory[entry:])
	written := binary.LittleEndian.Uint32(q.memory[entry+4:])
	if advanced > q.outstanding || device >= uint32(q.size) ||
		q.state[device] != descriptorPublished || written > q.length[device] {
		q.broken = true
		return 0, 0, UsedMalformed
	}
	id = uint16(device)
	q.state[id] = descriptorFree
	q.next[id] = q.freeHead
	q.freeHead = id
	q.free++
	q.outstanding--
	q.usedIndex++
	return id, written, UsedOK
}

// InFlight counts buffers added and not yet completed, published or not.
func (q *Queue) InFlight() int {
	return int(q.size - q.free)
}

// ValidReceive checks a receive completion's written length, header
// included, and returns the frame length after the header.
func ValidReceive(written uint32) (frameLength uint16, ok bool) {
	if written < NetHeaderSize+MinFrame || written > NetHeaderSize+MaxFrame {
		return 0, false
	}
	return uint16(written - NetHeaderSize), true
}

// ValidHeader checks the first NetHeaderSize bytes of a received buffer.
// The driver negotiates no receive offloads. It tolerates DATA_VALID from
// hosts that set the advisory bit without GUEST_CSUM, but does not trust it:
// the network stack still verifies every supported checksum. NEEDS_CSUM,
// other flags and every segmented packet remain unsupported. ValidHeader
// ignores hdr_len, gso_size and the checksum fields because the driver never
// uses them.
func ValidHeader(header []byte) bool {
	return len(header) >= NetHeaderSize && header[headerFlags]&^headerDataValid == 0 && header[headerSegmentType] == 0
}
