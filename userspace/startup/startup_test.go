package startup

import (
	"goos/kernel/handle"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var wire [Size]byte
	handles := []Handle{{Name: []byte("stdout"), ID: 0x100000001}, {Name: []byte("vfs"), ID: 0x100000002}}
	encoded, ok := Encode([]byte("cat"), []byte("/system"), []byte("motd"), handles, &wire)
	if !ok {
		t.Fatal("encode failed")
	}
	data, ok := Decode(encoded)
	if !ok || string(data.Command) != "cat" || string(data.CWD) != "/system" || string(data.Arguments) != "motd" {
		t.Fatalf("data = %#v", data)
	}
	if stdout, found := data.Handle("stdout"); !found || stdout != handles[0].ID {
		t.Fatalf("stdout = %#x, %v", stdout, found)
	}
}

func TestBindHandles(t *testing.T) {
	var templateWire, boundWire [Size]byte
	template, ok := Encode([]byte("cat"), []byte("/"), nil, []Handle{{Name: []byte("stdout")}, {Name: []byte("vfs")}}, &templateWire)
	if !ok {
		t.Fatal("template encode failed")
	}
	if _, ok := Decode(template); ok {
		t.Fatal("unbound template accepted as process startup")
	}
	bound, ok := BindHandles(template, []handle.ID{0x100000004, 0x100000009}, &boundWire)
	if !ok {
		t.Fatal("bind failed")
	}
	data, ok := Decode(bound)
	if !ok {
		t.Fatal("bound startup rejected")
	}
	if stdout, _ := data.Handle("stdout"); stdout != 0x100000004 {
		t.Fatalf("stdout = %#x", stdout)
	}
}

func TestRejectsMalformedStartup(t *testing.T) {
	var wire [Size]byte
	if _, ok := Encode([]byte("cat"), []byte("/"), nil, []Handle{{Name: []byte("vfs")}, {Name: []byte("vfs")}}, &wire); ok {
		t.Fatal("duplicate handle name accepted")
	}
	template, ok := Encode([]byte("cat"), []byte("/"), nil, []Handle{{Name: []byte("vfs")}}, &wire)
	if !ok {
		t.Fatal("valid template rejected")
	}
	var bound [Size]byte
	if _, ok := BindHandles(template, nil, &bound); ok {
		t.Fatal("binding count mismatch accepted")
	}
	corrupt := append([]byte(nil), template...)
	corrupt[4]++
	if _, ok := BindHandles(corrupt, []handle.ID{1}, &bound); ok {
		t.Fatal("unknown version accepted")
	}
}
