package supervisor

import (
	"goos/kernel/ipc"
	"testing"
)

func TestLifecycleProtocol(t *testing.T) {
	exited, ok := ProcessExited(7, 0x8006)
	if !ok {
		t.Fatal("exit encode failed")
	}
	event, ok := DecodeLifecycle(exited)
	if !ok || event.Operation != LifecycleProcessExited || event.Process != 7 || event.Code != 0x8006 {
		t.Fatalf("exit event = %#v", event)
	}
	launch, ok := LaunchRequest(3, 9)
	if !ok {
		t.Fatal("launch encode failed")
	}
	event, ok = DecodeLifecycle(launch)
	if !ok || event.Operation != LifecycleLaunch || event.Process != 3 {
		t.Fatalf("launch event = %#v", event)
	}
	launch.Version++
	if _, ok := DecodeLifecycle(launch); ok {
		t.Fatal("wrong protocol version accepted")
	}
	launch.Version = LifecycleVersion
	launch.Correlation = 0
	if _, ok := DecodeLifecycle(launch); ok {
		t.Fatal("uncorrelated launch accepted")
	}
	if _, ok := DecodeLifecycle(ipc.Message{}); ok {
		t.Fatal("empty message accepted")
	}
	result, ok := LaunchResult(8, 0, 9)
	if !ok {
		t.Fatal("result encode failed")
	}
	event, ok = DecodeLifecycle(result)
	if !ok || event.Operation != LifecycleLaunchResult || event.Process != 8 || event.Code != 0 {
		t.Fatalf("launch result = %#v", event)
	}
}
