package commands

import (
	"strings"
	"testing"
)

func TestIndexRoundTrip(t *testing.T) {
	var index []byte
	var ok bool
	index, ok = Append(index, "ls", Manifest{Usage: "ls [PATH]", Summary: "list a directory", Options: "-a, --all", MaxArgs: 1, Grants: GrantVFS})
	if !ok {
		t.Fatal("ls refused")
	}
	index, ok = Append(index, "echo", Manifest{Usage: "echo [TEXT...]", Summary: "print its arguments", MaxArgs: Unbounded})
	if !ok {
		t.Fatal("echo refused")
	}
	ls, found := Lookup(index, []byte("ls"))
	if !found || string(ls.Usage) != "ls [PATH]" || string(ls.Options) != "-a, --all" || ls.Grants != GrantVFS || !ls.Accepts(1) || ls.Accepts(2) {
		t.Fatalf("ls = %+v", ls)
	}
	echo, found := Lookup(index, []byte("echo"))
	if !found || echo.Grants != 0 || echo.MaxArgs != Unbounded || !echo.Accepts(40) {
		t.Fatalf("echo = %+v", echo)
	}
	if _, found := Lookup(index, []byte("cat")); found {
		t.Fatal("absent command found")
	}
	names := 0
	for offset := 0; ; names++ {
		_, next, ok := Next(index, offset)
		if !ok {
			break
		}
		offset = next
	}
	if names != 2 {
		t.Fatalf("iterated %d entries", names)
	}
}

func TestAppendRefusesWhatReadersReject(t *testing.T) {
	for name, manifest := range map[string]Manifest{
		"Upper":   {Usage: "x", Summary: "x"},
		"tab":     {Usage: "tab\tx", Summary: "x"},
		"ctl":     {Usage: "ctl", Summary: "\x1b[2J"},
		"option":  {Usage: "option", Summary: "x", Options: "bad\toption"},
		"longopt": {Usage: "longopt", Summary: "x", Options: strings.Repeat("x", MaxOptions+1)},
		"grant":   {Usage: "grant", Summary: "x", Grants: 1 << 7},
		"range":   {Usage: "range", Summary: "x", MinArgs: 2, MaxArgs: 1},
		"huge":    {Usage: "huge", Summary: "x", MaxArgs: MaxArgs + 1},
		"empty":   {Summary: "x"},
		"":        {Usage: "x", Summary: "x"},
		"1digit":  {Usage: "x", Summary: "x"},
	} {
		if _, ok := Append(nil, name, manifest); ok {
			t.Fatalf("%q accepted", name)
		}
	}
}

func TestNextRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{
		"ls\t1\t0\t1\tls\tlist\t",          // no newline
		"ls\t1\t0\t1\tls\tlist\n",          // missing field
		"ls\t1\t0\t1\tls\tlist\t\textra\n", // extra field
		"ls\t16\t0\t1\tls\tlist\t\n",       // unknown grant
		"ls\t1\t2\t1\tls\tlist\t\n",        // max below min
		"ls\t1\t-1\t1\tls\tlist\t\n",       // negative
		"ls\t1\t0\t1000\tls\tlist\t\n",     // over bound
		"LS\t1\t0\t1\tls\tlist\t\n",        // bad name
		"ls\t1\t0\t1\tls\tlist\tbad\x1b\n", // control byte
	} {
		if _, _, ok := Next([]byte(line), 0); ok {
			t.Fatalf("%q accepted", line)
		}
	}
	if _, found := Lookup([]byte("broken\nls\t1\t0\t1\tls\tlist\t\n"), []byte("ls")); found {
		t.Fatal("lookup read past a malformed line")
	}
}

func TestLookupDoesNotAllocate(t *testing.T) {
	index, _ := Append(nil, "ls", Manifest{Usage: "ls [PATH]", Summary: "list", MaxArgs: 1, Grants: GrantVFS})
	name := []byte("ls")
	if allocations := testing.AllocsPerRun(10, func() { Lookup(index, name) }); allocations != 0 {
		t.Fatalf("lookup allocated %v times", allocations)
	}
}
