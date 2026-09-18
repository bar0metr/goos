package exec

import (
	"goos/userspace/startup"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	var wire [96]byte
	arguments, environment, ok := startup.EncodeVectors([][]byte{[]byte("cat"), []byte("motd")}, []startup.Variable{{Name: []byte("PATH"), Value: []byte("/system/bin")}}, &wire)
	if !ok {
		t.Fatal("vector encode failed")
	}
	message, ok := EncodeRequest(Request{Path: []byte("/system/bin/cat"), CWD: []byte("/system"), Arguments: arguments, Environment: environment}, 7)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || string(request.Path) != "/system/bin/cat" || string(request.CWD) != "/system" {
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

func TestPipelineProtocolRoundTrip(t *testing.T) {
	message, ok := EncodeRequest(Request{
		Path: []byte("/system/bin/echo"), CWD: []byte("/"), Arguments: []byte{1, 2}, Environment: []byte{3},
		NextPath: []byte("/system/bin/readline"), NextArguments: []byte{4},
	}, 9)
	if !ok {
		t.Fatal("pipeline encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || string(request.NextPath) != "/system/bin/readline" || len(request.NextArguments) != 1 {
		t.Fatalf("pipeline request = %#v", request)
	}
}

func TestProtocolRejectsInvalidPathsAndResults(t *testing.T) {
	for _, path := range []string{"hello", "/system/../hello", "/system//hello", "/"} {
		if _, ok := EncodeRequest(Request{Path: []byte(path), CWD: []byte("/")}, 1); ok {
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
