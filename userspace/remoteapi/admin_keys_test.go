package remoteapi

import (
	"bytes"
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestKeyRequestBoundedFragmentsAndCorrelation(t *testing.T) {
	for _, size := range []int{1, 50, 240, 241, 4096} {
		argument := bytes.Repeat([]byte{'x'}, size)
		var buffer KeyRequestBuffer
		channel := ipc.NewChannel()
		for offset := 0; offset < len(argument); offset += MaxStatus {
			var out ipc.Message
			if !EncodeKeyRequestPart(AdminKeyAdd, argument, offset, 19, &out) || out.PayloadSize > ipc.MaxPayload {
				t.Fatal("fragment encode", size, offset)
			}
			if status := channel.Send(0, out, nil, nil, nil); status != ipc.StatusOK {
				t.Fatal("IPC fragment", status)
			}
			in, status := channel.Receive(1)
			if status != ipc.StatusOK {
				t.Fatal(status)
			}
			op, data, corr, done, ok := buffer.Accept(&in)
			final := offset+MaxStatus >= size
			if !ok || op != AdminKeyAdd || corr != 19 || done != final || !bytes.Equal(data, argument[:min(offset+MaxStatus, size)]) {
				t.Fatal("fragment decode", size, offset)
			}
			pending := 0
			if final {
				pending = 1
			}
			if channel.Pending(0) != pending {
				t.Fatal("partial fragment created pending RPC")
			}
		}
		var reply ipc.Message
		if !EncodeAdminReply(AdminKeyAdd, AdminOK, []byte("committed"), 19, &reply) || channel.Send(1, reply, nil, nil, nil) != ipc.StatusOK {
			t.Fatal("final reply")
		}
		buffer.Reset()
		if buffer.Pending() {
			t.Fatal("reset")
		}
	}
}
func TestKeyRequestRejectsMalformedReorderedAndOversized(t *testing.T) {
	argument := bytes.Repeat([]byte{'x'}, 500)
	var first, second ipc.Message
	EncodeKeyRequestPart(AdminKeyAdd, argument, 0, 7, &first)
	EncodeKeyRequestPart(AdminKeyAdd, argument, MaxStatus, 7, &second)
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.PayloadSize = 65535 }, func(m *ipc.Message) { m.Version-- }, func(m *ipc.Message) { m.HandleCount = 1; m.Handles[0] = 9 }, func(m *ipc.Message) { m.Payload[6] = 1 }, func(m *ipc.Message) { m.Correlation = 7 }, func(m *ipc.Message) { m.Payload[1] = 0 }, func(m *ipc.Message) { binary.LittleEndian.PutUint16(m.Payload[2:4], 4097) }, func(m *ipc.Message) { m.Payload[0] = byte(AdminStatus) }, func(m *ipc.Message) { m.Kind = ipc.KindRequest },
	} {
		var b KeyRequestBuffer
		m := first
		mutate(&m)
		if _, _, _, _, ok := b.Accept(&m); ok {
			t.Fatal("bad fragment accepted")
		}
	}
	for _, mutate := range []func(*ipc.Message){func(m *ipc.Message) { m.Payload[4] = 0 }, func(m *ipc.Message) { m.Payload[8]++ }, func(m *ipc.Message) { m.Payload[0] = byte(AdminKeyRemove) }, func(m *ipc.Message) { m.Payload[2]++ }} {
		var b KeyRequestBuffer
		b.Accept(&first)
		m := second
		mutate(&m)
		if _, _, _, _, ok := b.Accept(&m); ok || b.Pending() {
			t.Fatal("bad ordering/interleave accepted")
		}
	}
	var b KeyRequestBuffer
	if _, _, _, _, ok := b.Accept(&second); ok {
		t.Fatal("missing initial fragment")
	}
	b.Accept(&first)
	if _, _, _, _, ok := b.Accept(&first); ok {
		t.Fatal("duplicate fragment")
	}
	var out ipc.Message
	for _, size := range []int{0, 4097, 65536} {
		if EncodeKeyRequestPart(AdminKeyAdd, make([]byte, size), 0, 7, &out) {
			t.Fatal("oversized upload accepted", size)
		}
	}
}
func TestKeyControlAuthorityBoundAndEnvelope(t *testing.T) {
	for _, op := range []KeyControlOperation{KeyBegin, KeyCheck, KeyEnd} {
		token := uint64(9)
		if op == KeyBegin {
			token = 0
		}
		var message ipc.Message
		if !EncodeKeyControlRequest(op, token, 7, &message) {
			t.Fatal("request encode")
		}
		decoded, got, ok := DecodeKeyControlRequest(&message)
		if !ok || decoded != op || got != token {
			t.Fatal("request decode")
		}
		message.HandleCount = 1
		if _, _, ok := DecodeKeyControlRequest(&message); ok {
			t.Fatal("request attached authority")
		}
		reply := KeyControlReply{Operation: op, Result: ResultOK}
		files := handle.ID(0)
		if op == KeyBegin {
			reply.Token, reply.Revision, reply.Path = 7, 13, []byte("/home/user/.ssh/authorized_keys")
			files = 11
		}
		if !EncodeKeyControlReply(reply, files, 7, &message) {
			t.Fatal("reply encode")
		}
		decodedReply, ok := DecodeKeyControlReply(&message, op, 7)
		if !ok || decodedReply.Token != reply.Token || !bytes.Equal(decodedReply.Path, reply.Path) {
			t.Fatal("reply decode")
		}
		if _, ok := DecodeKeyControlReply(&message, op, 8); ok {
			t.Fatal("stale reply")
		}
		message.HandleCount = 2
		if _, ok := DecodeKeyControlReply(&message, op, 7); ok {
			t.Fatal("extra authority")
		}
	}
	var reply ipc.Message
	if EncodeKeyControlReply(KeyControlReply{Operation: KeyCheck, Result: ResultOK}, 11, 7, &reply) {
		t.Fatal("check broadened authority")
	}
	if !EncodeAdminRequest(AdminKeys, 7, &reply) {
		t.Fatal("list codec")
	}
	if EncodeAdminRequest(AdminKeyAdd, 7, &reply) {
		t.Fatal("key add without bounded argument")
	}
}
func TestFullKeyListReplyExceedsQueueWithoutLosingCorrelation(t *testing.T) {
	channel := ipc.NewChannel()
	var request ipc.Message
	EncodeAdminRequest(AdminKeys, 31, &request)
	channel.Send(0, request, nil, nil, nil)
	channel.Receive(1)
	// A worst-case list is 64 full fingerprints, separators, 64-byte comments.
	text := bytes.Repeat([]byte{'x'}, 64*(50+1+64+1))
	parts := (len(text) + MaxStatus - 1) / MaxStatus
	if parts > MaxStatusParts || parts <= ipc.QueueCapacity {
		t.Fatal("list fragmentation budget", parts)
	}
	var got []byte
	for part := 0; part < parts; part++ {
		n := min(MaxStatus, len(text))
		var reply ipc.Message
		EncodeAdminPart(AdminKeys, AdminOK, text[:n], part+1 < parts, 31, &reply)
		if channel.Send(1, reply, nil, nil, nil) != ipc.StatusOK {
			t.Fatal("reply")
		}
		in, status := channel.Receive(0)
		if status != ipc.StatusOK {
			t.Fatal(status)
		}
		_, chunk, _, ok := DecodeAdminPart(&in, AdminKeys, 31)
		if !ok {
			t.Fatal("part")
		}
		got = append(got, chunk...)
		text = text[n:]
	}
	if len(got) != 64*(50+1+64+1) || channel.Pending(0) != 0 {
		t.Fatal("list or final correlation lost")
	}
}
