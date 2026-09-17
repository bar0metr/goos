package startup

import (
	"goos/kernel/handle"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var wire [Size]byte
	var vectors [96]byte
	arguments, environment, ok := EncodeVectors([][]byte{[]byte("cat"), []byte("motd")}, []Variable{{Name: []byte("PATH"), Value: []byte("/system/bin")}}, &vectors)
	if !ok {
		t.Fatal("vector encode failed")
	}
	handles := []Handle{{Name: []byte("stdout"), ID: 0x100000001}, {Name: []byte("stderr"), ID: 0x100000002}, {Name: []byte("vfs"), ID: 0x100000003}}
	encoded, ok := Encode([]byte("cat"), []byte("/system"), arguments, environment, handles, &wire)
	if !ok {
		t.Fatal("encode failed")
	}
	data, ok := Decode(encoded)
	argument, argumentFound := data.Arg(1)
	path, pathFound := data.Env("PATH")
	if !ok || string(data.Command) != "cat" || string(data.CWD) != "/system" || !argumentFound || string(argument) != "motd" || !pathFound || string(path) != "/system/bin" {
		t.Fatalf("data = %#v", data)
	}
	if stdout, found := data.Handle("stdout"); !found || stdout != handles[0].ID {
		t.Fatalf("stdout = %#x, %v", stdout, found)
	}
	if stderr, found := data.Handle("stderr"); !found || stderr != handles[1].ID {
		t.Fatalf("stderr = %#x, %v", stderr, found)
	}
}

func TestBindHandles(t *testing.T) {
	var templateWire, boundWire [Size]byte
	arguments, environment := testVectors(t)
	template, ok := Encode([]byte("cat"), []byte("/"), arguments, environment, []Handle{{Name: []byte("stdout")}, {Name: []byte("vfs")}}, &templateWire)
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
	arguments, environment := testVectors(t)
	if _, ok := Encode([]byte("cat"), []byte("/"), arguments, environment, []Handle{{Name: []byte("vfs")}, {Name: []byte("vfs")}}, &wire); ok {
		t.Fatal("duplicate handle name accepted")
	}
	template, ok := Encode([]byte("cat"), []byte("/"), arguments, environment, []Handle{{Name: []byte("vfs")}}, &wire)
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

func TestRejectsMalformedVectors(t *testing.T) {
	var wire [96]byte
	if _, _, ok := EncodeVectors([][]byte{[]byte("tool")}, []Variable{{Name: []byte("bad"), Value: []byte("value")}}, &wire); ok {
		t.Fatal("lowercase environment name accepted")
	}
	arguments := []byte{1, 4, 't', 'o', 'o', 'l'}
	environment := []byte{2, 1, 1, 'A', '1', 1, 1, 'A', '2'}
	if _, _, ok := DecodeVectors(arguments, environment); ok {
		t.Fatal("duplicate environment name accepted")
	}
}

func TestExternalUtilityStandardErrorBindingsFit(t *testing.T) {
	var vectors [96]byte
	arguments, environment, ok := EncodeVectors(
		[][]byte{[]byte("cp"), []byte("/system/motd"), []byte("/persist/state.txt")},
		[]Variable{{Name: []byte("PATH"), Value: []byte("/system/bin")}},
		&vectors,
	)
	if !ok {
		t.Fatal("vector encode failed")
	}
	var wire [Size]byte
	bindings := []Handle{{Name: []byte("stdout")}, {Name: []byte("stderr")}, {Name: []byte("vfs")}}
	if _, ok = Encode([]byte("cp"), []byte("/system"), arguments, environment, bindings, &wire); !ok {
		t.Fatal("standard stream bindings exceed startup ABI")
	}
}

func testVectors(t *testing.T) ([]byte, []byte) {
	t.Helper()
	var wire [96]byte
	arguments, environment, ok := EncodeVectors([][]byte{[]byte("cat")}, nil, &wire)
	if !ok {
		t.Fatal("vector encode failed")
	}
	return append([]byte(nil), arguments...), append([]byte(nil), environment...)
}
