package exec

import (
	"bytes"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/userspace/startup"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	var wire [startup.VectorSize]byte
	arguments, environment, ok := startup.EncodeVectors([][]byte{[]byte("cat"), []byte("motd")}, []startup.Variable{{Name: []byte("PATH"), Value: []byte("/system/bin")}}, &wire)
	if !ok {
		t.Fatal("vector encode failed")
	}
	var slot [SlotSize]byte
	data, ok := EncodeRequest(Request{Stages: [MaxStages]Stage{{Path: []byte("/system/bin/cat"), Arguments: arguments}}, Count: 1, Background: true, CWD: []byte("/system"), Environment: environment}, &slot)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(data)
	if !ok || string(request.Stages[0].Path) != "/system/bin/cat" || string(request.CWD) != "/system" || !request.Background {
		t.Fatalf("request = %#v", request)
	}
	replyMessage, ok := EncodeReply(Reply{Process: 12, ExitCode: 7}, 7)
	if !ok {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReply(replyMessage)
	if !ok || reply.Process != 12 || reply.LaunchStatus != 0 || reply.ExitCode != 7 {
		t.Fatalf("reply = %#v", reply)
	}
}

func pipelineRequest(t *testing.T) Request {
	t.Helper()
	request := Request{Count: MaxStages, CWD: []byte("/"), Environment: []byte{0}}
	for index := range request.Stages {
		var wire [startup.VectorSize]byte
		arguments, _, ok := startup.EncodeVectors([][]byte{[]byte("readline")}, nil, &wire)
		if !ok {
			t.Fatal("vectors")
		}
		request.Stages[index] = Stage{Path: []byte("/system/bin/readline"), Arguments: arguments}
	}
	return request
}

func TestPipelineProtocolRoundTrip(t *testing.T) {
	for count := uint8(1); count <= MaxStages; count++ {
		original := pipelineRequest(t)
		original.Count = count
		var slot [SlotSize]byte
		data, ok := EncodeRequest(original, &slot)
		if !ok {
			t.Fatal("pipeline encode failed")
		}
		request, ok := DecodeRequest(data)
		if !ok || request.Count != count || !bytes.Equal(request.CWD, original.CWD) || !bytes.Equal(request.Environment, original.Environment) {
			t.Fatalf("pipeline request = %#v", request)
		}
		for i := 0; i < int(count); i++ {
			if !bytes.Equal(request.Stages[i].Path, original.Stages[i].Path) || !bytes.Equal(request.Stages[i].Arguments, original.Stages[i].Arguments) {
				t.Fatal("stage changed")
			}
		}
	}
}

func TestPipelineProtocolRoundTripsErrorSource(t *testing.T) {
	for count := uint8(2); count <= MaxStages; count++ {
		for source := uint8(1); source < count; source++ {
			original := pipelineRequest(t)
			original.Count, original.ErrorSource = count, source
			var slot [SlotSize]byte
			data, ok := EncodeRequest(original, &slot)
			if !ok {
				t.Fatalf("encode count=%d source=%d", count, source)
			}
			request, ok := DecodeRequest(data)
			if !ok || request.ErrorSource != source || request.Count != count {
				t.Fatalf("decoded request = %#v", request)
			}
		}
	}
}

func TestPipelineProtocolRejectsMalformedData(t *testing.T) {
	var slot [SlotSize]byte
	original, ok := EncodeRequest(pipelineRequest(t), &slot)
	if !ok {
		t.Fatal("encode")
	}
	for size := 0; size < len(original); size++ {
		if _, ok := DecodeRequest(original[:size]); ok {
			t.Fatalf("accepted truncation %d", size)
		}
	}
	if _, ok := DecodeRequest(append(append([]byte(nil), original...), 0)); ok {
		t.Fatal("accepted trailing byte")
	}
	for _, mutate := range []func([]byte){
		func(d []byte) { d[0] = 0 },
		func(d []byte) { d[0] = MaxStages + 1 },
		func(d []byte) { d[1] = 255 },
		func(d []byte) { d[2] = 255 },
		func(d []byte) { d[3] = MaxStages },
		func(d []byte) { d[3] = 255 },
		func(d []byte) { d[4] = 2 },
		func(d []byte) { d[7] = 255 },
		func(d []byte) { d[8] = 255 },
		func(d []byte) { d[9] = '.' },
	} {
		data := append([]byte(nil), original...)
		mutate(data)
		if _, ok := DecodeRequest(data); ok {
			t.Fatal("accepted malformed request")
		}
	}
}

func TestSubmitRoundTripAndBounds(t *testing.T) {
	message, ok := EncodeSubmit(Slots-1, SlotSize, 5)
	slot, size, decoded := DecodeSubmit(message)
	if !ok || !decoded || slot != Slots-1 || size != SlotSize {
		t.Fatalf("submit = %d %d %v %v", slot, size, ok, decoded)
	}
	for _, bad := range []struct {
		slot uint8
		size int
		corr uint64
	}{{Slots, 16, 1}, {0, SlotSize + 1, 1}, {0, 1, 1}, {0, 16, 0}} {
		if _, ok := EncodeSubmit(bad.slot, bad.size, bad.corr); ok {
			t.Fatalf("encoded %#v", bad)
		}
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Version-- },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.Payload[0] = Slots },
		func(m *ipc.Message) { m.Payload[2] = 0xff },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.Correlation = 0 },
	} {
		corrupt := message
		mutate(&corrupt)
		if _, _, ok := DecodeSubmit(corrupt); ok {
			t.Fatal("accepted malformed submit")
		}
	}
}

