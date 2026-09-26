package block

import "testing"

type memoryBackend struct {
	transfers int
	data      [1024]byte
	readOnly  bool
	flushes   int
	// lastOffset is the device offset of the last transfer.
	lastOffset uint64
}

func (backend *memoryBackend) Capacity() uint64  { return uint64(len(backend.data)) }
func (backend *memoryBackend) BlockSize() uint32 { return 512 }
func (backend *memoryBackend) ReadAt(offset uint64, target []byte) Status {
	copy(target, backend.data[offset:offset+uint64(len(target))])
	return StatusOK
}
func (backend *memoryBackend) WriteAt(offset uint64, data []byte) Status {
	if backend.readOnly {
		return StatusReadOnly
	}
	copy(backend.data[offset:offset+uint64(len(data))], data)
	return StatusOK
}
func (backend *memoryBackend) Flush() Status { backend.flushes++; return StatusOK }
func (backend *memoryBackend) Transfer(request Request) Status {
	backend.transfers++
	backend.lastOffset = request.Offset
	return StatusOK
}

func TestServiceReadWriteFlush(t *testing.T) {
	backend := &memoryBackend{}
	service, ok := NewService(backend)
	if !ok {
		t.Fatal("service construction failed")
	}
	info := service.Dispatch(Request{Operation: OperationInfo})
	if info.Status != StatusOK || info.Capacity != 1024 || info.BlockSize != 512 {
		t.Fatalf("info = %#v", info)
	}
	write := service.Dispatch(Request{Operation: OperationWrite, Offset: 510, Length: 5, Data: []byte("block")})
	read := service.Dispatch(Request{Operation: OperationRead, Offset: 510, Length: 5})
	flush := service.Dispatch(Request{Operation: OperationFlush})
	if write.Status != StatusOK || read.Status != StatusOK || string(read.Data) != "block" || flush.Status != StatusOK || backend.flushes != 1 {
		t.Fatalf("write=%#v read=%#v flush=%#v", write, read, flush)
	}
}

func TestServiceBoundsAndReadOnly(t *testing.T) {
	backend := &memoryBackend{readOnly: true}
	service, _ := NewService(backend)
	if reply := service.Dispatch(Request{Operation: OperationRead, Offset: 1000, Length: 25}); reply.Status != StatusOutOfRange {
		t.Fatalf("out-of-range read = %#v", reply)
	}
	if reply := service.Dispatch(Request{Operation: OperationWrite, Length: 1, Data: []byte{1}}); reply.Status != StatusReadOnly {
		t.Fatalf("read-only write = %#v", reply)
	}
}

func TestSliceTranslatesAndBounds(t *testing.T) {
	backend := &memoryBackend{}
	service, _ := NewService(backend)
	slice := Slice{Start: 512, Length: 256, Writable: true}
	if info := service.DispatchIn(Request{Operation: OperationInfo}, slice); info.Capacity != 256 || info.BlockSize != 512 {
		t.Fatalf("info = %#v", info)
	}
	if reply := service.DispatchIn(Request{Operation: OperationWrite, Offset: 250, Length: 6, Data: []byte("sliced")}, slice); reply.Status != StatusOK {
		t.Fatal(reply.Status)
	}
	if string(backend.data[762:768]) != "sliced" {
		t.Fatal("the write did not land at the slice's offset")
	}
	if reply := service.DispatchIn(Request{Operation: OperationRead, Offset: 250, Length: 6}, slice); string(reply.Data) != "sliced" {
		t.Fatalf("read = %#v", reply)
	}
	// Nothing outside the slice is reachable, including by overflow.
	for _, request := range []Request{
		{Operation: OperationRead, Offset: 251, Length: 6},
		{Operation: OperationRead, Offset: 256, Length: 1},
		{Operation: OperationRead, Offset: ^uint64(0), Length: 2},
		{Operation: OperationWrite, Offset: 255, Length: 2, Data: []byte("xx")},
		{Operation: OperationReadBulk, Offset: 0, Length: 257},
		{Operation: OperationWriteBulk, Offset: ^uint64(0) - 1, Length: 4},
		{Operation: OperationRead, Offset: 0, Length: 0},
	} {
		if reply := service.DispatchIn(request, slice); reply.Status != StatusOutOfRange {
			t.Errorf("%+v: %v", request, reply.Status)
		}
	}
	if backend.transfers != 0 {
		t.Fatal("an out-of-range bulk request reached the backend")
	}
	// A bulk request inside the slice reaches the backend at the device
	// offset.
	if reply := service.DispatchIn(Request{Operation: OperationReadBulk, Offset: 16, Length: 16}, slice); reply.Status != StatusOK || backend.transfers != 1 || backend.lastOffset != 528 {
		t.Fatalf("bulk: %v, %d transfers at %d", reply.Status, backend.transfers, backend.lastOffset)
	}
}

func TestReadOnlySliceRefusesWrites(t *testing.T) {
	backend := &memoryBackend{}
	service, _ := NewService(backend)
	slice := Slice{Start: 0, Length: 512}
	if reply := service.DispatchIn(Request{Operation: OperationWrite, Offset: 0, Length: 1, Data: []byte{1}}, slice); reply.Status != StatusReadOnly {
		t.Fatalf("write: %v", reply.Status)
	}
	if reply := service.DispatchIn(Request{Operation: OperationWriteBulk, Offset: 0, Length: 16}, slice); reply.Status != StatusReadOnly || backend.transfers != 0 {
		t.Fatalf("bulk write: %v", reply.Status)
	}
	if reply := service.DispatchIn(Request{Operation: OperationRead, Offset: 0, Length: 16}, slice); reply.Status != StatusOK {
		t.Fatalf("read: %v", reply.Status)
	}
	if backend.data[0] != 0 {
		t.Fatal("a read-only slice was written")
	}
}

func TestSliceOutsideTheDeviceServesNothing(t *testing.T) {
	service, _ := NewService(&memoryBackend{})
	for _, slice := range []Slice{{Start: 1000, Length: 100}, {Start: 0, Length: 0}, {Start: ^uint64(0), Length: 2}} {
		if reply := service.DispatchIn(Request{Operation: OperationInfo}, slice); reply.Status != StatusIOError {
			t.Errorf("%+v: %v", slice, reply.Status)
		}
	}
	if whole := service.Whole(); whole.Length != 1024 || !whole.Writable {
		t.Fatalf("whole = %+v", whole)
	}
}
