package dns

import (
	"testing"

	"goos/userspace/hostname"
)

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()
	want := Policy{CacheEntries: 64, MaxTTL: 86400, MaxNegative: 3600, FailureTTL: 5,
		QueryTimeout: 3000 * millisecond, Attempts: 2}
	if policy != want || policy.Validate() != -1 {
		t.Fatalf("%+v", policy)
	}
}

func TestParsePolicy(t *testing.T) {
	good := [PolicyFields]uint64{64, 86400, 3600, 5, 3000, 2}
	policy, bad := ParsePolicy(&good)
	if bad != -1 || policy != DefaultPolicy() {
		t.Fatalf("%+v %d", policy, bad)
	}
	low := [PolicyFields]uint64{1, 1, 1, 1, 500, 1}
	high := [PolicyFields]uint64{256, 604800, 86400, 300, 30000, 3}
	for _, values := range [][PolicyFields]uint64{low, high} {
		policy, bad := ParsePolicy(&values)
		if bad != -1 || policy.Validate() != -1 {
			t.Fatalf("%v: %+v %d", values, policy, bad)
		}
	}
	for index := 0; index < PolicyFields; index++ {
		// Values that would wrap into range when narrowed, or multiplied.
		for _, value := range []uint64{0, low[index] - 1, high[index] + 1, 1 << 16, 1<<32 + 64, 1<<63 + 3000,
			^uint64(0)} {
			if value >= low[index] && value <= high[index] {
				continue
			}
			values := good
			values[index] = value
			if _, bad := ParsePolicy(&values); bad != index {
				t.Fatalf("field %d value %d: bad %d", index, value, bad)
			}
		}
	}
	// The first bad value is reported.
	values := good
	values[2], values[4] = 0, 0
	if _, bad := ParsePolicy(&values); bad != 2 {
		t.Fatalf("bad %d", bad)
	}
}

func TestPolicyValidate(t *testing.T) {
	policy := DefaultPolicy()
	policy.QueryTimeout = 499 * millisecond
	if policy.Validate() != 4 {
		t.Fatal("short timeout accepted")
	}
	policy = DefaultPolicy()
	policy.Attempts = 4
	if policy.Validate() != 5 {
		t.Fatal("four attempts accepted")
	}
	policy = DefaultPolicy()
	policy.CacheEntries = MaxCacheEntries + 1
	if policy.Validate() != 0 {
		t.Fatal("oversized cache accepted")
	}
}

