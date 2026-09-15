package supervisor

import (
	"goos/internal/capability"
	"goos/kernel/handle"
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
	grants := []Capability{{Handle: 0x100000002, Type: handle.TypeDevice, Rights: capability.Write}}
	launch, ok := LaunchRequest(3, "/system/fault-probe", grants, 9)
	if !ok {
		t.Fatal("launch encode failed")
	}
	event, ok = DecodeLifecycle(launch)
	if !ok || event.Operation != LifecycleLaunch || event.Process != 3 || event.ExecutablePath() != "/system/fault-probe" ||
		event.CapabilityCount != 1 || event.Capabilities[0] != grants[0] {
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
	if _, ok := LaunchRequest(3, "../escape", nil, 9); ok {
		t.Fatal("parent traversal accepted")
	}
	if _, ok := LaunchRequest(3, "/system/./init", nil, 9); ok {
		t.Fatal("non-canonical executable accepted")
	}
	if _, ok := LaunchRequest(3, "system/init", nil, 9); ok {
		t.Fatal("relative executable accepted")
	}
	result, ok := LaunchResult(8, LaunchOK, 9)
	if !ok {
		t.Fatal("result encode failed")
	}
	event, ok = DecodeLifecycle(result)
	if !ok || event.Operation != LifecycleLaunchResult || event.Process != 8 || event.Code != 0 {
		t.Fatalf("launch result = %#v", event)
	}
	if _, ok := LaunchResult(0, LaunchNotFound, 9); !ok {
		t.Fatal("failed launch result rejected")
	}
	if _, ok := LaunchResult(0, LaunchOK, 9); ok {
		t.Fatal("successful result without process accepted")
	}
	if _, ok := LaunchRequest(3, "/system/init", []Capability{{Handle: 1, Type: handle.TypeDevice}}, 9); ok {
		t.Fatal("capability without rights accepted")
	}
	tooMany := make([]Capability, MaxCapabilities+1)
	for index := range tooMany {
		tooMany[index] = Capability{Handle: handle.ID(index + 1), Type: handle.TypeDevice, Rights: capability.Write}
	}
	if _, ok := LaunchRequest(3, "/system/init", tooMany, 9); ok {
		t.Fatal("oversized capability manifest accepted")
	}
}
