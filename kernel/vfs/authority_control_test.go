package vfs

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"strings"
	"testing"
)

func TestAuthoritySessionControlRoundTrip(t *testing.T) {
	authority := Authority{Confined: true}
	authority.Grants[0], authority.Grants[3] = Read|Write, Read
	message, ok := AddSessionWithAuthorityRequest(7, authority, 9)
	if !ok {
		t.Fatal("encode")
	}
	endpoint, decoded, ok := DecodeAddSessionAuthority(message)
	if !ok || endpoint != 7 || decoded != authority {
		t.Fatalf("decode: %d %+v %v", endpoint, decoded, ok)
	}
	if _, ok := DecodeAddSession(message); ok {
		t.Fatal("legacy decoder discarded authority")
	}
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("session accepted privileged control")
	}
	mutations := []func(*ipc.Message){
		func(m *ipc.Message) { m.Version-- },
		func(m *ipc.Message) { m.Kind = ipc.KindNotification },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.HandleCount = 0 },
		func(m *ipc.Message) { m.Handles[0] = 0 },
		func(m *ipc.Message) { m.PayloadSize-- },
		func(m *ipc.Message) { m.Payload[1] = 2 },
		func(m *ipc.Message) { m.Payload[2] = byte(Write << 1) },
	}
	for index, mutate := range mutations {
		corrupt := message
		mutate(&corrupt)
		if _, _, ok := DecodeAddSessionAuthority(corrupt); ok {
			t.Fatalf("accepted corruption %d", index)
		}
	}
	legacy, _ := AddSessionRequest(8, 10)
	if endpoint, decoded, ok := DecodeAddSessionAuthority(legacy); !ok || endpoint != 8 || decoded != (Authority{}) {
		t.Fatal("legacy default")
	}
}

func TestScopeControlRoundTripAndValidation(t *testing.T) {
	paths := []string{"/state/security/ssh", "/" + strings.Repeat("x", 300)}
	for _, path := range paths {
		object := handle.ID(0)
		if len(path) > ipc.MaxPayload-scopeControlHeader {
			object = 12
		}
		message, ok := InstallScopeRequest(2, []byte(path), true, Read|Write, object, 1)
		if !ok {
			t.Fatal("encode")
		}
		request, ok := DecodeInstallScope(&message)
		if !ok || request.Slot != 2 || !request.Subtree || request.Denied != Read|Write {
			t.Fatal("decode")
		}
		if request.Attached {
			var copied [MaxPath]byte
			if request.TakePath([]byte(path)[:len(path)-1], &copied) {
				t.Fatal("short attached path")
			}
			if !request.TakePath([]byte(path), &copied) {
				t.Fatal("attached path")
			}
			if request.TakePath(append([]byte{0}, []byte(path)[1:]...), &copied) {
				t.Fatal("accepted nul")
			}
			request.TakePath([]byte(path), &copied)
		}
		if string(request.Path) != path {
			t.Fatal("path changed")
		}
		if _, ok := DecodeRequest(message); ok {
			t.Fatal("session accepted install control")
		}
		mutations := []func(*ipc.Message){
			func(m *ipc.Message) { m.Version-- }, func(m *ipc.Message) { m.Correlation = 0 },
			func(m *ipc.Message) { m.Kind = ipc.KindReply }, func(m *ipc.Message) { m.Payload[1] = MaxScopes },
			func(m *ipc.Message) { m.Payload[2] = 2 }, func(m *ipc.Message) { m.Payload[3] = byte(Write << 1) },
			func(m *ipc.Message) { m.PayloadSize-- }, func(m *ipc.Message) { m.HandleCount++ },
			func(m *ipc.Message) { m.Payload[4], m.Payload[5] = 0, 0 },
		}
		for index, mutate := range mutations {
			corrupt := message
			mutate(&corrupt)
			if _, ok := DecodeInstallScope(&corrupt); ok {
				t.Fatalf("accepted corruption %d", index)
			}
		}
	}
	for _, path := range []string{"", "relative", "/bad\x00name", "/" + strings.Repeat("x", MaxPath)} {
		if _, ok := InstallScopeRequest(0, []byte(path), false, Read, 0, 1); ok {
			t.Fatalf("accepted path %q", path)
		}
	}
	if _, ok := InstallScopeRequest(0, []byte("/short"), false, Read, 3, 1); ok {
		t.Fatal("unnecessary attached object")
	}
	if _, ok := InstallScopeRequest(MaxScopes, []byte("/short"), false, Read, 0, 1); ok {
		t.Fatal("invalid slot")
	}
}
