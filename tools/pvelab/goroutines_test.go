package main

import (
	"strings"
	"testing"
)

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

// native-concurrency delegates its runtime checks to goroutinesScenario.
func TestGoroutineChecksIncludeCollectorStress(t *testing.T) {
	for _, command := range []string{"goprobe gcstress 1", "goprobe gcstress 8"} {
		found := false
		for _, check := range goroutineChecks {
			if check.command == command {
				found = check.marker == "GOPROBE_GCSTRESS_OK" && len(check.records) == 1 && check.records[0] == "GOPROBE_GCSTRESS"
			}
		}
		if !found {
			t.Errorf("missing collector stress check: %s", command)
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

func TestRegRootsRecordsRequireEveryClassRegisterAndSituation(t *testing.T) {
	records := []string{
		"GOPROBE_REGROOTS class=xmm situation=thread regs=16 collections=80 lost=0 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=xmm situation=goroutine regs=16 collections=128 lost=0 running=64 switched=64 permits=3 " +
			"running_visits=90 running_preemptions=0 entry_regs=16 switched_preemptions=64 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=gpr situation=thread regs=15 collections=76 lost=0 controls=4 reclaimed=4",
		"GOPROBE_REGROOTS class=gpr situation=goroutine regs=15 collections=120 lost=0 running=60 switched=60 permits=3 " +
			"running_visits=80 running_preemptions=0 entry_regs=15 switched_preemptions=60 controls=4 reclaimed=4",
	}
	good := strings.Join(records, "\r\n") + "\r\nGOPROBE_REGROOTS_OK\r\n"
	if err := regRootsRecords(good); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"",
		records[0] + "\n" + records[1],
		strings.Join(records[:3], "\n"),
		good + records[0] + "\n",
		strings.Replace(good, "regs=16", "regs=15", 1),
		strings.Replace(good, "regs=15", "regs=16", 1),
		strings.Replace(good, "lost=0", "lost=1", 1),
		strings.Replace(good, "collections=76", "collections=59", 1),
		strings.Replace(good, "reclaimed=4", "reclaimed=3", 1),
		strings.Replace(good, "switched=60", "switched=10", 1),
		strings.Replace(good, "switched_preemptions=64", "switched_preemptions=0", 1),
		strings.Replace(good, "entry_regs=15", "entry_regs=0", 1),
		strings.Replace(good, "regs=16", "regs=x", 1),
		strings.Replace(good, "class=gpr situation=thread", "class=other situation=thread", 1),
		good + "GOPROBE_REGROOTS_LOST class=gpr situation=thread reg=7 collections=4 entered=0 kept=1 bad_cell=1\n",
		good + "GOPROBE_REGROOTS_KEPT class=xmm situation=thread collections=4\n",
	} {
		if err := regRootsRecords(output); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}

func TestGoroutineChecksIncludeRegRoots(t *testing.T) {
	for _, check := range goroutineChecks {
		if check.command == "goprobe regroots" && check.marker == "GOPROBE_REGROOTS_OK" && goroutineVerifiers[check.command] != nil {
			return
		}
	}
	t.Fatal("missing goprobe regroots check")
}
