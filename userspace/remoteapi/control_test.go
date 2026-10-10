package remoteapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestWorkerLaunchBoundedHandles(t *testing.T) {
	request := WorkerRequest{Operation: WorkerLaunch}
	var message ipc.Message
	if !EncodeWorkerRequest(&request, []handle.ID{1}, 7, &message) {
		t.Fatal("launch encoding")
	}
	var decoded WorkerRequest
	if !DecodeWorkerRequest(&message, &decoded) || decoded.Operation != WorkerLaunch || decoded.Worker != 0 {
		t.Fatal("launch decoding")
	}
	for _, attachments := range [][]handle.ID{nil, {0}, {1, 2}, {1, 2, 3}, {1, 1}} {
		if EncodeWorkerRequest(&request, attachments, 7, &message) {
			t.Fatal("accepted invalid attachments", attachments)
		}
	}
	if !EncodeWorkerRequest(&request, []handle.ID{1}, 7, &message) {
		t.Fatal("encoding")
	}
	message.HandleCount = 255
	if DecodeWorkerRequest(&message, &decoded) {
		t.Fatal("unbounded handle count")
	}
}
func TestWorkerAuthorityExactPathsAndHandles(t *testing.T) {
	request := WorkerRequest{Operation: WorkerAuthority, HostPath: []byte("/state/security/ssh/host"), AuthorizedKeys: []byte("/home/user/.ssh/authorized_keys")}
	var message ipc.Message
	if !EncodeWorkerRequest(&request, nil, 8, &message) {
		t.Fatal("authority encoding")
	}
	var decoded WorkerRequest
	if !DecodeWorkerRequest(&message, &decoded) || string(decoded.HostPath) != string(request.HostPath) || string(decoded.AuthorizedKeys) != string(request.AuthorizedKeys) {
		t.Fatal("authority decoding")
	}
	for _, path := range []string{"/", "relative", "/a/../b", "/a/./b", "/a//b", "/a/", "/a b", "/a\\b"} {
		if ValidAuthorityPath([]byte(path)) {
			t.Fatal("accepted invalid path", path)
		}
	}
	reply := WorkerReply{Operation: WorkerAuthority, Result: ResultOK}
	if !EncodeWorkerAuthorityReply(&reply, []handle.ID{9, 10}, 8, &message) {
		t.Fatal("authority reply encoding")
	}
	var result WorkerReply
	if !DecodeWorkerReply(&message, 8, WorkerAuthority, &result) || result.Worker != 0 {
		t.Fatal("authority reply decoding")
	}
	message.Handles[1] = 9
	if DecodeWorkerReply(&message, 8, WorkerAuthority, &result) {
		t.Fatal("duplicate authority handles")
	}
}
func TestWorkerCloseTokenAndReplyCorrelation(t *testing.T) {
	request := WorkerRequest{Operation: WorkerClose, Worker: 0x100000001}
	var message ipc.Message
	if !EncodeWorkerRequest(&request, nil, 11, &message) {
		t.Fatal("close encoding")
	}
	var decoded WorkerRequest
	if !DecodeWorkerRequest(&message, &decoded) || decoded.Worker != request.Worker {
		t.Fatal("lost token generation")
	}
	reply := WorkerReply{Operation: WorkerClose, Result: ResultOK, Worker: request.Worker}
	if !EncodeWorkerReply(&reply, 11, &message) {
		t.Fatal("reply encoding")
	}
	var result WorkerReply
	if DecodeWorkerReply(&message, 12, WorkerClose, &result) || DecodeWorkerReply(&message, 11, WorkerLaunch, &result) {
		t.Fatal("accepted stale or wrong operation reply")
	}
	if !DecodeWorkerReply(&message, 11, WorkerClose, &result) {
		t.Fatal("reply decoding")
	}
}
func TestAuthenticateHasNoSessionOrGeometry(t *testing.T) {
	request := Request{Operation: OperationAuthenticate}
	var message ipc.Message
	if !EncodeRequest(&request, 13, &message) {
		t.Fatal("authenticate encoding")
	}
	var decoded Request
	if !DecodeRequest(&message, &decoded) || decoded.Operation != OperationAuthenticate {
		t.Fatal("authenticate decoding")
	}
	request.Session = 1
	if EncodeRequest(&request, 13, &message) {
		t.Fatal("authenticate selected owner")
	}
	request.Session = 0
	request.Geometry, _ = MakeGeometry(80, 24, []byte("xterm"))
	if EncodeRequest(&request, 13, &message) {
		t.Fatal("authenticate selected geometry")
	}
	reply := Reply{Operation: OperationAuthenticate, Result: ResultOK, Session: 0x100000001}
	if !EncodeReply(&reply, nil, 13, &message) {
		t.Fatal("authenticate reply encoding")
	}
	var response Reply
	if !DecodeReply(&message, 13, OperationAuthenticate, &response) {
		t.Fatal("authenticate reply decoding")
	}
}

func TestPersistentHostPaths(t *testing.T) {
	for _, path := range []string{"/state/security/ssh/host", "/state/custom/host"} {
		if !ValidHostPath([]byte(path)) {
			t.Fatal("persistent path rejected", path)
		}
	}
	for _, path := range []string{"/state", "/state/", "/stateful/host", "/home/user/host", "/run/host", "/state/a/../host"} {
		if ValidHostPath([]byte(path)) {
			t.Fatal("nonpersistent host path accepted", path)
		}
	}
}

func TestTerminalEndNotice(t *testing.T) {
	const token = uint64(0x400000001)
	message, ok := TerminalEndNotice(token, 37)
	if !ok {
		t.Fatal("notice encoding")
	}
	session, code, ok := DecodeTerminalEndNotice(&message)
	if !ok || session != token || code != 37 {
		t.Fatal("notice lost token/status")
	}
	if _, ok := TerminalEndNotice(0, 37); ok {
		t.Fatal("zero owner")
	}
	original := message
	message.Correlation = 1
	if _, _, ok := DecodeTerminalEndNotice(&message); ok {
		t.Fatal("nonzero correlation")
	}
	message = original
	message.HandleCount = 1
	if _, _, ok := DecodeTerminalEndNotice(&message); ok {
		t.Fatal("notice carried authority")
	}
	message = original
	message.Payload[1] = 1
	if _, _, ok := DecodeTerminalEndNotice(&message); ok {
		t.Fatal("reserved data")
	}
	message = original
	message.Kind = ipc.KindRequest
	if _, _, ok := DecodeTerminalEndNotice(&message); ok {
		t.Fatal("request accepted as notice")
	}
}
