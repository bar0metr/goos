package main

import "testing"

func TestGoroutineMarkerRequiresOneCompletedFixture(t *testing.T) {
	marker := "GOPROBE_POOL_OK"
	if err := goroutineMarker("GOPROBE_POOL jobs=100 sum=9900\r\n"+marker+"\r\n", marker); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"", "prefix " + marker, marker + " suffix", marker + "\n" + marker,
		"GOPROBE_FAIL corrupt result\n" + marker,
		"fatal error: deadlock\n" + marker,
	} {
		if err := goroutineMarker(output, marker); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

func TestGoroutineRecordRequiresOneNamedRecord(t *testing.T) {
	name := "GOPROBE_WAITSETS_JOIN"
	if err := goroutineRecord("GOPROBE_WAITSETS polls=3 mask=1\r\n"+name+" spins=40\r\nGOPROBE_WAITSETS_OK\r\n", name); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"", name, "GOPROBE_WAITSETS_JOINED spins=1", name + " spins=1\n" + name + " spins=2",
	} {
		if err := goroutineRecord(output, name); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}
