package stream

import "testing"

func TestBoundedStreamLifecycle(t *testing.T) {
	stream := New()
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
	if _, status := stream.Read(output[:1]); status != StatusPeerClosed {
		t.Fatalf("EOF status = %v", status)
	}
	if !stream.CloseReader() || !stream.Closed() {
		t.Fatal("stream did not close")
	}
}

func TestRegistryEndpointIdentity(t *testing.T) {
	var registry Registry
	_, id, ok := registry.Create()
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
