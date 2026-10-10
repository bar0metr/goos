package remoteapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestWorkerLaunchReturnsControllerRelayEnds(t *testing.T) {
	reply := WorkerReply{Operation: WorkerLaunch, Result: ResultOK, Worker: 0x100000001}
	var message ipc.Message
	if !EncodeWorkerLaunchReply(&reply, []handle.ID{9, 10}, 7, &message) {
		t.Fatal("launch reply encoding")
	}
	var decoded WorkerReply
	if !DecodeWorkerReply(&message, 7, WorkerLaunch, &decoded) || decoded != reply || message.HandleCount != 2 {
		t.Fatal("launch reply decoding")
	}
	for _, handles := range [][]handle.ID{nil, {9}, {0, 10}, {9, 9}, {9, 10, 11}} {
		if EncodeWorkerLaunchReply(&reply, handles, 7, &message) {
			t.Fatal("invalid relay attachments", handles)
		}
	}
	if !EncodeWorkerReply(&reply, 7, &message) || DecodeWorkerReply(&message, 7, WorkerLaunch, &decoded) {
		t.Fatal("successful launch without relay ends accepted")
	}
	reply.Result = ResultUnavailable
	if !EncodeWorkerLaunchReply(&reply, nil, 7, &message) || !DecodeWorkerReply(&message, 7, WorkerLaunch, &decoded) {
		t.Fatal("failed launch decoding")
	}
	if EncodeWorkerLaunchReply(&reply, []handle.ID{9, 10}, 7, &message) {
		t.Fatal("failed launch carried handles")
	}
}
