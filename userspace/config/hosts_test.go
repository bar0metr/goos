package config

import (
	"bytes"
	"fmt"
	"goos/userspace/hostname"
	"strings"
	"testing"
)

func mustName(t testing.TB, text string) *hostname.Name {
	t.Helper()
	name, ok := hostname.Parse([]byte(text))
	if !ok {
		t.Fatalf("%q does not parse", text)
	}
	return &name
}

func listHosts(hosts *Hosts) []string {
	var lines []string
	for cursor := 0; ; {
		name, address, next, status := hosts.At(cursor)
		if status != StatusOK {
			return lines
		}
		lines = append(lines, fmt.Sprintf("%d.%d.%d.%d %s\n", address[0], address[1], address[2], address[3], name))
		cursor = next
	}
}

func TestHostsLocalhostIsPolicy(t *testing.T) {
	var hosts Hosts
	if address, status := hosts.Lookup(mustName(t, "LocalHost.")); status != StatusOK || address != [4]byte{127, 0, 0, 1} {
		t.Fatalf("localhost = %v %v", address, status)
	}
	for _, name := range []string{"localhost", "LOCALHOST", "a.localhost"} {
		if status := hosts.Set(mustName(t, name), [4]byte{10, 0, 0, 1}); status != StatusReadOnly {
			t.Fatalf("set %s = %v", name, status)
		}
		if status := hosts.Remove(mustName(t, name)); status != StatusReadOnly {
			t.Fatalf("remove %s = %v", name, status)
		}
	}
	if _, status := hosts.Lookup(mustName(t, "a.localhost")); status != StatusNotFound {
		t.Fatalf("a name under localhost answered by the table: %v", status)
	}
	if hosts.Generation != 0 || hosts.Count() != 0 {
		t.Fatal("refused changes changed the table")
	}
	name, address, next, status := hosts.At(0)
	if status != StatusOK || string(name) != "localhost" || address != LocalhostAddress || next != 1 {
		t.Fatalf("first entry %q %v %d %v", name, address, next, status)
	}
}

func TestHostsSetReplaceRemove(t *testing.T) {
	var hosts Hosts
	if hosts.Set(mustName(t, "NAS.Example."), [4]byte{10, 0, 0, 2}) != StatusOK ||
		hosts.Set(mustName(t, "build"), [4]byte{127, 0, 1, 1}) != StatusOK {
		t.Fatal("set refused")
	}
	if hosts.Generation != 2 || hosts.Count() != 2 {
		t.Fatalf("generation %d count %d", hosts.Generation, hosts.Count())
	}
	// The same address again is not a change.
	if hosts.Set(mustName(t, "nas.example"), [4]byte{10, 0, 0, 2}) != StatusOK || hosts.Generation != 2 {
		t.Fatal("an unchanged set counted as a change")
	}
	if hosts.Set(mustName(t, "nas.EXAMPLE"), [4]byte{10, 0, 0, 3}) != StatusOK || hosts.Generation != 3 || hosts.Count() != 2 {
		t.Fatal("replace")
	}
	if address, status := hosts.Lookup(mustName(t, "nas.example.")); status != StatusOK || address != [4]byte{10, 0, 0, 3} {
		t.Fatalf("lookup = %v %v", address, status)
	}
	if got := strings.Join(listHosts(&hosts), ""); got != "127.0.0.1 localhost\n10.0.0.3 nas.example\n127.0.1.1 build\n" {
		t.Fatalf("listing %q", got)
	}
	if hosts.Remove(mustName(t, "missing")) != StatusNotFound || hosts.Generation != 3 {
		t.Fatal("removing a missing name")
	}
	if hosts.Remove(mustName(t, "NAS.example")) != StatusOK || hosts.Generation != 4 || hosts.Count() != 1 {
		t.Fatal("remove")
	}
	if _, status := hosts.Lookup(mustName(t, "nas.example")); status != StatusNotFound {
		t.Fatal("removed name still answers")
	}
	if name, _, _, status := hosts.At(1); status != StatusOK || string(name) != "build" {
		t.Fatalf("record after removal %q %v", name, status)
	}
	if _, _, next, status := hosts.At(2); status != StatusNotFound || next != 2 {
		t.Fatalf("past the end %d %v", next, status)
	}
}

func TestHostsRejectsAddresses(t *testing.T) {
	var hosts Hosts
	for _, address := range [][4]byte{{0, 0, 0, 0}, {0, 1, 2, 3}, {255, 255, 255, 255}, {224, 0, 0, 1}, {239, 1, 1, 1}, {240, 0, 0, 1}} {
		if status := hosts.Set(mustName(t, "a"), address); status != StatusInvalidArgument {
			t.Fatalf("%v accepted: %v", address, status)
		}
	}
	for _, address := range [][4]byte{{1, 0, 0, 0}, {127, 0, 0, 2}, {223, 255, 255, 255}, {192, 168, 1, 255}} {
		if !ValidHostAddress(address) {
			t.Fatalf("%v refused", address)
		}
	}
}

