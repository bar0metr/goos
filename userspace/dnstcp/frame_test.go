package dnstcp

import (
	"bytes"
	"encoding/binary"
	"testing"

	"goos/userspace/dnswire"
)

const slot = 4096

func framed(message []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...)
}

func body(size int) []byte {
	out := make([]byte, size)
	for index := range out {
		out[index] = byte(index*7 + 3)
	}
	return out
}

// feed hands stream to a fresh Receiver in the given piece sizes, cycling
// through them, asking only for what Want allows.
func feed(t *testing.T, stream []byte, sizes ...int) (*Receiver, *Buffer, bool) {
	t.Helper()
	var receiver Receiver
	buffer := new(Buffer)
	for index := 0; len(stream) > 0; index++ {
		size := min(sizes[index%len(sizes)], receiver.Want(slot), len(stream))
		complete, valid := receiver.Take(buffer, stream[:size])
		if !valid {
			return &receiver, buffer, false
		}
		stream = stream[size:]
		if complete {
			break
		}
	}
	return &receiver, buffer, true
}

func TestReceiverSplits(t *testing.T) {
	message := body(5000)
	for _, sizes := range [][]int{
		{slot},       // prefix and body together, body over one slot
		{1},          // byte by byte: prefix split 1+1
		{1, 1, 7},    // prefix split, then small body pieces
		{3, 4096},    // prefix with one body byte
		{2, 100, 13}, // whole prefix, many chunks
	} {
		receiver, buffer, valid := feed(t, framed(message), sizes...)
		if !valid || !receiver.Complete() || !bytes.Equal(receiver.Message(buffer), message) {
			t.Fatalf("sizes %v: valid %v complete %v", sizes, valid, receiver.Complete())
		}
	}
}

func TestReceiverLargest(t *testing.T) {
	message := body(MaxMessage)
	receiver, buffer, valid := feed(t, framed(message), slot)
	if !valid || !receiver.Complete() || !bytes.Equal(receiver.Message(buffer), message) {
		t.Fatal("65535-byte message")
	}
}

// Want never asks past the message, so bytes after it stay in the stream;
// a piece that holds more is cut at the message's end.
func TestReceiverWantAndTrailing(t *testing.T) {
	var receiver Receiver
	buffer := new(Buffer)
	if receiver.Want(slot) != slot || receiver.Started() {
		t.Fatal("fresh receiver")
	}
	receiver.Take(buffer, []byte{0, 20, 1, 2, 3})
	if receiver.Want(slot) != 17 || !receiver.Started() {
		t.Fatalf("want %d", receiver.Want(slot))
	}
	complete, valid := receiver.Take(buffer, bytes.Repeat([]byte{9}, 40))
	if !complete || !valid || len(receiver.Message(buffer)) != 20 {
		t.Fatal("trailing bytes")
	}
	if receiver.Want(slot) != 0 {
		t.Fatal("asks past the message")
	}
}

func TestReceiverShortPrefix(t *testing.T) {
	for _, length := range []uint16{0, 1, dnswire.HeaderSize - 1} {
		_, _, valid := feed(t, framed(body(int(length))), 1)
		if valid {
			t.Fatalf("length %d accepted", length)
		}
	}
	if _, _, valid := feed(t, framed(body(dnswire.HeaderSize)), 1); !valid {
		t.Fatal("header-sized message refused")
	}
}

// A stream that ends early leaves the receiver incomplete, before or after
// the prefix.
func TestReceiverEarlyEnd(t *testing.T) {
	stream := framed(body(300))
	for _, cut := range []int{0, 1, 2, 150} {
		receiver, _, valid := feed(t, stream[:cut], 64)
		if !valid || receiver.Complete() {
			t.Fatalf("cut %d: valid %v complete %v", cut, valid, receiver.Complete())
		}
		if receiver.Started() != (cut != 0) {
			t.Fatalf("cut %d: started %v", cut, receiver.Started())
		}
	}
}

func TestSenderPartialWrites(t *testing.T) {
	query := body(dnswire.MaxQuery)
	var sender Sender
	sender.Set(query)
	var got []byte
	for _, accept := range []int{0, 1, 5, 1000} {
		piece := sender.Next(7)
		take := min(accept, len(piece))
		got = append(got, piece[:take]...)
		if !sender.Accepted(take) {
			t.Fatalf("accept %d of %d", take, len(piece))
		}
	}
	for !sender.Done() {
		piece := sender.Next(slot)
		got = append(got, piece...)
		sender.Accepted(len(piece))
	}
	if !bytes.Equal(got, framed(query)) {
		t.Fatal("framed bytes differ")
	}
	if len(sender.Next(slot)) != 0 {
		t.Fatal("piece after done")
	}
}

func TestSenderOverAccept(t *testing.T) {
	var sender Sender
	sender.Set(body(30))
	piece := sender.Next(10)
	if sender.Accepted(len(piece)+1) || sender.Accepted(-1) {
		t.Fatal("accepted more than offered")
	}
	if !sender.Accepted(4) || !sender.Accepted(6) || sender.Accepted(1) {
		t.Fatal("piece accounting")
	}
	// Set starts over.
	sender.Set(body(12))
	if sender.Done() || len(sender.Next(slot)) != 14 {
		t.Fatal("reset")
	}
}
