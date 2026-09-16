package startup

import "testing"

func TestRoundTrip(t *testing.T) {
	var wire [Size]byte
	if _, ok := Encode([]byte("cat"), []byte("/system"), []byte("motd"), &wire); !ok {
		t.Fatal("encode failed")
	}
	data, ok := Decode(wire[:])
	if !ok || string(data.Command) != "cat" || string(data.CWD) != "/system" || string(data.Arguments) != "motd" {
		t.Fatalf("data = %#v", data)
	}
}
