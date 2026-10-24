package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testdata/objdump.txt is `go tool objdump` of a goprobe ELF, and
// testdata/guest.log the region lines a TCG run of that ELF printed, with the
// per-direction offsets the guest now also prints.

func fixture(t *testing.T) (map[string][]uint64, string) {
	t.Helper()
	objdump, err := os.Open("testdata/objdump.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer objdump.Close()
	want, err := boundaries(objdump)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile("testdata/guest.log")
	if err != nil {
		t.Fatal(err)
	}
	return want, string(log)
}

func TestBoundariesCountReachableInstructions(t *testing.T) {
	want, _ := fixture(t)
	// tools/stackframes pins both sequences: fifteen instructions in the
	// switch, 48 in the entry, whose UD2 only a failed take reaches.
	if len(want["goos_g_swap"]) != 15 || len(want["goos_preempt_entry"]) != 47 {
		t.Fatalf("boundaries: swap %d, preempt %d", len(want["goos_g_swap"]), len(want["goos_preempt_entry"]))
	}
	if want["goos_g_swap"][0] != 0 || want["goos_preempt_entry"][0] != 0 {
		t.Fatal("a range does not start at its first instruction")
	}
}

func TestCheckAcceptsTheRecordedRun(t *testing.T) {
	want, log := fixture(t)
	summary, err := check(strings.NewReader(log), want)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "swaphold: boundaries held preempt=47/47 swap=15/15" {
		t.Fatalf("summary %q", summary)
	}
}

func TestCheckRejectsAMissedOrForeignBoundary(t *testing.T) {
	want, log := fixture(t)
	for name, mutate := range map[string]func(string) string{
		"missed":       func(s string) string { return strings.Replace(s, "offsets=0,2,4,", "offsets=0,4,", 1) },
		"mid-insn":     func(s string) string { return strings.Replace(s, "offsets=0,2,4,", "offsets=0,1,4,", 1) },
		"count":        func(s string) string { return strings.Replace(s, "distinct_rips=15", "distinct_rips=14", 1) },
		"lost":         func(s string) string { return strings.Replace(s, "lost=0", "lost=1", 1) },
		"no preempt":   func(s string) string { return strings.Split(s, "\n")[0] + "\n" },
		"empty offset": func(s string) string { return strings.Replace(s, "offsets=0,2,4,", "offsets=,", 1) },
		"direction":    func(s string) string { return strings.Replace(s, "offsets_in=0,2,4,", "offsets_in=0,4,", 1) },
		"no direction": func(s string) string { return strings.Replace(s, " offsets_out=", " other=", 1) },
	} {
		if _, err := check(strings.NewReader(mutate(log)), want); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestBuiltFixture checks the goprobe ELF a validation build left, when
// there is one.
func TestBuiltFixture(t *testing.T) {
	elf := os.Getenv("GOPROBE_ELF")
	if elf == "" {
		t.Skip("GOPROBE_ELF not set")
	}
	output, err := exec.Command("go", "tool", "objdump", "-s", "^(goos_g_swap|goos_preempt_entry)$", elf).Output()
	if err != nil {
		t.Fatal(err)
	}
	want, err := boundaries(strings.NewReader(string(output)))
	if err != nil {
		t.Fatal(err)
	}
	if len(want["goos_g_swap"]) != 15 || len(want["goos_preempt_entry"]) != 47 {
		t.Fatalf("boundaries: swap %d, preempt %d", len(want["goos_g_swap"]), len(want["goos_preempt_entry"]))
	}
}
