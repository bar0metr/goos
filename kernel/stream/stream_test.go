package stream

import "testing"

func TestBoundedStreamLifecycle(t *testing.T) {
	stream := New()
	if status := stream.ReadStatus(); status != StatusWouldBlock {
		t.Fatalf("empty read readiness = %v", status)
	}
	if status := stream.WriteStatus(); status != StatusOK {
		t.Fatalf("empty write readiness = %v", status)
	}
	if _, status := stream.Read(make([]byte, 1)); status != StatusWouldBlock {
		t.Fatalf("empty read status = %v", status)
	}
	input := make([]byte, Capacity+1)
	for index := range input {
		input[index] = byte(index)
	}
	if size, status := stream.Write(input); status != StatusOK || size != Capacity {
		t.Fatalf("write = %d, %v", size, status)
	}
	if _, status := stream.Write(input[:1]); status != StatusWouldBlock {
		t.Fatalf("full write status = %v", status)
	}
	if status := stream.WriteStatus(); status != StatusWouldBlock {
		t.Fatalf("full write readiness = %v", status)
	}
	output := make([]byte, Capacity)
	if size, status := stream.Read(output); status != StatusOK || size != Capacity {
		t.Fatalf("read = %d, %v", size, status)
	}
	for index := range output {
		if output[index] != byte(index) {
			t.Fatalf("byte %d = %d", index, output[index])
		}
	}
	if !stream.CloseWriter() {
		t.Fatal("writer close failed")
	}
	if status := stream.ReadStatus(); status != StatusPeerClosed {
		t.Fatalf("closed writer readiness = %v", status)
	}
	if _, status := stream.Read(output[:1]); status != StatusPeerClosed {
		t.Fatalf("EOF status = %v", status)
	}
	if !stream.CloseReader() || !stream.Closed() {
		t.Fatal("stream did not close")
	}
}

func TestWriteStatusReportsClosedReader(t *testing.T) {
	stream := New()
	if !stream.CloseReader() || stream.WriteStatus() != StatusPeerClosed {
		t.Fatal("closed reader was not reported")
	}
}

func TestWriteStatusForUsesRequestedAtomicSize(t *testing.T) {
	value := New()
	payload := make([]byte, Capacity-7)
	if count, status := value.Write(payload); status != StatusOK || count != len(payload) {
		t.Fatalf("fill = %d status=%d", count, status)
	}
	if value.WriteStatusFor(7) != StatusOK || value.WriteStatusFor(8) != StatusWouldBlock {
		t.Fatal("requested write size was not respected")
	}
}

func TestRegistryEndpointIdentity(t *testing.T) {
	var registry Registry
	_, id, ok := registry.Create(7)
	reader, readerOK := ReaderObject(id)
	writer, writerOK := WriterObject(id)
	readerID, readerIsWriter, readerValid := DecodeEndpointObject(reader)
	writerID, writerIsWriter, writerValid := DecodeEndpointObject(writer)
	if !ok || !readerOK || !writerOK || !readerValid || !writerValid || readerID != id || writerID != id || readerIsWriter || !writerIsWriter {
		t.Fatal("invalid endpoint identity")
	}
}

func BenchmarkStreamRoundTrip(b *testing.B) {
	input := make([]byte, 256)
	output := make([]byte, 256)
	b.SetBytes(int64(len(input)))
	for index := 0; index < b.N; index++ {
		stream := New()
		if size, status := stream.Write(input); status != StatusOK || size != len(input) {
			b.Fatal("write failed")
		}
		if size, status := stream.Read(output); status != StatusOK || size != len(output) {
			b.Fatal("read failed")
		}
	}
}

func TestRegistryReportsTheChargedProcessOnRelease(t *testing.T) {
	var registry Registry
	endpoint, id, ok := registry.Create(9)
	if !ok {
		t.Fatal("stream was not created")
	}
	if _, released := registry.Release(id); released {
		t.Fatal("live stream was released")
	}
	endpoint.CloseReader()
	endpoint.CloseWriter()
	owner, released := registry.Release(id)
	if !released || owner != 9 {
		t.Fatalf("released = %v, charged to %d", released, owner)
	}
	// The slot is reusable and no longer names the old owner.
	if _, _, created := registry.Create(11); !created {
		t.Fatal("released slot was not reused")
	}
}
