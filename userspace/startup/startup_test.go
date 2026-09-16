package startup

import "testing"

func TestRoundTrip(t *testing.T) {
	var wire [Size]byte
	if _, ok := Encode(3, []byte("/system"), []byte("motd"), &wire); !ok {
		t.Fatal("encode failed")
	}
	data, ok := Decode(wire[:])
	if !ok || data.Command != 3 || string(data.CWD) != "/system" || string(data.Arguments) != "motd" {
		t.Fatalf("data = %#v", data)
	}
}