func TestParseServers(t *testing.T) {
	cases := []struct {
		texts [4]string
		want  [4][4]byte
		bad   int
	}{
		{[4]string{"192.0.2.1", "0.0.0.0", "", "198.51.100.7"},
			[4][4]byte{{192, 0, 2, 1}, {}, {}, {198, 51, 100, 7}}, -1},
		{[4]string{"0.0.0.0", "0.0.0.0", "0.0.0.0", "0.0.0.0"}, [4][4]byte{}, -1},
		{[4]string{"192.0.2.1", "192.0.2.01"}, [4][4]byte{}, 1},
		{[4]string{"192.0.2.1", "192.0.2"}, [4][4]byte{}, 1},
		{[4]string{"256.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{" 1.2.3.4"}, [4][4]byte{}, 0},
		{[4]string{"1.2.3.4.5"}, [4][4]byte{}, 0},
		{[4]string{"1.1.1.1", "255.255.255.255"}, [4][4]byte{}, 1},
		{[4]string{"224.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{"240.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{"127.0.0.53"}, [4][4]byte{}, 0},
		{[4]string{"0.1.2.3"}, [4][4]byte{}, 0},
		{[4]string{"8.8.8.8", "0.0.0.0", "8.8.8.8"}, [4][4]byte{}, 2},
	}
	for _, c := range cases {
		var texts [4][]byte
		for index, text := range c.texts {
			texts[index] = []byte(text)
		}
		got, bad := ParseServers(&texts)
		if got != c.want || bad != c.bad {
			t.Fatalf("%q: %v %d, want %v %d", c.texts, got, bad, c.want, c.bad)
		}
	}
}

func TestDecodeName(t *testing.T) {
	message := append([]byte{0xff}, wire("Mail.Example.COM")...)
	message = append(message, 3, 'w', 'w', 'w', 0xc0, 6) // www.example.com
	message = append(message, 0xc0, 23)                  // pointer to a pointer
	var out wireName
	end, ok := decodeName(message, 1, &out)
	if !ok || end != 19 || string(out.data[:out.size]) != string(wire("mail.example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.data[:out.size])
	}
	end, ok = decodeName(message, 19, &out)
	if !ok || end != 25 || string(out.data[:out.size]) != string(wire("www.example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.data[:out.size])
	}
	end, ok = decodeName(message, 25, &out)
	if !ok || end != 27 || string(out.data[:out.size]) != string(wire("example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.data[:out.size])
	}
	for _, offset := range []int{-1, len(message), 1000} {
		if _, ok := decodeName(message, offset, &out); ok {
			t.Fatalf("offset %d accepted", offset)
		}
	}
	// The longest name, 255 bytes in wire form, and one byte more.
	var longest []byte
	for index := 0; index < 3; index++ {
		longest = append(longest, 63)
		longest = append(longest, make([]byte, 63)...)
	}
	longest = append(longest, 61)
	longest = append(longest, make([]byte, 61)...)
	longest = append(longest, 0)
	if _, ok := decodeName(longest, 0, &out); !ok || out.size != 255 {
		t.Fatalf("255-byte name: %v %d", ok, out.size)
	}
	longest[len(longest)-63] = 62
	longest = append(longest[:len(longest)-1], 'x', 0)
	if _, ok := decodeName(longest, 0, &out); ok {
		t.Fatal("256-byte name accepted")
	}
}

func TestWireHost(t *testing.T) {
	var out wireName
	host, _ := hostname.Parse([]byte("a.example.com"))
	out.setHost(&host)
	if string(out.data[:out.size]) != string(wire("a.example.com")) {
		t.Fatalf("%q", out.data[:out.size])
	}
	var back hostname.Name
	if !out.host(&back) || !back.Equal(&host) {
		t.Fatalf("round trip %q", back.Bytes())
	}
	for _, raw := range [][]byte{{0}, append([]byte{3, 'a', '.', 'b'}, 0), append([]byte{3, 'a', '_', 'b'}, 0)} {
		copy(out.data[:], raw)
		out.size = uint8(len(raw))
		if out.host(&back) {
			t.Fatalf("%q converted", raw)
		}
	}
	zone, name := wireName{}, wireName{}
	for _, c := range []struct {
		name, zone string
		want       bool
	}{
		{"a.b.c", "b.c", true}, {"a.b.c", "a.b.c", true}, {"a.b.c", ".", true}, {"a.b.c", "c", true},
		{"a.b.c", "x.c", false}, {"ab.c", "b.c", false}, {"b.c", "a.b.c", false},
	} {
		copy(name.data[:], wire(c.name))
		name.size = uint8(len(wire(c.name)))
		copy(zone.data[:], wire(c.zone))
		zone.size = uint8(len(wire(c.zone)))
		if name.within(&zone) != c.want {
			t.Fatalf("%s within %s", c.name, c.zone)
		}
	}
}

func TestEncodeQuery(t *testing.T) {
	var name wireName
	host, _ := hostname.Parse([]byte("example.com"))
	name.setHost(&host)
	buffer := make([]byte, maxQuerySize)
	length := encodeQuery(buffer, 0xabcd, &name, false)
	if length != headerSize+len(wire("example.com"))+questionFixed {
		t.Fatalf("classic length %d", length)
	}
	q := decodeQuery(t, Outgoing{Length: length}, buffer[:length])
	if q.opt || q.id != 0xabcd || q.name != "example.com" {
		t.Fatalf("%+v", q)
	}
	length = encodeQuery(buffer, 1, &name, true)
	if q = decodeQuery(t, Outgoing{Length: length}, buffer[:length]); !q.opt || q.payload != DatagramSize {
		t.Fatalf("%+v", q)
	}
}
