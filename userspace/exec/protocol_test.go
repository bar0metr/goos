package exec

import (
	"bytes"
	"goos/kernel/ipc"
	"goos/userspace/startup"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	var wire [96]byte
	arguments, environment, ok := startup.EncodeVectors([][]byte{[]byte("cat"), []byte("motd")}, []startup.Variable{{Name: []byte("PATH"), Value: []byte("/system/bin")}}, &wire)
	if !ok {
		t.Fatal("vector encode failed")
	}
	message, ok := EncodeRequest(Request{Stages: [MaxStages]Stage{{Path: []byte("/system/bin/cat"), Arguments: arguments}}, Count: 1, CWD: []byte("/system"), Environment: environment}, 7)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || string(request.Stages[0].Path) != "/system/bin/cat" || string(request.CWD) != "/system" {
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
		var wire [96]byte
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
		message, ok := EncodeRequest(original, 9)
		if !ok {
			t.Fatal("pipeline encode failed")
		}
		request, ok := DecodeRequest(message)
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

func TestPipelineProtocolRejectsMalformedWire(t *testing.T) {
	original, ok := EncodeRequest(pipelineRequest(t), 1)
	if !ok {
		t.Fatal("encode")
	}
	for size := uint16(0); size < original.PayloadSize; size++ {
		message := original
		message.PayloadSize = size
		if _, ok := DecodeRequest(message); ok {
			t.Fatalf("accepted truncation %d", size)
		}
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Version-- },
		func(m *ipc.Message) { m.PayloadSize = ipc.MaxPayload + 1 },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.Payload[0] = 0 },
		func(m *ipc.Message) { m.Payload[0] = MaxStages + 1 },
		func(m *ipc.Message) { m.Payload[1] = 255 },
		func(m *ipc.Message) { m.Payload[2] = 255 },
		func(m *ipc.Message) { m.Payload[3] = 1 },
		func(m *ipc.Message) { m.Payload[6] = 255 },
		func(m *ipc.Message) { m.Payload[7] = 255 },
		func(m *ipc.Message) { m.Payload[8] = '.' },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.Correlation = 0 },
	} {
		message := original
		mutate(&message)
		if _, ok := DecodeRequest(message); ok {
			t.Fatal("accepted malformed message")
		}
	}
}

func TestPipelineProtocolRejectsInvalidStageAndOverflow(t *testing.T) {
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Count = 0 },
		func(r *Request) { r.Count = MaxStages + 1 },
		func(r *Request) { r.Stages[3].Path = []byte("/tmp/readline") },
		func(r *Request) { r.Stages[3].Arguments = []byte{1, 8, 'x'} },
		func(r *Request) { r.Environment = []byte{1} },
		func(r *Request) { r.CWD = append([]byte("/"), bytes.Repeat([]byte{'a'}, 191)...) },
	} {
		request := pipelineRequest(t)
		mutate(&request)
		if _, ok := EncodeRequest(request, 1); ok {
			t.Fatal("accepted invalid pipeline")
		}
	}
}

func TestProtocolRejectsInvalidPathsAndResults(t *testing.T) {
	for _, path := range []string{"hello", "/system/../hello", "/system//hello", "/"} {
		if _, ok := EncodeRequest(Request{Count: 1, Stages: [MaxStages]Stage{{Path: []byte(path), Arguments: []byte{1, 1, 'x'}}}, CWD: []byte("/"), Environment: []byte{0}}, 1); ok {
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
