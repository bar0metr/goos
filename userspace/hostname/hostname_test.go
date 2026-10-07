package hostname

import (
	"strings"
	"testing"
)

func TestParseNormalizes(t *testing.T) {
	for text, want := range map[string]string{
		"localhost":        "localhost",
		"LocalHost.":       "localhost",
		"Example.COM":      "example.com",
		"a-b.c1":           "a-b.c1",
		"xn--bcher-kva.ch": "xn--bcher-kva.ch",
		"1.2.3":            "1.2.3",
	} {
		name, ok := Parse([]byte(text))
		if !ok || string(name.Bytes()) != want {
			t.Fatalf("%q: %q %v", text, name.Bytes(), ok)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	long := strings.Repeat("a", 63)
	tooLong := strings.Repeat(long+".", 4)[:254]
	for _, text := range []string{"", ".", "..", "a..b", ".a", "a.", "-a", "a-", "a-.b", "a.-b", "a_b", "a b",
		"é.com", strings.Repeat("a", 64), "a..", tooLong} {
		if text == "a." {
			continue // one trailing dot is accepted
		}
		if name, ok := Parse([]byte(text)); ok {
			t.Fatalf("%q accepted as %q", text, name.Bytes())
		}
	}
	exact := strings.Repeat(long+".", 3) + strings.Repeat("a", 61)
	if name, ok := Parse([]byte(exact)); !ok || name.Size != MaxText {
		t.Fatalf("253-byte name refused")
	}
	if _, ok := Parse([]byte(exact + ".")); !ok {
		t.Fatal("253-byte name with a trailing dot refused")
	}
}

func TestZones(t *testing.T) {
	check := func(text string, localhost, local bool) {
		t.Helper()
		name, ok := Parse([]byte(text))
		if !ok || name.IsLocalhost() != localhost || name.IsLocalScope() != local {
			t.Fatalf("%q: localhost %v local %v", text, name.IsLocalhost(), name.IsLocalScope())
		}
	}
	check("localhost", true, false)
	check("a.b.LOCALHOST.", true, false)
	check("notlocalhost", false, false)
	check("localhost.com", false, false)
	check("printer.local", false, true)
	check("local", false, true)
	check("nonlocal", false, false)
	a, _ := Parse([]byte("Host"))
	b, _ := Parse([]byte("host."))
	if !a.Equal(&b) || !a.IsText("host") {
		t.Fatal("equal names differ")
	}
}
