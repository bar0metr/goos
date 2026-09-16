package exec

import "testing"

func TestProtocolRoundTrip(t *testing.T) {
	message, ok := EncodeRequest(Request{Path: []byte("/system/bin/cat"), CWD: []byte("/system"), Arguments: []byte("motd")}, 7)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || string(request.Path) != "/system/bin/cat" || string(request.CWD) != "/system" || string(request.Arguments) != "motd" {
		t.Fatalf("request = %#v", request)
	}
	replyMessage, ok := EncodeReply(Reply{Process: 12}, 7)
	if !ok {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReply(replyMessage)
	if !ok || reply.Process != 12 || reply.Status != 0 {
		t.Fatalf("reply = %#v", reply)
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