// TestLongPipelineFitsSlot covers a request that exceeded the former
// 256-byte IPC payload.
func TestLongPipelineFitsSlot(t *testing.T) {
	request := Request{Count: MaxStages, CWD: append([]byte("/"), bytes.Repeat([]byte{'d'}, 95)...), Environment: []byte{1, 4, 11, 'P', 'A', 'T', 'H', '/', 's', 'y', 's', 't', 'e', 'm', '/', 'b', 'i', 'n'}}
	for index := range request.Stages {
		var wire [startup.VectorSize]byte
		arguments, _, ok := startup.EncodeVectors([][]byte{[]byte("readline"), bytes.Repeat([]byte{'a'}, 40)}, nil, &wire)
		if !ok {
			t.Fatal("vectors")
		}
		request.Stages[index] = Stage{Path: []byte("/system/bin/readline"), Arguments: append([]byte(nil), arguments...)}
	}
	var slot [SlotSize]byte
	data, ok := EncodeRequest(request, &slot)
	if !ok || len(data) <= ipc.MaxPayload {
		t.Fatalf("encoded %d bytes, ok=%v", len(data), ok)
	}
	if _, ok := DecodeRequest(data); !ok {
		t.Fatal("long pipeline rejected")
	}
}

func TestPipelineProtocolRejectsInvalidStageAndOverflow(t *testing.T) {
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Count = 0 },
		func(r *Request) { r.Count = MaxStages + 1 },
		func(r *Request) { r.Count, r.ErrorSource = 1, 1 },
		func(r *Request) { r.ErrorSource = MaxStages },
		func(r *Request) { r.ErrorSource = 255 },
		func(r *Request) { r.Stages[3].Path = []byte("/tmp/readline") },
		func(r *Request) { r.Stages[3].Arguments = []byte{1, 8, 'x'} },
		func(r *Request) { r.Environment = []byte{1} },
		func(r *Request) {
			arguments := []byte{4}
			for index := 0; index < 4; index++ {
				arguments = append(append(arguments, 62), bytes.Repeat([]byte{'a'}, 62)...)
			}
			for index := range r.Stages {
				r.Stages[index].Arguments = arguments
			}
		},
	} {
		request := pipelineRequest(t)
		mutate(&request)
		var slot [SlotSize]byte
		if _, ok := EncodeRequest(request, &slot); ok {
			t.Fatal("accepted invalid pipeline")
		}
	}
}

func TestProtocolRejectsInvalidPathsAndResults(t *testing.T) {
	for _, path := range []string{"hello", "/system/../hello", "/system//hello", "/"} {
		var slot [SlotSize]byte
		if _, ok := EncodeRequest(Request{Count: 1, Stages: [MaxStages]Stage{{Path: []byte(path), Arguments: []byte{1, 1, 'x'}}}, CWD: []byte("/"), Environment: []byte{0}}, &slot); ok {
			t.Fatalf("accepted %q", path)
		}
	}
	if _, ok := EncodeReply(Reply{}, 1); ok {
		t.Fatal("accepted empty successful result")
	}
}

func TestResolveExecutableUsesBoundedSystemPath(t *testing.T) {
	var target [MaxPath]byte
	path, ok := ResolveExecutable([]byte("args"), &target)
	if !ok || string(path) != "/system/bin/args" {
		t.Fatalf("resolved path = %q", path)
	}
	command, ok := CommandFromSystemPath(path)
	if !ok || string(command) != "args" {
		t.Fatalf("system command = %q", command)
	}
	if _, ok := CommandFromSystemPath([]byte("/tmp/args")); ok {
		t.Fatal("accepted executable outside system search root")
	}
	for _, command := range [][]byte{nil, []byte("../init"), []byte("nested/tool"), []byte("bad name")} {
		if _, ok := ResolveExecutable(command, &target); ok {
			t.Fatalf("accepted command %q", command)
		}
	}
}

func TestPageInsideSharedWindow(t *testing.T) {
	if PageAddress%PageSize != 0 || PageAddress < vm.UserSharedAddress || PageAddress+PageSize > vm.UserSharedLimit {
		t.Fatalf("exec page %#x outside [%#x, %#x)", PageAddress, vm.UserSharedAddress, vm.UserSharedLimit)
	}
}