func TestHostsNameLimitAndFullTable(t *testing.T) {
	var hosts Hosts
	long := strings.Repeat(strings.Repeat("a", 61)+".", 4) // 248 bytes
	if len(long) != MaxHostName {
		t.Fatal(len(long))
	}
	if hosts.Set(mustName(t, long), [4]byte{10, 0, 0, 1}) != StatusOK {
		t.Fatal("longest name refused")
	}
	if hosts.Set(mustName(t, long+"a"), [4]byte{10, 0, 0, 1}) != StatusInvalidArgument {
		t.Fatal("name over MaxHostName accepted")
	}
	for index := 1; index < MaxHosts; index++ {
		if status := hosts.Set(mustName(t, fmt.Sprintf("h%d", index)), [4]byte{10, 0, 0, byte(index)}); status != StatusOK {
			t.Fatalf("record %d: %v", index, status)
		}
	}
	if hosts.Set(mustName(t, "one-more"), [4]byte{10, 0, 1, 1}) != StatusResourceExhausted {
		t.Fatal("a full table took another record")
	}
	// Replacing a record in a full table is not growth.
	if hosts.Set(mustName(t, long), [4]byte{10, 0, 0, 9}) != StatusOK {
		t.Fatal("replace in a full table")
	}
	// localhost answers whatever the table holds.
	if address, status := hosts.Lookup(mustName(t, "localhost")); status != StatusOK || address != LocalhostAddress {
		t.Fatal("localhost lost to a full table")
	}
}

func TestParseHostsFile(t *testing.T) {
	file := "# hosts\n" +
		"\n" +
		"10.0.0.2 nas\n" +
		"  192.0.2.10\t\tBuild.Example.   # trailing comment\n" +
		"127.0.0.1 localhost\n" +
		"127.0.0.2 a.localhost\n" +
		"10.0.0.3 NAS\n" +
		"10.0.0.4 one two\n" +
		"10.0.0.256 bad\n" +
		"010.0.0.1 octal\n" +
		"0.0.0.0 zero\n" +
		"224.0.0.1 multicast\n" +
		"10.0.0.5\n" +
		"10.0.0.6 -bad\n" +
		"   \t  \n" +
		"#10.0.0.7 commented\n" +
		"10.0.0.8 last"
	var hosts Hosts
	skipped := ParseHosts([]byte(file), &hosts)
	if skipped != 10 {
		t.Fatalf("skipped %d", skipped)
	}
	if got := string(AppendHosts(nil, &hosts)); got != "10.0.0.2 nas\n192.0.2.10 build.example\n10.0.0.8 last\n" {
		t.Fatalf("table %q", got)
	}
	if hosts.Generation == 0 {
		t.Fatal("loading did not change the generation")
	}
	// Loading replaces what was there.
	if ParseHosts(nil, &hosts) != 0 || hosts.Count() != 0 {
		t.Fatal("empty file")
	}
}

func TestParseHostsFullTableAndBound(t *testing.T) {
	var file bytes.Buffer
	for index := 0; index < MaxHosts+3; index++ {
		fmt.Fprintf(&file, "10.0.0.1 host%d\n", index)
	}
	var hosts Hosts
	if skipped := ParseHosts(file.Bytes(), &hosts); skipped != 3 || hosts.Count() != MaxHosts {
		t.Fatalf("skipped %d count %d", skipped, hosts.Count())
	}
	// A file past the bound is cut there, and the line the cut splits is
	// skipped rather than read as a shorter name.
	file.Reset()
	file.WriteString("10.0.0.1 first\n")
	for file.Len() < HostsFileSize-10 {
		file.WriteString("# padding\n")
	}
	file.WriteString("10.0.0.2 split-name\n")
	if skipped := ParseHosts(file.Bytes(), &hosts); skipped != 1 || hosts.Count() != 1 {
		t.Fatalf("bound: skipped %d count %d", skipped, hosts.Count())
	}
}

func TestHostsFileRoundTrip(t *testing.T) {
	var hosts Hosts
	long := strings.Repeat(strings.Repeat("z", 61)+".", 4)
	for index := 0; index < MaxHosts; index++ {
		name := long[:MaxHostName-2] + string(rune('a'+index/26)) + string(rune('a'+index%26))
		if hosts.Set(mustName(t, name), [4]byte{223, 255, 255, byte(index)}) != StatusOK {
			t.Fatal(index)
		}
	}
	data := AppendHosts(nil, &hosts)
	if len(data) > HostsFileSize {
		t.Fatalf("a full table formats to %d bytes, over the bound", len(data))
	}
	var loaded Hosts
	if skipped := ParseHosts(data, &loaded); skipped != 0 || !bytes.Equal(AppendHosts(nil, &loaded), data) {
		t.Fatalf("round trip skipped %d", skipped)
	}
}

func FuzzParseHosts(f *testing.F) {
	f.Add([]byte("10.0.0.2 nas\n# c\n\n192.0.2.1\tA.b. #x\n"))
	f.Add([]byte("127.0.0.1 localhost\n10.0.0.1 a\n10.0.0.1 A\n"))
	f.Add([]byte("1.2.3.4 a b\n0.0.0.0 x\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var hosts Hosts
		ParseHosts(data, &hosts)
		if hosts.Count() > MaxHosts {
			t.Fatal("over MaxHosts")
		}
		for index := 0; index < hosts.Count(); index++ {
			record := &hosts.records[index]
			if record.Name.Size == 0 || record.Name.Size > MaxHostName || record.Name.IsLocalhost() || !ValidHostAddress(record.Address) {
				t.Fatalf("invalid record %q %v", record.Name.Bytes(), record.Address)
			}
			if normalized, ok := hostname.Parse(record.Name.Bytes()); !ok || !normalized.Equal(&record.Name) {
				t.Fatalf("record not normalized: %q", record.Name.Bytes())
			}
			for other := 0; other < index; other++ {
				if hosts.records[other].Name.Equal(&record.Name) {
					t.Fatalf("duplicate %q", record.Name.Bytes())
				}
			}
		}
		// What is written reads back as itself.
		formatted := AppendHosts(nil, &hosts)
		var again Hosts
		if skipped := ParseHosts(formatted, &again); skipped != 0 || !bytes.Equal(AppendHosts(nil, &again), formatted) {
			t.Fatalf("round trip of %q skipped %d", formatted, skipped)
		}
	})
}
