package dnswire

import (
	"bytes"
	"encoding/binary"
	"testing"

	"goos/userspace/hostname"
)

func TestDecodeName(t *testing.T) {
	message := append([]byte{0xff}, wire("Mail.Example.COM")...)
	message = append(message, 3, 'w', 'w', 'w', 0xc0, 6) // www.example.com
	message = append(message, 0xc0, 23)                  // pointer to a pointer
	var out Name
	end, ok := DecodeName(message, 1, &out)
	if !ok || end != 19 || !bytes.Equal(out.Bytes(), wire("mail.example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.Bytes())
	}
	end, ok = DecodeName(message, 19, &out)
	if !ok || end != 25 || !bytes.Equal(out.Bytes(), wire("www.example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.Bytes())
	}
	end, ok = DecodeName(message, 25, &out)
	if !ok || end != 27 || !bytes.Equal(out.Bytes(), wire("example.com")) {
		t.Fatalf("%v %d %q", ok, end, out.Bytes())
	}
	for _, offset := range []int{-1, len(message), 1000} {
		if _, ok := DecodeName(message, offset, &out); ok {
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
	if _, ok := DecodeName(longest, 0, &out); !ok || out.size != 255 {
		t.Fatalf("255-byte name: %v %d", ok, out.size)
	}
	longest[len(longest)-63] = 62
	longest = append(longest[:len(longest)-1], 'x', 0)
	if _, ok := DecodeName(longest, 0, &out); ok {
		t.Fatal("256-byte name accepted")
	}
}

func TestDecodeNameRefusals(t *testing.T) {
	label64 := append([]byte{64}, make([]byte, 64)...)
	cases := map[string][]byte{
		"forward pointer":   {0xc0, 2, 0},
		"self pointer":      {0xc0, 0},
		"loop":              {1, 'a', 0xc0, 0},
		"label 64":          append(label64, 0),
		"extended label":    {0x40, 0},
		"reserved label":    {0x80, 0},
		"cut label":         {5, 'a', 'b'},
		"cut pointer":       {0xc0},
		"no root":           {1, 'a'},
		"pointer past data": {0, 0xc0, 9},
	}
	for what, data := range cases {
		var out Name
		offset := 0
		if what == "pointer past data" {
			offset = 1
		}
		if _, ok := DecodeName(data, offset, &out); ok {
			t.Errorf("%s accepted", what)
		}
	}
	// The pointer budget: a chain of 64 backward pointers is followed, 65
	// is not.
	// Each pointer points at the one before it, the first at the root.
	chain := []byte{0}
	for index := 0; index < MaxPointers+1; index++ {
		target := 0
		if index > 0 {
			target = len(chain) - 2
		}
		chain = append(chain, 0xc0, byte(target))
	}
	var out Name
	if _, ok := DecodeName(chain, len(chain)-2, &out); ok {
		t.Fatal("65 pointers followed")
	}
	if _, ok := DecodeName(chain, len(chain)-4, &out); !ok {
		t.Fatal("64 pointers refused")
	}
	// A 63-byte label is the longest.
	label63 := append(append([]byte{63}, bytes.Repeat([]byte{'a'}, 63)...), 0)
	if _, ok := DecodeName(label63, 0, &out); !ok {
		t.Fatal("63-byte label refused")
	}
}

func TestNameHost(t *testing.T) {
	var out Name
	host, _ := hostname.Parse([]byte("a.example.com"))
	out.SetHost(&host)
	if !bytes.Equal(out.Bytes(), wire("a.example.com")) {
		t.Fatalf("%q", out.Bytes())
	}
	var back hostname.Name
	if !out.Host(&back) || !back.Equal(&host) {
		t.Fatalf("round trip %q", back.Bytes())
	}
	for _, raw := range [][]byte{{0}, append([]byte{3, 'a', '.', 'b'}, 0), append([]byte{3, 'a', '_', 'b'}, 0)} {
		DecodeName(raw, 0, &out)
		if out.Host(&back) {
			t.Fatalf("%q converted", raw)
		}
	}
	for _, c := range []struct {
		name, zone string
		want       bool
	}{
		{"a.b.c", "b.c", true}, {"a.b.c", "a.b.c", true}, {"a.b.c", ".", true}, {"a.b.c", "c", true},
		{"a.b.c", "x.c", false}, {"ab.c", "b.c", false}, {"b.c", "a.b.c", false},
	} {
		if named(t, c.name).Within(named(t, c.zone)) != c.want {
			t.Fatalf("%s within %s", c.name, c.zone)
		}
	}
}

func TestSetText(t *testing.T) {
	for text, want := range map[string]string{
		"Example.COM":             "example.com",
		"example.com.":            "example.com",
		".":                       ".",
		"_sip._tcp.example":       "_sip._tcp.example",
		"80.2.0.192.in-addr.arpa": "80.2.0.192.in-addr.arpa",
	} {
		var name Name
		if !name.SetText([]byte(text)) || !bytes.Equal(name.Bytes(), wire(want)) {
			t.Fatalf("%q: %q", text, name.Bytes())
		}
	}
	long := bytes.Repeat([]byte("a"), 63)
	var longest []byte
	for index := 0; index < 3; index++ {
		longest = append(append(longest, long...), '.')
	}
	longest = append(longest, bytes.Repeat([]byte("b"), 61)...)
	for _, bad := range [][]byte{nil, []byte(""), []byte(".."), []byte("a..b"), []byte(".a"),
		[]byte("a b"), []byte("a\\.b"), []byte("a\x1b"), []byte("a\x80"), append(long, 'x'), append(longest, 'b')} {
		var name Name
		if name.SetText(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
	var name Name
	if !name.SetText(longest) || name.size != MaxName {
		t.Fatalf("longest name: %d", name.size)
	}
}

func TestEncodeQuery(t *testing.T) {
	name := named(t, "example.com")
	buffer := make([]byte, MaxQuery)
	for _, qtype := range []uint16{TypeA, TypeAAAA, TypeCNAME, TypeNS, TypeMX, TypeTXT, TypeSOA, TypePTR} {
		length := EncodeQuery(buffer, 0xabcd, name, qtype, false)
		want := append([]byte{0xab, 0xcd, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}, wire("example.com")...)
		want = append(append(want, u16(qtype)...), 0, 1)
		if !bytes.Equal(buffer[:length], want) {
			t.Fatalf("type %d: % x", qtype, buffer[:length])
		}
	}
	length := EncodeQuery(buffer, 1, name, TypeMX, true)
	query := buffer[:length]
	if binary.BigEndian.Uint16(query[10:]) != 1 {
		t.Fatal("no additional count")
	}
	optRecord := query[length-OPTSize:]
	if !bytes.Equal(optRecord, []byte{0, 0, TypeOPT, 0x04, 0xd0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("OPT % x", optRecord)
	}
	// The longest name fits MaxQuery with OPT.
	var longest Name
	longest.size = MaxName
	if EncodeQuery(buffer, 1, &longest, TypeA, true) != MaxQuery {
		t.Fatal("MaxQuery")
	}
}

func TestParseType(t *testing.T) {
	for text, want := range map[string]uint16{"a": TypeA, "AAAA": TypeAAAA, "cname": TypeCNAME, "Ns": TypeNS,
		"MX": TypeMX, "txt": TypeTXT, "SOA": TypeSOA, "ptr": TypePTR} {
		if got, ok := ParseType([]byte(text)); !ok || got != want {
			t.Fatalf("%s: %d %v", text, got, ok)
		}
	}
	for _, bad := range []string{"", "OPT", "ANY", "AX", "TYPE1", "A "} {
		if _, ok := ParseType([]byte(bad)); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}
