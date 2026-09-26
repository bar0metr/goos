package block

import "testing"

type memoryBackend struct {
	transfers int
	data     [1024]byte
	readOnly bool
	flushes  int
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
